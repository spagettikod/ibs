package site

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ibs/internal/keys"
)

// New files are age blocks, combined into whole shared blocks that read
// back, also in part; the metadata backup is an age file too.
func TestEverythingInAge(t *testing.T) {
	s, files := sharedTestSite(t)
	ctx := context.Background()
	if n := count(t, s, `SELECT COUNT(*) FROM blocks WHERE age = 0`); n != 0 {
		t.Fatalf("%d blocks not in age", n)
	}
	s.combineNew(ctx, nil)
	if count(t, s, `SELECT COUNT(*) FROM blocks WHERE shared = 1 AND age = 1`) == 0 {
		t.Fatal("no shared age block")
	}
	readAll(t, s, files)
	blob, err := s.snapshot(ctx)
	if err != nil || !keys.IsAge(blob) {
		t.Fatalf("backup not in age: %v", err)
	}
	if _, err := openBackup(s.ID, blob); err != nil {
		t.Fatal(err)
	}
	if _, err := openBackup(s.ID, []byte("\x01\x02 a backup from before age")); err == nil || !strings.Contains(err.Error(), "1.10") {
		t.Fatalf("an old backup: %v", err)
	}
}

// A site with data still encrypted the way from before age doesn't start
// with this version, and says to run 1.10 first.
func TestRefusesDataFromBeforeAge(t *testing.T) {
	s, _ := sharedTestSite(t)
	cfg := s.cfg
	s.DB.Exec(`UPDATE blocks SET age = 0 WHERE id = (SELECT id FROM blocks WHERE refs > 0 LIMIT 1)`)
	s.Close()
	if _, err := Open(cfg); err == nil || !strings.Contains(err.Error(), "IBS 1.10") {
		t.Fatalf("opened a site with a block from before age: %v", err)
	}
}

// The export writes every file from the recovery phrase, the index (the
// database or a metadata backup) and the blocks, without the site.
func TestExport(t *testing.T) {
	s, files := sharedTestSite(t)
	ctx := context.Background()
	s.combineNew(ctx, nil) // some in a shared block, the big file's first block on its own
	s.collectGarbage(ctx)
	phrase, err := RecoveryPhrase(s.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	blocks := filepath.Join(s.cfg.DataDir, "blocks")
	check := func(index string) {
		t.Helper()
		out := t.TempDir()
		res, err := Export(ExportOptions{Phrase: phrase, Index: index, Blocks: []string{blocks}, Out: out, Log: os.Stderr})
		if err != nil || len(res.Missing) > 0 || res.Files != len(files) {
			t.Fatalf("export from %s: %v, %+v", filepath.Base(index), err, res)
		}
		for k, want := range files {
			got, err := os.ReadFile(filepath.Join(out, "small", k))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("%s exported different: %v", k, err)
			}
		}
	}
	check(filepath.Join(s.cfg.DataDir, "ibs.db"))
	blob, _ := s.snapshot(ctx)
	backup := filepath.Join(t.TempDir(), "backup")
	os.WriteFile(backup, blob, 0o600)
	check(backup)

	var keysOut bytes.Buffer
	if _, err := Export(ExportOptions{Phrase: phrase, Index: backup, PrintKeys: true, Log: &keysOut}); err != nil ||
		strings.Count(keysOut.String(), "AGE-SECRET-KEY-PQ-1") != 2 {
		t.Fatalf("keys: %v\n%s", err, keysOut.String())
	}
	other := keys.EncodePhrase(keys.NewSeed())
	if _, err := Export(ExportOptions{Phrase: other, Index: backup, Out: t.TempDir(), Log: os.Stderr}); err == nil {
		t.Fatal("exported with another site's phrase")
	}
	res, err := Export(ExportOptions{Phrase: phrase, Index: backup, Blocks: []string{t.TempDir()}, Out: t.TempDir(), Log: &bytes.Buffer{}})
	if err == nil && len(res.Missing) != len(files) {
		t.Fatalf("without blocks: %d missing, want all %d", len(res.Missing), len(files))
	}
}
