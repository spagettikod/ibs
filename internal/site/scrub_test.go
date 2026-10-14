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
