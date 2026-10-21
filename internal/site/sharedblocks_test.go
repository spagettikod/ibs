package site

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"testing"
	"time"
)

// sharedTestSite has small files and one large file with a short last part,
// stored a while ago (past the wait for combining).
func sharedTestSite(t *testing.T) (*Site, map[string][]byte) {
	t.Helper()
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("small")
	files := map[string][]byte{}
	for i := range 40 {
		data := make([]byte, 1000+i*97)
		rand.Read(data)
		files[fmt.Sprintf("f%02d", i)] = data
	}
	big := make([]byte, 4<<20+300_000) // a whole block and a short last part
	rand.Read(big)
	files["big"] = big
	for k, data := range files {
		if _, err := s.PutObject(ctx, "small", k, bytes.NewReader(data), "", nil); err != nil {
			t.Fatal(err)
		}
	}
	s.DB.Exec(`UPDATE blocks SET touched = touched - 120`) // past combining and cleanup's wait
	return s, files
}

// readAll checks that every file reads back exactly.
func readAll(t *testing.T, s *Site, files map[string][]byte) {
	t.Helper()
	for k, want := range files {
		o, err := s.HeadObject("small", k)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		var buf bytes.Buffer
		if err := s.WriteObject(context.Background(), &buf, o, 0, o.Size); err != nil {
			t.Fatalf("read %s: %v", k, err)
		}
		if !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("%s reads back different", k)
		}
		// A range across the end of the file's first piece.
		if o.Size > 10 {
			buf.Reset()
			if err := s.WriteObject(context.Background(), &buf, o, 3, o.Size-5); err != nil || !bytes.Equal(buf.Bytes(), want[3:o.Size-2]) {
				t.Fatalf("range of %s: %v", k, err)
			}
		}
	}
}

// quiet forgets recent uploads and deletes, as a second later: cleanup
// waits for those.
func quiet(s *Site) {
	s.act.mu.Lock()
	s.act.writes, s.act.deletes = nil, nil
	s.act.mu.Unlock()
}

func count(t *testing.T, s *Site, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSmallFilesShareBlocks(t *testing.T) {
	s, files := sharedTestSite(t)
	ctx := context.Background()
	before := count(t, s, `SELECT COUNT(*) FROM blocks WHERE refs > 0`)
	s.combineNew(ctx)
	shared := count(t, s, `SELECT COUNT(*) FROM blocks WHERE shared = 1 AND refs > 0`)
	if shared == 0 {
		t.Fatal("no shared blocks")
	}
	s.collectGarbage(ctx)
	after := count(t, s, `SELECT COUNT(*) FROM blocks WHERE refs > 0`)
	t.Logf("blocks: %d before, %d after (%d shared)", before, after, shared)
	// 40 small files and the big file's last part fit in one block; the big
	// file's first block stays.
	if after != 2 || count(t, s, `SELECT COUNT(*) FROM blocks`) != after {
		t.Fatalf("%d blocks in use of %d, want 2 and the small ones collected", after, count(t, s, `SELECT COUNT(*) FROM blocks`))
	}
	if n := count(t, s, `SELECT COUNT(*) FROM pieces`); n != 41 {
		t.Fatalf("%d pieces recorded, want 41", n)
	}
	readAll(t, s, files)

	// Deleting most files leaves the block at most half alive; cleanup
	// rewrites the rest into a new one and the old one goes.
	s.DeleteObject("small", "big")
	delete(files, "big")
	for i := range 30 {
		k := fmt.Sprintf("f%02d", i+10)
		if err := s.DeleteObject("small", k); err != nil {
			t.Fatal(err)
		}
		delete(files, k)
	}
	var old string
	s.DB.QueryRow(`SELECT id FROM blocks WHERE shared = 1`).Scan(&old)
	if count(t, s, `SELECT COUNT(*) FROM blocks WHERE id = ? AND dead * 2 >= size`, old) != 1 {
		t.Fatal("deleting most files didn't count their pieces as dead")
	}
	s.fillBlockFiles(ctx)
	quiet(s)
	s.cleanup(ctx)
	s.collectGarbage(ctx)
	if count(t, s, `SELECT COUNT(*) FROM blocks WHERE id = ?`, old) != 0 {
		t.Fatal("the half-empty block wasn't rewritten")
	}
	if n := count(t, s, `SELECT COUNT(*) FROM blocks WHERE shared = 1 AND dead = 0 AND refs = 10`); n != 1 {
		t.Fatalf("%d full new blocks with the 10 files left, want 1", n)
	}
	readAll(t, s, files)
}

// A file overwritten while its block is combined isn't moved; nor is a
// block that got a copy elsewhere meanwhile: no file loses copies.
func TestCombineSkipsChangedFiles(t *testing.T) {
	s, files := sharedTestSite(t)
	ctx := context.Background()
	srcs, _, _ := s.newSmallBlocks(ctx, "small")
	if len(srcs) < 3 {
		t.Fatalf("%d sources", len(srcs))
	}
	changed, copied := srcs[0].key, srcs[1]
	files[changed] = []byte("new content")
	s.PutObject(ctx, "small", changed, bytes.NewReader(files[changed]), "", nil)
	now := time.Now().Unix()
	s.DB.Exec(`INSERT INTO peers(site_id, name, pubkey, address, added, last_seen, online) VALUES('p', 'p', x'', '', ?, ?, 1)`, now, now)
	s.DB.Exec(`INSERT INTO replicas(block_id, site_id, state, verified) VALUES(?, 'p', 'ok', ?)`, copied.ref.ID, now)
	if err := s.rewrite(ctx, "small", srcs, 0); err != nil {
		t.Fatal(err)
	}
	o, _ := s.HeadObject("small", copied.key)
	if o.Blocks[len(o.Blocks)-1].ID != copied.ref.ID {
		t.Fatal("a block with a copy elsewhere moved into a block without")
	}
	if n := count(t, s, `SELECT refs FROM blocks WHERE shared = 1`); n != len(srcs)-2 {
		t.Fatalf("shared block holds %d files, want %d", n, len(srcs)-2)
	}
	readAll(t, s, files)
}

// With copies elsewhere, files move into a rewritten block only once it has
// as many copies.
func TestCleanupWaitsForCopies(t *testing.T) {
	s, files := sharedTestSite(t)
	ctx := context.Background()
	now := time.Now().Unix()
	s.DB.Exec(`INSERT INTO peers(site_id, name, pubkey, address, added, last_seen, online) VALUES('p', 'p', x'', '', ?, ?, 1)`, now, now)
	s.DB.Exec(`INSERT INTO replicas(block_id, site_id, state, verified) SELECT id, 'p', 'ok', ? FROM blocks`, now)
	s.fillBlockFiles(ctx)
	quiet(s)
	s.cleanup(ctx)
	var block string
	var target int
	if err := s.DB.QueryRow(`SELECT block, target FROM block_moves`).Scan(&block, &target); err != nil || target != 1 {
		t.Fatalf("no rewritten block waiting for 1 copy: %v (target %d)", err, target)
	}
	s.finishMoves(ctx)
	if count(t, s, `SELECT COUNT(*) FROM pieces`) != 0 {
		t.Fatal("files moved before the new block had its copy")
	}
	readAll(t, s, files)
	s.DB.Exec(`INSERT INTO replicas(block_id, site_id, state, verified) VALUES(?, 'p', 'ok', ?)`, block, now)
	s.finishMoves(ctx)
	if count(t, s, `SELECT COUNT(*) FROM pieces WHERE block = ?`, block) == 0 || count(t, s, `SELECT COUNT(*) FROM block_moves`) != 0 {
		t.Fatal("files didn't move once the new block had its copy")
	}
	readAll(t, s, files)
}

// No shared blocks while a newly installed version is on probation: the
// launcher may go back to the one before.
func TestNoSharingOnProbation(t *testing.T) {
	s, _ := sharedTestSite(t)
	t.Setenv("IBS_LAUNCHER", "1")
	if s.sharingAllowed() {
		t.Fatal("sharing on probation")
	}
	s.sharedBlocksOnce(context.Background())
	if count(t, s, `SELECT COUNT(*) FROM blocks WHERE shared = 1`) != 0 {
		t.Fatal("shared blocks made on probation")
	}
}

// A large file's blocks after its first part are large, once every other
// site accepts those; until then they stay the usual size.
func TestLargeBlocks(t *testing.T) {
	was := largeAfter
	largeAfter = 8 << 20
	t.Cleanup(func() { largeAfter = was })
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("video")
	data := make([]byte, 30<<20)
	rand.Read(data)
	sizes := func(key string) []int64 {
		if _, err := s.PutObject(ctx, "video", key, bytes.NewReader(data), "", nil); err != nil {
			t.Fatal(err)
		}
		o, _ := s.HeadObject("video", key)
		var out []int64
		for _, b := range o.Blocks {
			out = append(out, b.Size>>20)
		}
		var buf bytes.Buffer
		if err := s.WriteObject(ctx, &buf, o, 0, o.Size); err != nil || !bytes.Equal(buf.Bytes(), data) {
			t.Fatalf("%s reads back different: %v", key, err)
		}
		return out
	}
	if got := fmt.Sprint(sizes("new")); got != "[4 4 16 6]" {
		t.Fatalf("block sizes in MB %s, want [4 4 16 6]", got)
	}
	now := time.Now().Unix()
	s.DB.Exec(`INSERT INTO peers(site_id, name, pubkey, address, added, last_seen, online) VALUES('old', 'old', x'', '', ?, ?, 1)`, now, now)
	if got := fmt.Sprint(sizes("with-old-site")); got != "[4 4 4 4 4 4 4 2]" {
		t.Fatalf("block sizes in MB %s with a site from before large blocks, want all 4", got)
	}
}
