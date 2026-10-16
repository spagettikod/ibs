package site

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ibs/internal/blockcodec"
	"ibs/internal/blockstore"
	"ibs/internal/keys"
)

func TestScrub(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	if err := s.CreateBucket("data"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutObject(ctx, "data", "photos/cat.jpg", bytes.NewReader([]byte("meow")), "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutObject(ctx, "data", "notes.txt", bytes.NewReader([]byte("fine")), "", nil); err != nil {
		t.Fatal(err)
	}
	o, _ := s.HeadObject("data", "photos/cat.jpg")
	fsys := s.nodeStore(LocalNodeID).(*blockstore.FS)
	path := func(ns, id string) string { return filepath.Join(fsys.Dir, ns, id[:2], id) }

	// A block kept for another site.
	owner := strings.Repeat("ab", 16)
	hid, hct := blockcodec.Encrypt(keys.RandomKey(), []byte("theirs"))
	fsys.Put(ctx, owner, hid, hct)
	s.DB.Exec(`INSERT INTO held_blocks(owner, id, size, node_id, stored, verified) VALUES(?, ?, ?, ?, ?, 0)`,
		owner, hid, len(hct), LocalNodeID, time.Now().Unix())

	// Damage one of ours and the one kept for the other site.
	os.WriteFile(path(blockstore.OwnNS, o.Blocks[0].ID), []byte("rot"), 0o600)
	os.WriteFile(path(owner, hid), []byte("rot"), 0o600)

	if err := s.StartScrub(true); err != nil {
		t.Fatal(err)
	}
	if err := s.StartScrub(true); err == nil {
		t.Fatal("started a second scrub")
	}
	sc := s.currentScrub()
	if sc.Total != 3 {
		t.Fatalf("total %d, want 3 blocks", sc.Total)
	}

	// Paused, it does nothing.
	s.PauseScrub()
	s.runScrub(ctx, sc.ID)
	if c := s.currentScrub(); c == nil || c.State != "paused" || c.Checked != 0 {
		t.Fatalf("paused scrub ran: %+v", c)
	}
	s.ResumeScrub()
	s.runScrub(ctx, sc.ID)

	info := s.ScrubStatus()
	if info.Current != nil || info.Last == nil || info.Last.State != "done" || info.Last.Checked != 3 {
		t.Fatalf("scrub didn't finish: %+v", info)
	}
	var block, held *ScrubError
	for i, e := range info.Errors {
		switch e.What {
		case "block":
			block = &info.Errors[i]
		case "held":
			held = &info.Errors[i]
		}
	}
	// No other site has a copy, so our block can't be repaired, and the files
	// using it are named.
	if block == nil || block.Outcome != "failed" || block.Problem != "damaged" || !strings.Contains(block.Detail, "data/photos/cat.jpg") {
		t.Fatalf("damaged block: %+v", block)
	}
	if info.Last.Failed != 1 || info.Unresolved != 1 {
		t.Fatalf("failed %d, unresolved %d; want 1 and 1", info.Last.Failed, info.Unresolved)
	}
	// The copy kept for the other site is dropped, so that site sends a new one.
	if held == nil || held.Outcome != "replaced" {
		t.Fatalf("damaged held block: %+v", held)
	}
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM held_blocks`).Scan(&n)
	if n != 0 {
		t.Fatal("the damaged held block is still recorded")
	}
	if r := s.heldReport(owner); r.Checked != 1 || r.Damaged != 1 || r.Scrubbed == 0 {
		t.Fatalf("report for the other site: %+v", r)
	}

	// Reading the file fails instead of returning damaged data.
	if _, err := s.readBlock(ctx, o.Blocks[0].ID); err == nil {
		t.Fatal("read a block that has no intact copy")
	}

	// The schedule.
	if info.IntervalDays != defaultScrubDays || info.NextDue < info.Last.Started+30*86400 {
		t.Fatalf("schedule: every %d days, next %d", info.IntervalDays, info.NextDue)
	}
	if s.SetScrubInterval(0) != nil || s.ScrubStatus().NextDue != 0 {
		t.Fatal("scheduled scrubs can't be turned off")
	}
	if s.SetScrubInterval(-1) == nil {
		t.Fatal("accepted a negative interval")
	}
}

// The site holding copies answers the challenge only for the exact bytes, and
// reports missing and damaged copies; the owner drops copies whose answer
// differs from its own and has them copied again.
func TestCopyChallenge(t *testing.T) {
	holder := newTestSite(t)
	ctx := context.Background()
	fsys := holder.nodeStore(LocalNodeID).(*blockstore.FS)
	owner := strings.Repeat("cd", 16)
	put := func(plain string) (string, []byte) {
		id, ct := blockcodec.Encrypt(keys.RandomKey(), []byte(plain))
		fsys.Put(ctx, owner, id, ct)
		holder.DB.Exec(`INSERT INTO held_blocks(owner, id, size, node_id, stored, verified) VALUES(?, ?, ?, ?, ?, 0)`,
			owner, id, len(ct), LocalNodeID, time.Now().Unix())
		return id, ct
	}
	good, goodCT := put("intact")
	rotten, _ := put("will rot")
	os.WriteFile(filepath.Join(fsys.Dir, owner, rotten[:2], rotten), []byte("rot"), 0o600)
	gone, _ := blockcodec.Encrypt(keys.RandomKey(), []byte("never stored"))

	nonce := newNonce()
	reply := holder.checkHeld(ctx, owner, []string{good, rotten, gone}, nonce)
	if reply.Answers[good] != blockstore.ChallengeAnswer(nonce, goodCT) {
		t.Fatalf("answer for the intact copy is wrong: %+v", reply)
	}
	if len(reply.Damaged) != 1 || reply.Damaged[0] != rotten || len(reply.Missing) != 1 || reply.Missing[0] != gone {
		t.Fatalf("damaged %v, missing %v", reply.Damaged, reply.Missing)
	}
	var left int
	holder.DB.QueryRow(`SELECT COUNT(*) FROM held_blocks WHERE owner = ?`, owner).Scan(&left)
	if left != 1 {
		t.Fatalf("%d copies left; the damaged one should be dropped", left)
	}
	if again := holder.checkHeld(ctx, owner, []string{good}, newNonce()); again.Answers[good] == reply.Answers[good] {
		t.Fatal("a new nonce must give a new answer")
	}

	// The owner's side: one copy answers right, one wrong, one is missing.
	s := newTestSite(t)
	s.CreateBucket("data")
	for _, f := range []string{"a", "b", "c"} {
		s.PutObject(ctx, "data", f, bytes.NewReader([]byte("file "+f)), "", nil)
	}
	var ids []string
	rows, _ := s.DB.Query(`SELECT id FROM blocks ORDER BY id`)
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	peer := strings.Repeat("ef", 16)
	now := time.Now().Unix()
	s.DB.Exec(`INSERT INTO peers(site_id, name, pubkey, address, added, last_seen, online) VALUES(?, 'site-b', x'', '', ?, ?, 1)`, peer, now, now)
	for _, id := range ids {
		s.DB.Exec(`INSERT INTO replicas(block_id, site_id, state, verified) VALUES(?, ?, 'ok', ?)`, id, peer, now)
	}
	s.StartScrub(true)
	r := &scrubRun{s: s, id: s.currentScrub().ID, unreachable: map[string]bool{}, skipped: map[string]bool{}, legacy: map[string]bool{}}
	local := map[string]string{ids[0]: "right", ids[1]: "right"}
	results := make(chan copyCheckResult, 1)
	results <- copyCheckResult{site: peer, ids: ids, reply: copyCheckReply{
		Answers: map[string]string{ids[0]: "right", ids[1]: "wrong"}, Missing: []string{ids[2]}}}
	close(results)
	r.compareCopies(ctx, results, local)

	var kept []string
	rows, _ = s.DB.Query(`SELECT block_id FROM replicas WHERE site_id = ?`, peer)
	for rows.Next() {
		var id string
		rows.Scan(&id)
		kept = append(kept, id)
	}
	rows.Close()
	if len(kept) != 1 || kept[0] != ids[0] {
		t.Fatalf("copies still counted on the other site: %v; want only the one that answered right", kept)
	}
	var checked, bad int64
	s.DB.QueryRow(`SELECT checked, bad FROM scrub_copies WHERE scrub_id = ? AND site_id = ?`, r.id, peer).Scan(&checked, &bad)
	if checked != 3 || bad != 2 {
		t.Fatalf("checked %d, bad %d; want 3 and 2", checked, bad)
	}
	if sc := s.currentScrub(); sc.Copies != 3 || sc.Repaired != 2 {
		t.Fatalf("scrub counts %d copies, %d repaired", sc.Copies, sc.Repaired)
	}
}

// A copy check request does a bounded amount of reading: what doesn't fit is
// handed back, and a busy site says when to come back.
func TestCopyCheckBounded(t *testing.T) {
	t.Setenv("IBS_SCRUB_RATE", "64K")
	old := copyCheckWork
	copyCheckWork = time.Second // 64 KiB per request
	defer func() { copyCheckWork = old }()
	holder := newTestSite(t)
	ctx := context.Background()
	fsys := holder.nodeStore(LocalNodeID).(*blockstore.FS)
	owner := strings.Repeat("cd", 16)
	var ids []string
	for i := range 3 {
		id, ct := blockcodec.Encrypt(keys.RandomKey(), bytes.Repeat([]byte{byte(i)}, 40<<10))
		fsys.Put(ctx, owner, id, ct)
		holder.DB.Exec(`INSERT INTO held_blocks(owner, id, size, node_id, stored, verified) VALUES(?, ?, ?, ?, ?, 0)`,
			owner, id, len(ct), LocalNodeID, time.Now().Unix())
		ids = append(ids, id)
	}
	start := time.Now()
	reply := holder.checkHeld(ctx, owner, ids, newNonce())
	if len(reply.Answers) != 1 || len(reply.Later) != 2 {
		t.Fatalf("%d answered, %d later; want 1 and 2 (40 KiB blocks, 64 KiB per request)", len(reply.Answers), len(reply.Later))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the request took %v", d)
	}
	// Busy with a lot of reading already: come back later, at once.
	holder.scrubLimit.reserve(1<<20, 64<<10)
	start = time.Now()
	reply = holder.checkHeld(ctx, owner, ids, newNonce())
	if len(reply.Answers) != 0 || len(reply.Later) != 3 || reply.Wait < 5 || time.Since(start) > time.Second {
		t.Fatalf("busy: %d answered, %d later, wait %.1fs, took %v", len(reply.Answers), len(reply.Later), reply.Wait, time.Since(start))
	}
}
