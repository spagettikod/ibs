package site

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ibs/internal/blockcodec"
	"ibs/internal/db"
	"ibs/internal/keys"
	"ibs/internal/release"
)

// oldSite has files stored the way from before age (written on probation):
// small files, a lone small file in its own bucket, a file of several blocks,
// and a shared block of separately encrypted pieces as 1.9 made them.
func oldSite(t *testing.T) (*Site, map[string][]byte) {
	t.Helper()
	s := newTestSite(t)
	ctx := context.Background()
	t.Setenv("IBS_LAUNCHER", "1") // on probation: the format from before age
	if s.sharingAllowed() {
		t.Fatal("sharing allowed on probation")
	}
	s.CreateBucket("small")
	files := map[string][]byte{}
	for i := range 12 {
		data := make([]byte, 700+i*301)
		rand.Read(data)
		files[fmt.Sprintf("f%02d", i)] = data
	}
	big := make([]byte, 9<<20+12345)
	rand.Read(big)
	files["big"] = big
	for k, data := range files {
		if _, err := s.PutObject(ctx, "small", k, bytes.NewReader(data), "", nil); err != nil {
			t.Fatal(err)
		}
	}
	// A 1.9 shared block from f00 and f01: their encrypted blocks side by side.
	var ct []byte
	type part struct {
		key string
		ref BlockRef
	}
	var parts []part
	for _, k := range []string{"f00", "f01"} {
		o, _ := s.HeadObject("small", k)
		b, err := s.readBlock(ctx, o.Blocks[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, part{k, BlockRef{Size: o.Blocks[0].Size, Off: int64(len(ct)), Len: int64(len(b)), Piece: o.Blocks[0].ID}})
		ct = append(ct, b...)
	}
	id := blockcodec.ID(ct)
	if err := s.storeBlock(ctx, "small", "", id, ct); err != nil {
		t.Fatal(err)
	}
	s.inTx(func(tx *sql.Tx) error {
		for _, p := range parts {
			ref := p.ref
			ref.ID = id
			tx.Exec(`UPDATE objects SET blocks = ? WHERE bucket = 'small' AND key = ?`, encodeBlocks([]BlockRef{ref}), p.key)
			tx.Exec(`UPDATE blocks SET refs = 0 WHERE id = ?`, ref.Piece)
			tx.Exec(`INSERT INTO pieces(piece, block, bucket, key) VALUES(?, ?, 'small', ?)`, ref.Piece, id, p.key)
		}
		_, err := tx.Exec(`UPDATE blocks SET refs = 2, shared = 1 WHERE id = ?`, id)
		return err
	})
	s.collectGarbage(ctx)
	s.CreateBucket("lone")
	files["lone/one"] = []byte("a lone small file")
	s.PutObject(ctx, "lone", "one", bytes.NewReader(files["lone/one"]), "", nil)
	if n := count(t, s, `SELECT COUNT(*) FROM blocks WHERE age = 1`); n != 0 {
		t.Fatalf("%d age blocks written on probation", n)
	}
	t.Setenv("IBS_LAUNCHER", "")
	return s, files
}

func readBoth(t *testing.T, s *Site, files map[string][]byte) {
	t.Helper()
	for k, want := range files {
		bucket, key := "small", k
		if b, kk, ok := strings.Cut(k, "/"); ok {
			bucket, key = b, kk
		}
		o, err := s.HeadObject(bucket, key)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		var buf bytes.Buffer
		if err := s.WriteObject(context.Background(), &buf, o, 0, o.Size); err != nil || !bytes.Equal(buf.Bytes(), want) {
			t.Fatalf("%s reads back different: %v", k, err)
		}
	}
}

// Everything converts to age, in the background, and reads the same after.
func TestConvertToAge(t *testing.T) {
	s, files := oldSite(t)
	ctx := context.Background()
	readBoth(t, s, files)
	s.ensureAgeKeys()
	s.fillBlockFiles(ctx)
	s.refreshStats()
	if d, _ := s.Dashboard(); d.Cleanup.ConvertLeft == 0 {
		t.Fatal("the dashboard doesn't show blocks to convert")
	}
	m := release.Manifest{Version: "9.9.9", RequiresAge: true, DBLevel: db.Level}
	if s.compatBlocker(m) == "" {
		t.Fatal("a version reading only age installs before the site is converted")
	}
	for range 20 {
		quiet(s)
		s.cleanup(ctx)
		s.collectGarbage(ctx)
		if count(t, s, `SELECT COUNT(*) FROM blocks WHERE age = 0`) == 0 {
			break
		}
	}
	if n := count(t, s, `SELECT COUNT(*) FROM blocks WHERE age = 0`); n != 0 {
		t.Fatalf("%d blocks left in the old format", n)
	}
	readBoth(t, s, files)
	// Converted data is enough, also before the flag is written (a busy site
	// may not get the quiet moment that writes it).
	if why := s.compatBlocker(m); why != "" {
		t.Fatalf("a version reading only age is blocked on a converted site: %s", why)
	}
	s.noteConverted()
	if s.Setting("age_converted") != "1" || s.compatBlocker(m) != "" {
		t.Fatal("the site isn't marked converted")
	}
	// The metadata backup is age now too, and still opens.
	blob, err := s.snapshot(ctx)
	if err != nil || !keys.IsAge(blob) {
		t.Fatalf("backup not in age: %v", err)
	}
	if _, err := openBackup(s.ID, blob); err != nil {
		t.Fatal(err)
	}
}

// New files after probation are age blocks, combined into whole shared
// blocks that read back, also in part.
func TestNewFilesInAge(t *testing.T) {
	s, files := sharedTestSite(t)
	ctx := context.Background()
	if n := count(t, s, `SELECT COUNT(*) FROM blocks WHERE age = 0`); n != 0 {
		t.Fatalf("%d new blocks not in age", n)
	}
	s.combineNew(ctx, nil)
	if count(t, s, `SELECT COUNT(*) FROM blocks WHERE shared = 1 AND age = 1`) == 0 {
		t.Fatal("no shared age block")
	}
	readAll(t, s, files)
}

// The export writes every file from the recovery phrase, the index (the
// database or a metadata backup) and the blocks, without the site.
func TestExport(t *testing.T) {
	s, files := oldSite(t) // some old, then convert part of it: both formats
	ctx := context.Background()
	s.ensureAgeKeys()
	s.fillBlockFiles(ctx)
	quiet(s)
	s.cleanup(ctx)
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
			if !strings.Contains(k, "/") {
				k = "small/" + k
			}
			got, err := os.ReadFile(filepath.Join(out, k))
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
		strings.Count(keysOut.String(), "AGE-SECRET-KEY-PQ-1") != 3 {
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
