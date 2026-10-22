package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"ibs/internal/blockcodec"
	"ibs/internal/db"
	"ibs/internal/keys"
	"ibs/internal/version"
)

// Small files share blocks (docs/design/shared-blocks.md). A shared block is
// the encrypted blocks of several small files placed one after another; each
// keeps its own nonce and check value (a "piece", identified by its hash, the
// ID it had as a block of its own). Its ID is the hash of all of it, like any
// block's, so other sites, scrub and repair see an ordinary block.
//
//   - New small files are stored and confirmed as blocks of their own, as
//     always. They're combined into a shared block as soon as a block's worth
//     has arrived, and the rest as soon as their bucket stops receiving
//     files; until then their copies wait, so only the shared block is
//     copied.
//   - Deleting a file in a shared block counts its piece as dead space.
//     Cleanup rewrites blocks at most half alive, and small blocks of their
//     own, into new full ones. A rewritten block gets its copies first; only
//     then do files move into it, so no file ever has fewer copies. Cleanup
//     makes way for everything else: it pauses while files arrive or are
//     deleted, or other sites send copies, and goes on resumeAfter later.
//
// Every step is safe to stop at: until files are switched (in one
// transaction) they use the blocks they had, and a new block left without
// files is garbage collected.
const (
	smallBlock    = 1 << 20          // blocks smaller than this are combined
	resumeAfter   = 5 * time.Second  // cleanup goes on this long after other work stopped
	cleanupEvery  = 5 * time.Second  // a cleanup round, then combining and moves get their turn
	combineScope  = 10 * time.Minute // combining looks at blocks this new; older ones are cleanup's
	movesInFlight = 8                // shared blocks written by cleanup waiting for their copies
	movesExpire   = 24 * time.Hour   // a rewritten block that never got its copies is given up
)

// blockMove is a file's piece moving into a new shared block: the file
// (Key), the block it is in now (From), the piece's hash, and where it sits
// in the new block.
type blockMove struct {
	Key   string `json:"key"`
	From  string `json:"from"`
	Piece string `json:"piece"`
	Size  int64  `json:"size"`
	Off   int64  `json:"off"`
	Len   int64  `json:"len"`
}

// pieceSource is where a piece is read from: a whole small block, or part of
// a shared one.
type pieceSource struct {
	ref BlockRef // From: ID; for a shared block also Off, Len, Piece
	key string
}

// sharingAllowed says whether this site may create shared blocks: not while
// a newly installed version is on probation, so the launcher can still go
// back to the version before, which may not read them.
func (s *Site) sharingAllowed() bool {
	return !UnderLauncher() || s.store().Good() == version.Version
}

// sharedBlocksOnce combines new small blocks, moves files into rewritten
// blocks that have their copies, and cleans up, in that order, so the three
// never pick the same blocks.
func (s *Site) sharedBlocksOnce(ctx context.Context) {
	if !s.sharingAllowed() {
		return
	}
	act := s.Activity()
	busy := act.Writing || act.Deleting || s.copiesArriving()
	if busy {
		s.quietSince = time.Time{}
		s.cleanupDone = false // a delete may have left space to free
	} else if s.quietSince.IsZero() {
		s.quietSince = time.Now()
	}
	s.combineNew(ctx, act.Buckets)
	s.finishMoves(ctx)
	if !busy && !s.cleanupDone && time.Since(s.quietSince) >= resumeAfter {
		s.cleanupDone = !s.cleanup(ctx) // nothing left until something changes
	}
}

// copiesArriving reports whether other sites are sending copies here.
func (s *Site) copiesArriving() bool {
	arriving := false
	s.rates.Range(func(_, v any) bool {
		arriving = v.(*peerRates).in.bps() > 0
		return !arriving
	})
	return arriving
}

// combineHold is the part of the replication query that holds back copies
// of new small blocks combining hasn't looked at yet: in a bucket still
// receiving files, or stored since its last look (combinedAt).
func (s *Site) combineHold() (string, []any) {
	if !s.sharingAllowed() {
		return "", nil
	}
	expr := "0"
	var args []any
	s.combineMu.Lock()
	if len(s.combinedAt) > 0 {
		expr = "CASE b.bucket"
		for b, at := range s.combinedAt {
			expr += " WHEN ? THEN ?"
			args = append(args, b, at)
		}
		expr += " ELSE 0 END"
	}
	s.combineMu.Unlock()
	return " AND NOT (b.shared = 0 AND b.file != '' AND b.size < ? AND b.touched > ? AND b.touched >= " + expr + ")",
		append([]any{smallBlock, time.Now().Add(-combineScope).Unix()}, args...)
}

// combineNew writes new small blocks without copies elsewhere into shared
// blocks: a block's worth as soon as it's there, and the rest once their
// bucket stops receiving files (writing: the buckets that are). Then their
// copies may start: a lone small file stays a block of its own.
func (s *Site) combineNew(ctx context.Context, writing map[string]string) {
	rows, err := s.DB.QueryContext(ctx, `SELECT bucket, COUNT(*), SUM(size), MIN(touched) FROM blocks b
		WHERE shared = 0 AND touched > ? AND refs = 1 AND upload = '' AND file != '' AND size < ? AND node_id != ''
			AND NOT EXISTS (SELECT 1 FROM replicas r WHERE r.block_id = b.id)
		GROUP BY bucket`, time.Now().Add(-combineScope).Unix(), smallBlock)
	if err != nil {
		return
	}
	type bucket struct {
		name            string
		n, size, oldest int64
	}
	var buckets []bucket
	for rows.Next() {
		var b bucket
		rows.Scan(&b.name, &b.n, &b.size, &b.oldest)
		buckets = append(buckets, b)
	}
	rows.Close()
	now := time.Now().Unix()
	for _, b := range buckets {
		receiving := writing[b.name] == "writing"
		for ctx.Err() == nil && b.n >= 2 {
			srcs, total, _ := s.newSmallBlocks(ctx, b.name)
			if len(srcs) < 2 || (receiving && total < blockcodec.BlockSize-smallBlock) {
				break // the rest waits until the bucket stops receiving files
			}
			if err := s.rewrite(ctx, b.name, srcs, 0); err != nil {
				s.log.Warn("combine small files into a block", "bucket", b.name, "err", err)
				break
			}
		}
		s.combineMu.Lock()
		if receiving {
			s.combinedAt[b.name] = 0 // hold copies of everything new
		} else {
			s.combinedAt[b.name] = now
		}
		s.combineMu.Unlock()
		if !receiving {
			s.replDirty.Store(true) // what wasn't combined may be copied now
		}
	}
	// Buckets with nothing new left: no holding back.
	s.combineMu.Lock()
	for name := range s.combinedAt {
		if writing[name] != "writing" && !slices.ContainsFunc(buckets, func(b bucket) bool { return b.name == name }) {
			delete(s.combinedAt, name)
		}
	}
	s.combineMu.Unlock()
}

// newSmallBlocks picks the oldest new small blocks of a bucket that fit in
// one block together.
func (s *Site) newSmallBlocks(ctx context.Context, bucket string) (srcs []pieceSource, total, oldest int64) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, size, file, touched FROM blocks b
		WHERE shared = 0 AND touched > ? AND bucket = ? AND refs = 1 AND upload = '' AND file != '' AND size < ? AND node_id != ''
			AND NOT EXISTS (SELECT 1 FROM replicas r WHERE r.block_id = b.id)
		ORDER BY touched, id LIMIT 1024`, time.Now().Add(-combineScope).Unix(), bucket, smallBlock)
	if err != nil {
		return nil, 0, 0
	}
	defer rows.Close()
	oldest = time.Now().Unix()
	for rows.Next() {
		var id, file string
		var size, touched int64
		rows.Scan(&id, &size, &file, &touched)
		if total+size > blockcodec.BlockSize {
			break
		}
		srcs = append(srcs, pieceSource{ref: BlockRef{ID: id, Len: size}, key: file})
		total += size
		oldest = min(oldest, touched)
	}
	return srcs, total, oldest
}

// rewrite reads the pieces and writes them into one new shared block. With
// target 0 (none of them has copies elsewhere) the files move into it at
// once; otherwise it waits in block_moves until it has target copies on
// other sites (finishMoves).
func (s *Site) rewrite(ctx context.Context, bucket string, srcs []pieceSource, target int) error {
	var ct []byte
	var moves []blockMove
	for _, src := range srcs {
		var piece []byte
		var err error
		if src.ref.Piece != "" {
			piece, err = s.readPiece(ctx, src.ref)
		} else {
			piece, err = s.readBlock(ctx, src.ref.ID)
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", src.ref.ID[:12], err)
		}
		id := src.ref.identity()
		if !blockcodec.Verify(id, piece) {
			return fmt.Errorf("piece %s: %w", id[:12], blockcodec.ErrCorrupt)
		}
		moves = append(moves, blockMove{Key: src.key, From: src.ref.ID, Piece: id, Off: int64(len(ct)), Len: int64(len(piece))})
		ct = append(ct, piece...)
	}
	id := blockcodec.ID(ct)
	marker := "shared:" + keys.RandomToken(8)
	if err := s.storeBlock(ctx, bucket, marker, id, ct); err != nil {
		return err
	}
	if target == 0 {
		var moved int
		err := s.inTx(func(tx *sql.Tx) (err error) {
			moved, err = switchPieces(tx, id, bucket, int64(len(ct)), moves, true)
			return err
		})
		if err != nil {
			s.DB.Exec(`UPDATE blocks SET upload = '' WHERE id = ? AND upload = ?`, id, marker)
			return err
		}
		if moved > 0 {
			s.notify(TopicData, TopicReplication) // the shared block is ready to be copied
		}
		return nil
	}
	mj, _ := json.Marshal(moves)
	err := s.inTx(func(tx *sql.Tx) error {
		// One reference held by the move, so the block is copied but not
		// collected while it waits.
		if _, err := tx.Exec(`UPDATE blocks SET refs = 1, upload = '', shared = 1 WHERE id = ?`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO block_moves(block, bucket, created, target, moves) VALUES(?, ?, ?, ?, ?)`,
			id, bucket, time.Now().Unix(), target, string(mj))
		return err
	})
	if err == nil {
		s.notify(TopicReplication) // copy it now: the files move once it has its copies
	}
	return err
}

// switchPieces points files at their pieces in the new block id, where
// they still use the piece where it was (a file may have been deleted or
// overwritten meanwhile), and gives up the old places. fresh: the pieces
// come from new blocks that must still have no copies elsewhere, so no file
// loses copies by moving. It sets the new block's references to the files
// that moved; with none, garbage collection removes it.
func switchPieces(tx *sql.Tx, id, bucket string, size int64, moves []blockMove, fresh bool) (int, error) {
	moved, live := 0, int64(0)
	for _, m := range moves {
		if fresh {
			var copies int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM replicas WHERE block_id = ?`, m.From).Scan(&copies); err != nil {
				return 0, err
			}
			if copies > 0 {
				continue // copied meanwhile: stays a block of its own; cleanup takes it later
			}
		}
		var raw string
		err := tx.QueryRow(`SELECT blocks FROM objects WHERE bucket = ? AND key = ?`, bucket, m.Key).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		refs := decodeBlocks(raw)
		i := -1
		for j, r := range refs {
			if r.ID == m.From && r.identity() == m.Piece {
				i = j
				break
			}
		}
		if i < 0 {
			continue
		}
		old := refs[i]
		refs[i] = BlockRef{ID: id, Size: old.Size, Off: m.Off, Len: m.Len, Piece: m.Piece}
		if _, err := tx.Exec(`UPDATE objects SET blocks = ? WHERE bucket = ? AND key = ?`, encodeBlocks(refs), bucket, m.Key); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE blocks SET refs = refs - 1, dead = dead + CASE WHEN shared = 1 THEN ? ELSE 0 END WHERE id = ?`,
			m.Len, m.From); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT INTO pieces(piece, block, bucket, key) VALUES(?, ?, ?, ?)
			ON CONFLICT(piece) DO UPDATE SET block = excluded.block, bucket = excluded.bucket, key = excluded.key`,
			m.Piece, id, bucket, m.Key); err != nil {
			return 0, err
		}
		moved++
		live += m.Len
	}
	_, err := tx.Exec(`UPDATE blocks SET refs = ?, upload = '', shared = 1, dead = ? WHERE id = ?`, moved, size-live, id)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(`DELETE FROM block_moves WHERE block = ?`, id)
	return moved, err
}

// finishMoves moves files into rewritten blocks once those have as many
// copies on other sites as the blocks the files are in now, and gives up
// rewritten blocks that waited movesExpire.
func (s *Site) finishMoves(ctx context.Context) {
	rows, err := s.DB.QueryContext(ctx, `SELECT m.block, m.bucket, m.created, m.target, m.moves, b.size FROM block_moves m
		JOIN blocks b ON b.id = m.block`)
	if err != nil {
		return
	}
	type pending struct {
		id, bucket, moves string
		created, size     int64
		target            int
	}
	var list []pending
	for rows.Next() {
		var p pending
		rows.Scan(&p.id, &p.bucket, &p.created, &p.target, &p.moves, &p.size)
		list = append(list, p)
	}
	rows.Close()
	for _, p := range list {
		if s.countedCopies(p.id) < p.target {
			if time.Since(time.Unix(p.created, 0)) > movesExpire {
				s.inTx(func(tx *sql.Tx) error {
					if _, err := tx.Exec(`UPDATE blocks SET refs = 0 WHERE id = ?`, p.id); err != nil {
						return err
					}
					_, err := tx.Exec(`DELETE FROM block_moves WHERE block = ?`, p.id)
					return err
				})
				s.log.Warn("gave up rewriting a shared block: it didn't get its copies on other sites in time", "block", p.id[:12])
			}
			continue
		}
		var moves []blockMove
		json.Unmarshal([]byte(p.moves), &moves)
		if err := s.inTx(func(tx *sql.Tx) error {
			_, err := switchPieces(tx, p.id, p.bucket, p.size, moves, false)
			return err
		}); err != nil {
			s.log.Warn("move files into a rewritten block", "block", p.id[:12], "err", err)
			continue
		}
		s.notify(TopicData, TopicReplication)
	}
}

// countedCopies is how many copies of a block other sites hold that count:
// on sites seen within the grace period.
func (s *Site) countedCopies(id string) int {
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM replicas r JOIN peers p ON p.site_id = r.site_id
		WHERE r.block_id = ? AND r.state = 'ok' AND p.last_seen >= ?`, id, time.Now().Add(-s.cfg.PeerGrace).Unix()).Scan(&n)
	return n
}

// cleanup reports whether it found something to clean up.
//
// It rewrites, gently, shared blocks at most half alive (emptiest first)
// and small blocks of their own that have copies, into new full blocks. Not
// while files arrive or are deleted, and never more than movesInFlight
// rewritten blocks waiting for their copies.
func (s *Site) cleanup(ctx context.Context) bool {
	if s.cfg.CleanupRate <= 0 || s.Setting("block_files") != "1" {
		return false
	}
	if a := s.Activity(); a.Writing || a.Deleting {
		return true // look again soon
	}
	busy := map[string]bool{} // blocks pending moves are reading from
	var inFlight int
	if rows, err := s.DB.QueryContext(ctx, `SELECT moves FROM block_moves`); err == nil {
		for rows.Next() {
			var mj string
			rows.Scan(&mj)
			var moves []blockMove
			json.Unmarshal([]byte(mj), &moves)
			for _, m := range moves {
				busy[m.From] = true
			}
			inFlight++
		}
		rows.Close()
	}
	if inFlight >= movesInFlight {
		return true
	}
	var buckets []string
	if rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT bucket FROM blocks
		WHERE refs > 0 AND node_id != '' AND ((shared = 1 AND dead * 2 >= size) OR (shared = 0 AND size < ? AND file != ''))`, smallBlock); err == nil {
		for rows.Next() {
			var b string
			rows.Scan(&b)
			buckets = append(buckets, b)
		}
		rows.Close()
	}
	// Block after block for up to a round, at most IBS_CLEANUP_RATE, while
	// no files arrive or are deleted.
	did := false
	cursors := map[string]cleanupCursor{}
	for start := time.Now(); time.Since(start) < cleanupEvery && ctx.Err() == nil && inFlight < movesInFlight; {
		if a := s.Activity(); a.Writing || a.Deleting {
			return true
		}
		found := false
		for _, bucket := range buckets {
			srcs, worth := s.cleanupSources(ctx, bucket, busy, cursors)
			if !worth {
				continue
			}
			target := 0
			var size int64
			for _, src := range srcs {
				target = max(target, s.countedCopies(src.ref.ID))
				size += src.ref.Len
			}
			// Written here and sent to each site that should hold a copy.
			s.cleanupLimit.wait(ctx, size*int64(1+target), s.cfg.CleanupRate)
			if err := s.rewrite(ctx, bucket, srcs, target); err != nil {
				s.log.Warn("clean up shared blocks", "bucket", bucket, "err", err)
				continue
			}
			s.lastRewrite.Store(time.Now().UnixNano())
			for _, src := range srcs {
				busy[src.ref.ID] = true
			}
			if target > 0 {
				inFlight++
			}
			did, found = true, true
			break
		}
		if !found {
			break
		}
	}
	return did
}

// cleanupSources picks the live pieces of a bucket's emptiest shared blocks
// and its small blocks of their own, up to one block. worth: rewriting them
// frees space or combines blocks.
func (s *Site) cleanupSources(ctx context.Context, bucket string, busy map[string]bool, cursors map[string]cleanupCursor) ([]pieceSource, bool) {
	var srcs []pieceSource
	var total int64
	fromShared := false
	full := func(n int64) bool { return total+n > blockcodec.BlockSize }

	// Shared blocks at most half alive, the emptiest first.
	type shared struct {
		id         string
		size, dead int64
	}
	var blocks []shared
	if rows, err := s.DB.QueryContext(ctx, `SELECT id, size, dead FROM blocks
		WHERE bucket = ? AND shared = 1 AND refs > 0 AND node_id != '' AND dead * 2 >= size
		ORDER BY CAST(size - dead AS REAL) / size LIMIT 64`, bucket); err == nil {
		for rows.Next() {
			var b shared
			rows.Scan(&b.id, &b.size, &b.dead)
			blocks = append(blocks, b)
		}
		rows.Close()
	}
	for _, b := range blocks {
		if busy[b.id] || full(b.size-b.dead) {
			continue
		}
		pieces := s.livePieces(ctx, bucket, b.id)
		var n int64
		for _, p := range pieces {
			n += p.ref.Len
		}
		if full(n) {
			continue
		}
		srcs = append(srcs, pieces...)
		total += n
		fromShared = true
	}

	// Small blocks of their own, with copies, not new any more: page by page,
	// on from where the round got to in this bucket.
	cur := cursors[bucket]
	for done := false; !done; {
		rows, err := s.DB.QueryContext(ctx, `SELECT id, size, file, touched FROM blocks
			WHERE bucket = ? AND shared = 0 AND refs = 1 AND upload = '' AND file != '' AND size < ? AND node_id != ''
				-- not new ones without copies: those are combining's
				AND (touched <= ? OR EXISTS (SELECT 1 FROM replicas r WHERE r.block_id = blocks.id))
				AND (touched, id) > (?, ?)
			ORDER BY touched, id LIMIT 256`, bucket, smallBlock, time.Now().Add(-combineScope).Unix(), cur.touched, cur.id)
		if err != nil {
			break
		}
		n := 0
		for rows.Next() {
			n++
			var id, file string
			var size, touched int64
			rows.Scan(&id, &size, &file, &touched)
			if busy[id] {
				cur.touched, cur.id = touched, id
				continue
			}
			if full(size) {
				done = true
				break
			}
			cur.touched, cur.id = touched, id
			srcs = append(srcs, pieceSource{ref: BlockRef{ID: id, Len: size}, key: file})
			total += size
		}
		rows.Close()
		if n == 0 {
			break
		}
	}
	cursors[bucket] = cur
	return srcs, len(srcs) >= 2 || (fromShared && len(srcs) >= 1)
}

// cleanupCursor is how far a cleanup round got through a bucket's small
// blocks, so the next block in the round starts after it.
type cleanupCursor struct {
	touched int64
	id      string
}

// livePieces are the pieces of a shared block that files still use.
func (s *Site) livePieces(ctx context.Context, bucket, block string) []pieceSource {
	rows, err := s.DB.QueryContext(ctx, `SELECT piece, key FROM pieces WHERE block = ?`, block)
	if err != nil {
		return nil
	}
	type owner struct{ piece, key string }
	var owners []owner
	for rows.Next() {
		var o owner
		rows.Scan(&o.piece, &o.key)
		owners = append(owners, o)
	}
	rows.Close()
	var out []pieceSource
	for _, o := range owners {
		var raw string
		if s.DB.QueryRowContext(ctx, `SELECT blocks FROM objects WHERE bucket = ? AND key = ?`, bucket, o.key).Scan(&raw) != nil {
			continue
		}
		for _, r := range decodeBlocks(raw) {
			if r.ID == block && r.Piece == o.piece {
				out = append(out, pieceSource{ref: r, key: o.key})
				break
			}
		}
	}
	return out
}

// fillBlockFiles records, once, which file each block belongs to for blocks
// stored before blocks knew it, so cleanup can combine them. A thousand
// files at a time.
func (s *Site) fillBlockFiles(ctx context.Context) {
	if s.Setting("block_files") == "1" {
		return
	}
	var after struct{ bucket, key string }
	for ctx.Err() == nil {
		rows, err := s.DB.QueryContext(ctx, `SELECT bucket, key, blocks FROM objects WHERE (bucket, key) > (?, ?)
			ORDER BY bucket, key LIMIT 1000`, after.bucket, after.key)
		if err != nil {
			return
		}
		type obj struct{ bucket, key, blocks string }
		var batch []obj
		for rows.Next() {
			var o obj
			rows.Scan(&o.bucket, &o.key, &o.blocks)
			batch = append(batch, o)
		}
		rows.Close()
		if len(batch) == 0 {
			db.SetSetting(s.DB, "block_files", "1")
			return
		}
		err = s.inTx(func(tx *sql.Tx) error {
			for _, o := range batch {
				for _, r := range decodeBlocks(o.blocks) {
					if r.shared() {
						continue
					}
					if _, err := tx.Exec(`UPDATE blocks SET file = ? WHERE id = ? AND file = ''`, o.key, r.ID); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			s.log.Warn("record which file each block belongs to", "err", err)
			return
		}
		last := batch[len(batch)-1]
		after.bucket, after.key = last.bucket, last.key
	}
}

// largeAfter is blockcodec.LargeAfter, lowered in tests.
var largeAfter int64 = blockcodec.LargeAfter

// largeBlocksOK says whether blocks may be LargeBlockSize: every other site
// accepts copies that large.
func (s *Site) largeBlocksOK() bool {
	var older int
	s.DB.QueryRow(`SELECT COUNT(*) FROM peers WHERE max_block < ?`, blockcodec.LargeBlockSize).Scan(&older)
	return older == 0
}
