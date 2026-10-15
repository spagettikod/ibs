package site

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"ibs/internal/blockcodec"
	"ibs/internal/blockstore"
	"ibs/internal/db"
	"ibs/internal/keys"
)

const (
	gcGrace        = 10 * time.Minute // unreferenced blocks younger than this are kept
	replicateBatch = 256
)

type peerCandidate struct {
	siteID string
	free   int64
}

type replNeed struct {
	id      string
	missing int
}

// replicationNeeds returns the next batch of blocks that need more copies and
// that some online site could take.
func (s *Site) replicationNeeds(ctx context.Context) ([]replNeed, error) {
	countedSince := time.Now().Add(-s.cfg.PeerGrace).Unix()
	rows, err := s.DB.QueryContext(ctx, `SELECT id, replication, n FROM (
			SELECT b.id, bk.replication,
				(SELECT COUNT(*) FROM replicas r JOIN peers p ON p.site_id = r.site_id
					WHERE r.block_id = b.id AND r.state = 'ok' AND p.last_seen >= ?) AS n
			FROM blocks b JOIN buckets bk ON bk.name = b.bucket
			WHERE b.refs > 0 AND bk.replication > 0
				-- Only blocks some online site could take: the others (e.g. with fewer
				-- sites than copies) would otherwise fill every batch and stall the rest.
				AND EXISTS (SELECT 1 FROM peers p WHERE p.online = 1
					AND NOT EXISTS (SELECT 1 FROM replicas r WHERE r.block_id = b.id AND r.site_id = p.site_id))
				AND b.id > ?
			ORDER BY b.id)
		WHERE n < replication LIMIT ?`, countedSince, s.replCursor, replicateBatch)
	if err != nil {
		return nil, err
	}
	var needs []replNeed
	for rows.Next() {
		var id string
		var repl, n int
		rows.Scan(&id, &repl, &n)
		needs = append(needs, replNeed{id, repl - n})
	}
	rows.Close()
	// Each run goes on after the last block of the one before, so blocks that
	// can't be copied now (all candidate sites full) don't hold up the rest.
	if len(needs) == replicateBatch {
		s.replCursor = needs[len(needs)-1].id
	} else {
		s.replCursor = ""
	}
	return needs, nil
}

// replicateOnce brings blocks up to their bucket's replication count by
// copying them to online peers with the most free space. Replicas on peers
// that have been unreachable longer than the grace period do not count, so
// their blocks are re-replicated elsewhere. Blocks every online site already
// holds wait for another site to come online.
func (s *Site) replicateOnce(ctx context.Context) {
	needs, err := s.replicationNeeds(ctx)
	if err != nil {
		s.log.Warn("replication query", "err", err)
		return
	}
	if len(needs) == 0 {
		s.noRoom.Store(s.replNoRoom)
		s.replNoRoom = 0
		return
	}

	peers := s.onlinePeers()
	if len(peers) == 0 {
		return
	}
	full := map[string]bool{}
	copied, noRoom := 0, int64(0)
	for _, nd := range needs {
		if ctx.Err() != nil {
			return
		}
		have := map[string]bool{}
		hr, err := s.DB.Query(`SELECT site_id FROM replicas WHERE block_id = ?`, nd.id)
		if err != nil {
			return
		}
		for hr.Next() {
			var sid string
			hr.Scan(&sid)
			have[sid] = true
		}
		hr.Close()
		var targets []string
		anyFull := false
		for _, p := range peers {
			if len(targets) == nd.missing {
				break
			}
			if !have[p.siteID] && full[p.siteID] {
				anyFull = true
			}
			if !have[p.siteID] && !full[p.siteID] {
				targets = append(targets, p.siteID)
			}
		}
		if len(targets) == 0 {
			if anyFull {
				noRoom++
			}
			continue
		}
		ct, err := s.readBlock(ctx, nd.id)
		if err != nil {
			s.log.Warn("cannot read block for replication", "block", nd.id[:12], "err", err)
			continue
		}
		placed, refused := 0, 0
		for _, sid := range targets {
			err := s.peerPutBlock(ctx, sid, nd.id, ct)
			if isStatus(err, http.StatusInsufficientStorage) {
				full[sid] = true
				refused++
				continue
			}
			if err != nil {
				s.log.Warn("replicate block", "block", nd.id[:12], "peer", sid[:8], "err", err)
				continue
			}
			s.DB.Exec(`INSERT INTO replicas(block_id, site_id, state, verified) VALUES(?, ?, 'ok', ?)
				ON CONFLICT(block_id, site_id) DO UPDATE SET state = 'ok', verified = excluded.verified`,
				nd.id, sid, time.Now().Unix())
			copied++
			placed++
		}
		if refused > 0 && placed < nd.missing {
			noRoom++
		}
	}
	// Counted over a whole pass through the blocks, not one batch.
	s.replNoRoom += noRoom
	if s.replCursor == "" {
		s.noRoom.Store(s.replNoRoom)
		s.replNoRoom = 0
	}
	if copied > 0 {
		s.notify(TopicReplication)
		s.log.Info("replicated blocks", "copies", copied)
	}
}

// trimOnce removes surplus replicas, e.g. after a long-offline peer returns
// with copies that were meanwhile re-created elsewhere, or after a bucket's
// replication was lowered. Only replicas on online peers count here, so a
// copy is never dropped in favour of one that cannot be confirmed. Copies on
// the fullest peers go first; the garbage collector sends the deletes.
func (s *Site) trimOnce(ctx context.Context) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, replication, n FROM (
			SELECT b.id, bk.replication,
				(SELECT COUNT(*) FROM replicas r JOIN peers p ON p.site_id = r.site_id
					WHERE r.block_id = b.id AND r.state = 'ok' AND p.online = 1) AS n
			FROM blocks b JOIN buckets bk ON bk.name = b.bucket
			WHERE b.refs > 0)
		WHERE n > replication LIMIT ?`, replicateBatch)
	if err != nil {
		s.log.Warn("trim query", "err", err)
		return
	}
	type surplus struct {
		id    string
		extra int
	}
	var list []surplus
	for rows.Next() {
		var id string
		var repl, n int
		rows.Scan(&id, &repl, &n)
		list = append(list, surplus{id, n - repl})
	}
	rows.Close()
	trimmed := 0
	for _, sp := range list {
		res, err := s.DB.ExecContext(ctx, `UPDATE replicas SET state = 'delete'
			WHERE block_id = ? AND site_id IN (
				SELECT r.site_id FROM replicas r JOIN peers p ON p.site_id = r.site_id
				WHERE r.block_id = ? AND r.state = 'ok' AND p.online = 1
				ORDER BY p.free ASC, r.site_id LIMIT ?)`, sp.id, sp.id, sp.extra)
		if err != nil {
			s.log.Warn("trim replicas", "block", sp.id[:12], "err", err)
			continue
		}
		n, _ := res.RowsAffected()
		trimmed += int(n)
	}
	if trimmed > 0 {
		s.notify(TopicReplication)
		s.log.Info("trimmed surplus replicas", "copies", trimmed)
	}
}

// onlinePeers returns reachable peers, most free space first.
func (s *Site) onlinePeers() []peerCandidate {
	rows, err := s.DB.Query(`SELECT site_id, free FROM peers WHERE online = 1 ORDER BY free DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []peerCandidate
	for rows.Next() {
		var p peerCandidate
		rows.Scan(&p.siteID, &p.free)
		out = append(out, p)
	}
	return out
}

// collectGarbage deletes blocks no object or part references any more,
// locally and on peers.
func (s *Site) collectGarbage(ctx context.Context) {
	cutoff := time.Now().Add(-gcGrace).Unix()
	rows, err := s.DB.Query(`SELECT id FROM blocks WHERE refs <= 0 AND touched < ? LIMIT 500`, cutoff)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		s.deleteBlock(ctx, id, cutoff)
	}
	if len(ids) > 0 {
		s.notify(TopicReplication)
		s.log.Info("garbage collected blocks", "count", len(ids))
	}

	// Propagate pending replica deletions to reachable peers.
	rows, err = s.DB.Query(`SELECT r.site_id, r.block_id FROM replicas r JOIN peers p ON p.site_id = r.site_id
		WHERE r.state = 'delete' AND p.online = 1 LIMIT 2000`)
	if err != nil {
		return
	}
	bySite := map[string][]string{}
	for rows.Next() {
		var sid, id string
		rows.Scan(&sid, &id)
		bySite[sid] = append(bySite[sid], id)
	}
	rows.Close()
	for sid, ids := range bySite {
		if err := s.peerDeleteBlocks(ctx, sid, ids); err != nil {
			s.log.Warn("delete replicas on peer", "peer", sid[:8], "err", err)
			continue
		}
		for _, id := range ids {
			s.DB.Exec(`DELETE FROM replicas WHERE site_id = ? AND block_id = ? AND state = 'delete'`, sid, id)
		}
		s.notify(TopicReplication)
	}
}

func (s *Site) deleteBlock(ctx context.Context, id string, cutoff int64) {
	unlock := s.lockBlock(id)
	defer unlock()
	var nodeID string
	tx, err := s.DB.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	err = tx.QueryRow(`DELETE FROM blocks WHERE id = ? AND refs <= 0 AND touched < ? RETURNING node_id`, id, cutoff).Scan(&nodeID)
	if err != nil {
		return // referenced again or already gone
	}
	if _, err := tx.Exec(`UPDATE replicas SET state = 'delete' WHERE block_id = ?`, id); err != nil {
		return
	}
	if tx.Commit() != nil {
		return
	}
	if st := s.nodeStore(nodeID); st != nil {
		if err := st.Delete(ctx, blockstore.OwnNS, id); err != nil {
			s.log.Warn("delete local block", "block", id[:12], "err", err)
		}
	}
}

// lostNodesSQL selects storage nodes offline for longer than the grace
// period. Their blocks are restored elsewhere at this site from the IBS.
const lostNodesSQL = `SELECT id FROM nodes WHERE is_local = 0 AND last_seen < ?`

// healOnce restores local copies of blocks that are missing at this site:
// after a restore, a removed node, or a node offline longer than the grace
// period. Copies come from replicas on other sites and go to nodes with
// room. Blocks that exist nowhere else stay assigned to their lost node, so
// they come back with it. Blocks held for other sites on a lost node are
// dropped; their owners re-replicate them.
func (s *Site) healOnce(ctx context.Context) {
	lostCutoff := time.Now().Add(-s.cfg.PeerGrace).Unix()
	s.DB.Exec(`UPDATE blocks SET node_id = '' WHERE node_id != '' AND node_id NOT IN (SELECT id FROM nodes)`)
	if res, err := s.DB.Exec(`DELETE FROM held_blocks WHERE node_id IN (`+lostNodesSQL+`)`, lostCutoff); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			s.log.Warn("dropped blocks held for other sites on lost nodes", "count", n)
			s.notify(TopicReplication)
		}
	}
	rows, err := s.DB.Query(`SELECT b.id, b.node_id FROM blocks b
		WHERE b.refs > 0 AND (b.node_id = '' OR b.node_id IN (`+lostNodesSQL+`))
			AND EXISTS (SELECT 1 FROM replicas r JOIN peers p ON p.site_id = r.site_id
				WHERE r.block_id = b.id AND r.state = 'ok' AND p.online = 1)
		LIMIT 64`, lostCutoff)
	if err != nil {
		return
	}
	type missing struct{ id, from string }
	var list []missing
	for rows.Next() {
		var m missing
		rows.Scan(&m.id, &m.from)
		list = append(list, m)
	}
	rows.Close()
	healed := 0
	for _, m := range list {
		if ctx.Err() != nil {
			return
		}
		ct, _, err := s.fetchFromReplicas(ctx, m.id)
		if err != nil {
			continue
		}
		if err := s.healBlock(ctx, m.id, ct, m.from); err == nil {
			healed++
		} else if errors.Is(err, ErrNoSpace) {
			s.log.Warn("no room on any node to restore blocks; they are served from replicas")
			break
		}
	}
	if healed > 0 {
		s.log.Info("restored blocks from replicas", "count", healed)
	}
}

// reconcileNode removes copies on a storage node that are no longer
// assigned to it, e.g. blocks restored elsewhere while the node was lost,
// or leftovers of interrupted deletions. Runs when a node (re)appears.
func (s *Site) reconcileNode(ctx context.Context, nodeID string) {
	store := s.nodeStore(nodeID)
	if store == nil {
		return
	}
	nss, err := store.Namespaces(ctx)
	if err != nil {
		s.log.Warn("node reconcile: list namespaces", "node", nodeID, "err", err)
		return
	}
	removed := 0
	for _, ns := range nss {
		ids, err := store.List(ctx, ns)
		if err != nil {
			s.log.Warn("node reconcile: list blocks", "node", nodeID, "err", err)
			return
		}
		for _, id := range ids {
			unlock := s.lockBlock(id)
			var n int
			if ns == blockstore.OwnNS {
				s.DB.QueryRow(`SELECT COUNT(*) FROM blocks WHERE id = ? AND node_id = ?`, id, nodeID).Scan(&n)
			} else {
				s.DB.QueryRow(`SELECT COUNT(*) FROM held_blocks WHERE owner = ? AND id = ? AND node_id = ?`, ns, id, nodeID).Scan(&n)
			}
			if n == 0 && store.Delete(ctx, ns, id) == nil {
				removed++
			}
			unlock()
		}
	}
	if removed > 0 {
		s.log.Info("removed stale copies from node", "node", nodeID, "count", removed)
		s.notify(TopicNodes)
	}
}

func (s *Site) reconcileAll(ctx context.Context) {
	for _, p := range s.onlinePeers() {
		s.reconcilePeer(ctx, p.siteID)
	}
}

// reconcilePeer compares what a peer actually holds for us with what our
// metadata says, after a reconnect or periodically:
//   - replicas the peer lost are forgotten so they get re-replicated,
//   - blocks the peer holds that we still need are recorded as replicas,
//   - blocks the peer holds that we no longer need are deleted there,
//   - the peer gets our latest metadata backup.
func (s *Site) reconcilePeer(ctx context.Context, siteID string) {
	s.peerMu.Lock()
	if s.reconciling[siteID] {
		s.peerMu.Unlock()
		return
	}
	s.reconciling[siteID] = true
	s.peerMu.Unlock()
	defer func() {
		s.peerMu.Lock()
		delete(s.reconciling, siteID)
		s.peerMu.Unlock()
	}()

	var inv []InventoryItem
	if _, err := s.peerCall(ctx, siteID, http.MethodGet, "/peer/v1/inventory", nil, &inv); err != nil {
		s.log.Warn("reconcile: fetch inventory", "peer", siteID[:8], "err", err)
		return
	}
	held := make(map[string]bool, len(inv))
	for _, it := range inv {
		held[it.ID] = true
	}

	now := time.Now().Unix()
	var lost, confirmed int
	var orphans []string
	err := s.inTx(func(tx2 *sql.Tx) error {
		rows, err := tx2.Query(`SELECT block_id, state FROM replicas WHERE site_id = ?`, siteID)
		if err != nil {
			return err
		}
		recorded := map[string]string{}
		for rows.Next() {
			var id, st string
			rows.Scan(&id, &st)
			recorded[id] = st
		}
		rows.Close()
		for id, st := range recorded {
			if !held[id] {
				tx2.Exec(`DELETE FROM replicas WHERE block_id = ? AND site_id = ?`, id, siteID)
				if st == "ok" {
					lost++
				}
			}
		}
		for _, it := range inv {
			var refs int
			err := tx2.QueryRow(`SELECT refs FROM blocks WHERE id = ?`, it.ID).Scan(&refs)
			if err != nil || refs <= 0 || recorded[it.ID] == "delete" {
				orphans = append(orphans, it.ID)
				continue
			}
			tx2.Exec(`INSERT INTO replicas(block_id, site_id, state, verified) VALUES(?, ?, 'ok', ?)
				ON CONFLICT(block_id, site_id) DO UPDATE SET state = 'ok', verified = excluded.verified`, it.ID, siteID, now)
			confirmed++
		}
		_, err = tx2.Exec(`UPDATE peers SET reconciled = ? WHERE site_id = ?`, now, siteID)
		return err
	})
	if err != nil {
		s.log.Warn("reconcile", "peer", siteID[:8], "err", err)
		return
	}
	for start := 0; start < len(orphans); start += 1000 {
		batch := orphans[start:min(start+1000, len(orphans))]
		if err := s.peerDeleteBlocks(ctx, siteID, batch); err != nil {
			s.log.Warn("reconcile: delete orphans", "peer", siteID[:8], "err", err)
			break
		}
		for _, id := range batch {
			s.DB.Exec(`DELETE FROM replicas WHERE block_id = ? AND site_id = ?`, id, siteID)
		}
	}
	s.notify(TopicReplication, TopicPeers)
	s.log.Info("reconciled with peer", "peer", siteID[:8], "confirmed", confirmed, "lost", lost, "orphans", len(orphans))

	// Make sure the peer has our latest metadata backup.
	var theirs int64 = -1
	if hdr, err := s.peerCall(ctx, siteID, http.MethodHead, "/peer/v1/backup", nil, nil); err == nil {
		theirs = parseInt(hdr.Get("X-IBS-Generation"))
	}
	if gen := s.SettingInt("generation"); theirs < gen {
		s.pushBackup(ctx, siteID)
	} else {
		s.peerMu.Lock()
		s.pushedGen[siteID] = gen
		s.peerMu.Unlock()
	}
}

// ---- Metadata backup ----

func backupAAD(siteID string) []byte { return []byte("ibs-backup:" + siteID) }

// snapshot produces an encrypted copy of the metadata database.
func (s *Site) snapshot(ctx context.Context) ([]byte, error) {
	dir := filepath.Join(s.cfg.DataDir, "tmp")
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, "snapshot-"+keys.RandomToken(6)+".db")
	defer os.Remove(p)
	if _, err := s.DB.ExecContext(ctx, `VACUUM INTO ?`, p); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return keys.Seal(s.ID.Master, raw, backupAAD(s.ID.SiteID)), nil
}

// backupOnce bumps the metadata generation after changes and pushes the
// encrypted snapshot to every online peer that does not have it yet.
//
// The same snapshot also goes to every online storage node of this site, so
// the site can be restored from its own nodes (even without any peers).
func (s *Site) backupOnce(ctx context.Context) {
	if s.dirty.Swap(false) {
		gen := s.SettingInt("generation") + 1
		db.SetSetting(s.DB, "generation", strconv.FormatInt(gen, 10))
	}
	gen := s.SettingInt("generation")
	var blob []byte
	snapshot := func() []byte {
		if blob == nil {
			var err error
			if blob, err = s.snapshot(ctx); err != nil {
				s.log.Warn("snapshot metadata", "err", err)
			}
		}
		return blob
	}
	for _, p := range s.onlinePeers() {
		s.peerMu.Lock()
		pushed, ok := s.pushedGen[p.siteID]
		s.peerMu.Unlock()
		if ok && pushed >= gen {
			continue
		}
		if b := snapshot(); b != nil {
			s.pushBackupBlob(ctx, p.siteID, gen, b)
		}
	}
	for _, id := range s.onlineRemoteNodes() {
		s.peerMu.Lock()
		pushed, ok := s.nodeMetaGen[id]
		s.peerMu.Unlock()
		if ok && pushed >= gen {
			continue
		}
		r, isRemote := s.nodeStore(id).(*blockstore.Remote)
		b := snapshot()
		if !isRemote || b == nil {
			continue
		}
		if err := r.PutMetadata(ctx, gen, b); err != nil {
			s.log.Warn("push metadata to node", "node", id, "err", err)
			continue
		}
		s.peerMu.Lock()
		s.nodeMetaGen[id] = gen
		s.peerMu.Unlock()
		s.notify(TopicNodes)
	}
}

func (s *Site) onlineRemoteNodes() []string {
	rows, err := s.DB.Query(`SELECT id FROM nodes WHERE is_local = 0 AND last_seen >= ?`, time.Now().Add(-nodeTimeout).Unix())
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func (s *Site) pushBackup(ctx context.Context, siteID string) {
	blob, err := s.snapshot(ctx)
	if err != nil {
		s.log.Warn("snapshot metadata", "err", err)
		return
	}
	s.pushBackupBlob(ctx, siteID, s.SettingInt("generation"), blob)
}

func (s *Site) pushBackupBlob(ctx context.Context, siteID string, gen int64, blob []byte) {
	_, err := s.peerCall(ctx, siteID, http.MethodPut, "/peer/v1/backup?generation="+strconv.FormatInt(gen, 10), blob, nil)
	if err != nil && !isStatus(err, http.StatusConflict) {
		s.log.Warn("push metadata backup", "peer", siteID[:8], "err", err)
		return
	}
	s.peerMu.Lock()
	s.pushedGen[siteID] = gen
	s.peerMu.Unlock()
}

// ---- Health summary ----

type ReplicationHealth struct {
	Blocks          int64 `json:"blocks"`
	Bytes           int64 `json:"bytes"`
	UnderReplicated int64 `json:"under_replicated"`
	OverReplicated  int64 `json:"over_replicated"`
	PendingBytes    int64 `json:"pending_bytes"` // replica bytes still to be copied
	MissingLocally  int64 `json:"missing_locally"`
	PendingDeletes  int64 `json:"pending_deletes"`
	// NoRoom: blocks that need a copy no site has room for, at the last try.
	NoRoom int64 `json:"no_room"`
}

func (s *Site) ReplicationHealth() ReplicationHealth {
	h := ReplicationHealth{NoRoom: s.noRoom.Load()}
	countedSince := time.Now().Add(-s.cfg.PeerGrace).Unix()
	s.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM blocks WHERE refs > 0`).Scan(&h.Blocks, &h.Bytes)
	s.DB.QueryRow(`SELECT COUNT(*) FROM blocks WHERE refs > 0 AND node_id = ''`).Scan(&h.MissingLocally)
	s.DB.QueryRow(`SELECT COUNT(*) FROM replicas WHERE state = 'delete'`).Scan(&h.PendingDeletes)
	s.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM((replication - n) * size), 0) FROM (
			SELECT bk.replication, b.size,
				(SELECT COUNT(*) FROM replicas r JOIN peers p ON p.site_id = r.site_id
					WHERE r.block_id = b.id AND r.state = 'ok' AND p.last_seen >= ?) AS n
			FROM blocks b JOIN buckets bk ON bk.name = b.bucket WHERE b.refs > 0)
		WHERE n < replication`, countedSince).Scan(&h.UnderReplicated, &h.PendingBytes)
	s.DB.QueryRow(`SELECT COUNT(*) FROM (
			SELECT bk.replication,
				(SELECT COUNT(*) FROM replicas r JOIN peers p ON p.site_id = r.site_id
					WHERE r.block_id = b.id AND r.state = 'ok' AND p.online = 1) AS n
			FROM blocks b JOIN buckets bk ON bk.name = b.bucket WHERE b.refs > 0)
		WHERE n > replication`).Scan(&h.OverReplicated)
	return h
}

// drainOnce moves blocks off online nodes that share nothing
// (IBS_STORAGE_LIMIT=0), e.g. a manager that should leave all data to its
// storage nodes: this site's blocks and blocks held for other sites go to
// nodes with room. A block is only deleted from the old node after its new
// copy is recorded. Damaged copies are left to the healer.
func (s *Site) drainOnce(ctx context.Context) {
	online := time.Now().Add(-nodeTimeout).Unix()
	type item struct{ owner, id, node string } // owner "" = this site's block
	var items []item
	for _, q := range []string{
		`SELECT '', id, node_id FROM blocks WHERE node_id IN (SELECT id FROM nodes WHERE quota < 0 AND last_seen >= ?) LIMIT 64`,
		`SELECT owner, id, node_id FROM held_blocks WHERE node_id IN (SELECT id FROM nodes WHERE quota < 0 AND last_seen >= ?) LIMIT 64`,
	} {
		rows, err := s.DB.Query(q, online)
		if err != nil {
			return
		}
		for rows.Next() {
			var it item
			rows.Scan(&it.owner, &it.id, &it.node)
			items = append(items, it)
		}
		rows.Close()
	}
	moved := 0
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		err := s.moveBlock(ctx, it.owner, it.id, it.node)
		if errors.Is(err, ErrNoSpace) {
			s.log.Warn("cannot move blocks off a node that shares nothing: no other node has room")
			break
		}
		if err == nil {
			moved++
		}
	}
	if moved > 0 {
		s.log.Info("moved blocks off nodes that share nothing", "count", moved)
		s.notify(TopicNodes)
	}
}

func (s *Site) moveBlock(ctx context.Context, owner, id, from string) error {
	unlock := s.lockBlock(id)
	defer unlock()
	ns := blockstore.OwnNS
	if owner != "" {
		ns = owner
	}
	// Still on the node to drain?
	var n int
	if owner == "" {
		s.DB.QueryRow(`SELECT COUNT(*) FROM blocks WHERE id = ? AND node_id = ?`, id, from).Scan(&n)
	} else {
		s.DB.QueryRow(`SELECT COUNT(*) FROM held_blocks WHERE owner = ? AND id = ? AND node_id = ?`, owner, id, from).Scan(&n)
	}
	src := s.nodeStore(from)
	if n == 0 || src == nil {
		return nil
	}
	data, err := src.Get(ctx, ns, id)
	if err != nil && !errors.Is(err, blockstore.ErrNotFound) {
		return err // node unreachable; try later
	}
	if err != nil || !blockcodec.Verify(id, data) {
		// Missing or damaged: hand own blocks to the healer, drop held ones
		// (their owner re-replicates).
		if owner == "" {
			s.DB.Exec(`UPDATE blocks SET node_id = '' WHERE id = ? AND node_id = ?`, id, from)
		} else {
			s.DB.Exec(`DELETE FROM held_blocks WHERE owner = ? AND id = ? AND node_id = ?`, owner, id, from)
		}
		return nil
	}
	to, err := s.putOnNode(ctx, ns, id, data)
	if err != nil {
		return err
	}
	if owner == "" {
		_, err = s.DB.Exec(`UPDATE blocks SET node_id = ? WHERE id = ? AND node_id = ?`, to, id, from)
	} else {
		_, err = s.DB.Exec(`UPDATE held_blocks SET node_id = ? WHERE owner = ? AND id = ? AND node_id = ?`, to, owner, id, from)
	}
	if err != nil {
		return err
	}
	return src.Delete(ctx, ns, id)
}
