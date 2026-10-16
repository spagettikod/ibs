package site

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"ibs/internal/blockcodec"
)

// Room for a site's own files.
//
// Other sites' copies may use any free space on a site. When the site's own
// upload finds no room, it makes room by giving back copies it holds for
// other sites (makeRoom):
//
//  1. It asks the owners, only sites that are online, to release some of
//     their copies (POST /peer/v1/blocks/release).
//  2. The owner first tries to place a replacement copy on another site. It
//     releases a copy when the replacement worked, or when the block still
//     has its original and at least one other copy without it; otherwise it
//     keeps it.
//  3. The site deletes the released copies and stores its own block.
//
// Owners re-create the missing copies when there is room again. For an hour
// after making room a site takes no new copies, so they don't flow straight
// back into the space its own files need (IBS_ROOM_COOLDOWN).

const (
	roomBatch       = 16 * blockcodec.BlockSize // freed at least, so not every block of an upload asks again
	replaceDeadline = 10 * time.Second          // the owner answers well within peerTimeout
)

// heldMargin is space kept free for this site's own files: other sites'
// copies don't take it, so an upload always has room to start.
func heldMargin(total int64) int64 { return min(total/20, 1<<30) }

// offeredStorage is what this site offers other sites for their copies: all
// of its storage, and the free space minus a small margin (none for an hour
// after it had to make room for its own files).
func (s *Site) offeredStorage() (capacity, free int64) {
	total, diskFree := s.siteCapacity()
	if time.Since(time.Unix(s.SettingInt("room_made_at"), 0)) < s.cfg.RoomCooldown {
		return total, 0
	}
	return total, max(diskFree-heldMargin(total), 0)
}

// putOwn stores one of this site's blocks on a node, making room by giving
// back other sites' copies when the storage is full.
func (s *Site) putOwn(ctx context.Context, ns, id string, data []byte) (string, error) {
	nodeID, err := s.putOnNode(ctx, ns, id, data)
	if !errors.Is(err, ErrNoSpace) {
		return nodeID, err
	}
	if !s.makeRoom(ctx, int64(len(data))) {
		return "", ErrNoSpace
	}
	return s.putOnNode(ctx, ns, id, data)
}

// makeRoom frees at least need bytes by giving back copies held for online
// sites that agree to release them. It reports whether anything was freed.
func (s *Site) makeRoom(ctx context.Context, need int64) bool {
	s.roomMu.Lock()
	defer s.roomMu.Unlock()
	if _, _, err := s.pickNode(need); err == nil {
		return true // another upload made room meanwhile
	}
	target := max(need, roomBatch)
	rows, err := s.DB.QueryContext(ctx, `SELECT h.owner, SUM(h.size) AS held FROM held_blocks h
		JOIN peers p ON p.site_id = h.owner WHERE p.online = 1
		GROUP BY h.owner ORDER BY held DESC`)
	if err != nil {
		return false
	}
	var owners []string
	for rows.Next() {
		var o string
		var held int64
		rows.Scan(&o, &held)
		owners = append(owners, o)
	}
	rows.Close()

	var freed int64
	for _, owner := range owners {
		if freed >= target || ctx.Err() != nil {
			break
		}
		// Ask for twice what's still needed: the owner may keep some.
		ids, sizes := s.heldCandidates(owner, 2*(target-freed))
		if len(ids) == 0 {
			continue
		}
		body, _ := json.Marshal(releaseRequest{IDs: ids, Bytes: target - freed})
		var resp releaseResponse
		_, err := s.peerCall(ctx, owner, http.MethodPost, "/peer/v1/blocks/release", body, &resp)
		if err != nil {
			s.log.Warn("ask a site to release copies", "peer", owner[:8], "err", err)
			continue
		}
		s.SetSetting("room_made_at", itoa(time.Now().Unix())) // before deleting: no new copies meanwhile
		n := s.dropHeld(ctx, owner, resp.Released)
		for _, id := range resp.Released {
			freed += sizes[id]
		}
		if n > 0 {
			s.log.Info("gave back other sites' copies to make room for this site's files", "peer", owner[:8], "copies", n)
		}
	}
	if freed > 0 {
		s.notify(TopicReplication, TopicNodes, TopicPeers)
		s.markDirty()
	}
	return freed > 0
}

// heldCandidates lists copies held for owner, up to about bytes, newest first.
func (s *Site) heldCandidates(owner string, bytes int64) ([]string, map[string]int64) {
	rows, err := s.DB.Query(`SELECT id, size FROM held_blocks WHERE owner = ? ORDER BY stored DESC LIMIT 4096`, owner)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var ids []string
	sizes := map[string]int64{}
	var sum int64
	for rows.Next() && sum < bytes {
		var id string
		var size int64
		rows.Scan(&id, &size)
		ids = append(ids, id)
		sizes[id] = size
		sum += size
	}
	return ids, sizes
}

// dropHeld deletes released copies held for owner and gives their space back
// to the nodes.
func (s *Site) dropHeld(ctx context.Context, owner string, ids []string) int {
	n := 0
	for _, id := range ids {
		var nodeID string
		var size int64
		if s.DB.QueryRow(`SELECT node_id, size FROM held_blocks WHERE owner = ? AND id = ?`, owner, id).Scan(&nodeID, &size) != nil {
			continue
		}
		if st := s.nodeStore(nodeID); st != nil {
			if err := st.Delete(ctx, owner, id); err != nil {
				continue // the owner's garbage collector asks again
			}
		}
		s.DB.Exec(`DELETE FROM held_blocks WHERE owner = ? AND id = ?`, owner, id)
		s.DB.Exec(`UPDATE nodes SET free = free + ? WHERE id = ?`, size, nodeID)
		n++
	}
	return n
}

type releaseRequest struct {
	IDs   []string `json:"ids"`
	Bytes int64    `json:"bytes"`
}

type releaseResponse struct {
	Released []string `json:"released"`
}

// handleRelease is the owner's side: the caller, which holds copies of our
// blocks, needs room for its own files. Copies are released only when the
// block keeps its original here and at least one copy on another online
// site, after trying to place a replacement first.
func (s *Site) handleRelease(w http.ResponseWriter, r *http.Request) {
	holder := callerOf(r).siteID
	var req releaseRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	writeJSON(w, releaseResponse{Released: nonNil(s.releaseCopies(r.Context(), holder, req.IDs, req.Bytes))})
}

func (s *Site) releaseCopies(ctx context.Context, holder string, ids []string, bytes int64) []string {
	deadline := time.Now().Add(replaceDeadline)
	// Replacements stop at the deadline, also one in progress.
	rctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	full := map[string]bool{holder: true}
	var released []string
	var freed int64
	replaced, short := 0, 0
	for _, id := range ids {
		if freed >= bytes || ctx.Err() != nil {
			break
		}
		var size int64
		var refs int
		var nodeID string
		if s.DB.QueryRow(`SELECT size, refs, node_id FROM blocks WHERE id = ?`, id).Scan(&size, &refs, &nodeID) != nil || refs <= 0 {
			released = append(released, id) // not ours any more: garbage
			s.DB.Exec(`UPDATE replicas SET state = 'delete' WHERE block_id = ? AND site_id = ?`, id, holder)
			continue
		}
		if nodeID == "" {
			continue // the holder's copy may be what restores ours
		}
		others := s.otherCopies(id, holder)
		if time.Now().Before(deadline) && s.placeReplacement(rctx, id, full) {
			others++
			replaced++
		} else if others >= 1 {
			short++
		}
		if others < 1 {
			continue
		}
		s.DB.Exec(`UPDATE replicas SET state = 'delete' WHERE block_id = ? AND site_id = ?`, id, holder)
		released = append(released, id)
		freed += size
	}
	if len(released) > 0 {
		s.notify(TopicReplication)
		s.log.Info("released copies to a site that needs room for its own files", "peer", holder[:8],
			"copies", len(released), "replaced", replaced, "until_there_is_room", short)
	}
	return released
}

// otherCopies counts copies of a block on online sites other than except.
func (s *Site) otherCopies(id, except string) int {
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM replicas r JOIN peers p ON p.site_id = r.site_id
		WHERE r.block_id = ? AND r.site_id != ? AND r.state = 'ok' AND p.online = 1`, id, except).Scan(&n)
	return n
}

// placeReplacement copies a block to an online site that has room and no
// copy yet. full collects sites found full (and the site giving it back).
func (s *Site) placeReplacement(ctx context.Context, id string, full map[string]bool) bool {
	have := map[string]bool{}
	rows, err := s.DB.Query(`SELECT site_id FROM replicas WHERE block_id = ?`, id)
	if err != nil {
		return false
	}
	for rows.Next() {
		var sid string
		rows.Scan(&sid)
		have[sid] = true
	}
	rows.Close()
	var ct []byte
	for _, p := range s.onlinePeers() {
		if have[p.siteID] || full[p.siteID] {
			continue
		}
		if p.free < blockcodec.BlockSize {
			full[p.siteID] = true
			continue
		}
		if ct == nil {
			if ct, err = s.readBlock(ctx, id); err != nil {
				return false
			}
		}
		err := s.peerPutBlock(ctx, p.siteID, id, ct)
		if isStatus(err, http.StatusInsufficientStorage) {
			full[p.siteID] = true
			continue
		}
		if err != nil {
			continue
		}
		s.sentCopy(p.siteID, len(ct))
		s.DB.Exec(`INSERT INTO replicas(block_id, site_id, state, verified) VALUES(?, ?, 'ok', ?)
			ON CONFLICT(block_id, site_id) DO UPDATE SET state = 'ok', verified = excluded.verified`,
			id, p.siteID, time.Now().Unix())
		return true
	}
	return false
}
