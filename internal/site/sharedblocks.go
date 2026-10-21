package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
//     always. Their copies wait up to combineWait while they're combined into
//     a shared block here, which is then copied once.
//   - Deleting a file in a shared block counts its piece as dead space.
//     Cleanup rewrites blocks at most half alive, and small blocks of their
//     own, into new full ones. A rewritten block gets its copies first; only
//     then do files move into it, so no file ever has fewer copies.
//
// Every step is safe to stop at: until files are switched (in one
// transaction) they use the blocks they had, and a new block left without
// files is garbage collected.
const (
	smallBlock    = 1 << 20          // blocks smaller than this are combined
	combineWait   = 10 * time.Second // how long a new small block may wait to be combined
	replWait      = 15 * time.Second // and its copies wait for that, a little longer
	cleanupAfter  = time.Minute      // small blocks of their own older than this are cleaned up
	cleanupEvery  = 5 * time.Second
	cleanupIdle   = time.Minute      // after a round with nothing to clean up
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

// combineCutoff: small blocks stored after this wait for combining before
// they're copied; none do while sharing isn't allowed.
func (s *Site) combineCutoff() int64 {
	if !s.sharingAllowed() {
		return 0
	}
	return time.Now().Add(-replWait).Unix()
}

// sharedBlocksOnce combines new small blocks, moves files into rewritten
// blocks that have their copies, and cleans up, in that order, so the three
// never pick the same blocks.
func (s *Site) sharedBlocksOnce(ctx context.Context) {
	if !s.sharingAllowed() {
		return
	}
	s.combineNew(ctx)
	s.copyWhenDue()
	s.finishMoves(ctx)
	if time.Now().After(s.nextCleanup) {
		s.nextCleanup = time.Now().Add(cleanupEvery)
		if !s.cleanup(ctx) {
			s.nextCleanup = time.Now().Add(cleanupIdle)
		}
	}
}

// copyWhenDue starts replication for small blocks whose wait for combining
// ended since the last round (the ones not combined, e.g. a lone small
// file): replication only looks again when something changed.
func (s *Site) copyWhenDue() {
	now := time.Now()
	since := s.lastDueCheck
	s.lastDueCheck = now
	if since.IsZero() {
		since = now.Add(-time.Minute) // after a start: anything still waiting
	}
	var due int
	s.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM blocks WHERE shared = 0 AND file != '' AND size < ? AND refs > 0
		AND touched > ? AND touched <= ?)`, smallBlock, since.Add(-replWait).Unix(), now.Add(-replWait).Unix()).Scan(&due)
	if due == 1 {
		s.replDirty.Store(true)
	}
}

// combineNew writes new small blocks without copies elsewhere into shared
// blocks: once a bucket's add up to a block, or the oldest has waited
// combineWait. A lone small file stays a block of its own.
func (s *Site) combineNew(ctx context.Context) {
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
	due := time.Now().Add(-combineWait).Unix()
	for _, b := range buckets {
		if b.n < 2 || (b.size < blockcodec.BlockSize && b.oldest > due) {
			continue
		}
		for ctx.Err() == nil {
			srcs, total, oldest := s.newSmallBlocks(ctx, b.name)
			if len(srcs) < 2 || (total < blockcodec.BlockSize-smallBlock && oldest > due) {
				break
			}
			if err := s.rewrite(ctx, b.name, srcs, 0); err != nil {
				s.log.Warn("combine small files into a block", "bucket", b.name, "err", err)
				break
			}
		}
	}
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
	return s.inTx(func(tx *sql.Tx) error {
		// One reference held by the move, so the block is copied but not
		// collected while it waits.
		if _, err := tx.Exec(`UPDATE blocks SET refs = 1, upload = '', shared = 1 WHERE id = ?`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO block_moves(block, bucket, created, target, moves) VALUES(?, ?, ?, ?, ?)`,
			id, bucket, time.Now().Unix(), target, string(mj))
		return err
	})
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
	for _, bucket := range buckets {
		srcs, worth := s.cleanupSources(ctx, bucket, busy)
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
		return true // one block per round: gentle
	}
	return false
}

// cleanupSources picks the live pieces of a bucket's emptiest shared blocks
// and its small blocks of their own, up to one block. worth: rewriting them
// frees space or combines blocks.
func (s *Site) cleanupSources(ctx context.Context, bucket string, busy map[string]bool) ([]pieceSource, bool) {
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

	// Small blocks of their own, with copies, not new any more.
	if rows, err := s.DB.QueryContext(ctx, `SELECT id, size, file FROM blocks
		WHERE bucket = ? AND shared = 0 AND refs = 1 AND upload = '' AND file != '' AND size < ? AND node_id != '' AND touched < ?
		ORDER BY touched, id LIMIT 1024`, bucket, smallBlock, time.Now().Add(-cleanupAfter).Unix()); err == nil {
		for rows.Next() {
			var id, file string
			var size int64
			rows.Scan(&id, &size, &file)
			if busy[id] {
				continue
			}
			if full(size) {
				break
			}
			srcs = append(srcs, pieceSource{ref: BlockRef{ID: id, Len: size}, key: file})
			total += size
		}
		rows.Close()
	}
	return srcs, len(srcs) >= 2 || (fromShared && len(srcs) >= 1)
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
