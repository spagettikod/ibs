package site

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"filippo.io/age"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ibs/internal/blockcodec"
	"ibs/internal/blockstore"
	"ibs/internal/config"
	"ibs/internal/keys"
)

func newTestSite(t *testing.T) *Site {
	t.Helper()
	cfg := config.Config{DataDir: t.TempDir(), PeerGrace: time.Minute, NodeGrace: time.Minute, CheckInterval: time.Second, RoomCooldown: time.Hour, RebuildRate: 1 << 30, CleanupRate: 1 << 30}
	s, phrase, err := Create(cfg, "test", "password123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.DecodePhrase(phrase); err != nil {
		t.Fatalf("phrase does not round trip: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPhraseRoundTrip(t *testing.T) {
	seed := keys.NewSeed()
	p := keys.EncodePhrase(seed)
	got, err := keys.DecodePhrase(strings.ToLower(p))
	if err != nil || !bytes.Equal(got, seed) {
		t.Fatalf("round trip failed: %v", err)
	}
	broken := []byte(p)
	broken[3] ^= 1
	if _, err := keys.DecodePhrase(string(broken)); err == nil {
		t.Fatal("expected checksum failure")
	}
}

func TestPutGetOverwriteDelete(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	if err := s.CreateBucket("data"); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, blockcodec.BlockSize*2+123)
	rand.Read(payload)
	if _, err := s.PutObject(ctx, "data", "a/b.bin", bytes.NewReader(payload), "", nil); err != nil {
		t.Fatal(err)
	}
	o, err := s.HeadObject("data", "a/b.bin")
	if err != nil || o.Size != int64(len(payload)) || len(o.Blocks) != 3 {
		t.Fatalf("head: %+v %v", o, err)
	}
	var out bytes.Buffer
	if err := s.WriteObject(ctx, &out, o, 0, o.Size); err != nil || !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("read back mismatch: %v", err)
	}
	out.Reset()
	start := int64(blockcodec.BlockSize - 10)
	if err := s.WriteObject(ctx, &out, o, start, 20); err != nil || !bytes.Equal(out.Bytes(), payload[start:start+20]) {
		t.Fatalf("range across blocks mismatch: %v", err)
	}

	// Overwrite releases the old blocks.
	if _, err := s.PutObject(ctx, "data", "a/b.bin", strings.NewReader("small"), "", nil); err != nil {
		t.Fatal(err)
	}
	var unreferenced int
	s.DB.QueryRow(`SELECT COUNT(*) FROM blocks WHERE refs <= 0`).Scan(&unreferenced)
	if unreferenced != 3 {
		t.Fatalf("expected 3 unreferenced blocks after overwrite, got %d", unreferenced)
	}
	if err := s.DeleteBucket("data"); err != ErrBucketNotEmpty {
		t.Fatalf("expected bucket not empty, got %v", err)
	}
	if err := s.DeleteObject("data", "a/b.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HeadObject("data", "a/b.bin"); err != ErrNoSuchKey {
		t.Fatalf("expected no such key, got %v", err)
	}
	if err := s.DeleteBucket("data"); err != nil {
		t.Fatal(err)
	}
}

// Each file owns its blocks: identical data doesn't share blocks, and a copy
// gets new ones. Deleting a file frees its blocks at once.
func TestFilesOwnTheirBlocks(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("owned")
	data := bytes.Repeat([]byte("x"), 1000)
	s.PutObject(ctx, "owned", "one", bytes.NewReader(data), "", nil)
	s.PutObject(ctx, "owned", "two", bytes.NewReader(data), "", nil)
	if _, err := s.CopyObject(ctx, "owned", "one", "owned", "three", false, "", nil); err != nil {
		t.Fatal(err)
	}
	var blocks, refs int
	s.DB.QueryRow(`SELECT COUNT(*), SUM(refs) FROM blocks`).Scan(&blocks, &refs)
	if blocks != 3 || refs != 3 {
		t.Fatalf("%d blocks, %d refs; want 3 blocks of one file each", blocks, refs)
	}
	var out bytes.Buffer
	o, _ := s.HeadObject("owned", "three")
	if err := s.WriteObject(ctx, &out, o, 0, o.Size); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("copy reads back wrong: %v", err)
	}
	s.DeleteObject("owned", "one")
	s.collectGarbage(ctx)
	s.DB.QueryRow(`SELECT COUNT(*) FROM blocks`).Scan(&blocks)
	if blocks != 2 {
		t.Fatalf("%d blocks after deleting a file and collecting; want 2, at once", blocks)
	}
}

// Blocks of an upload in progress are kept however long it takes; an upload
// whose block went missing fails instead of storing a broken file; a restart
// releases blocks of uploads that were in progress.
func TestUploadsOwnTheirBlocks(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("uploads")
	// An upload that is still sending: its first block was written long ago.
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := s.PutObject(ctx, "uploads", "slow", pr, "", nil)
		pr.CloseWithError(err)
		done <- err
	}()
	if _, err := pw.Write(bytes.Repeat([]byte("a"), blockcodec.BlockSize)); err != nil {
		t.Fatalf("upload stopped: %v", <-done)
	}
	for {
		var n int
		s.DB.QueryRow(`SELECT COUNT(*) FROM blocks WHERE upload != ''`).Scan(&n)
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.DB.Exec(`UPDATE blocks SET touched = 0`) // as if written an hour ago
	s.collectGarbage(ctx)
	pw.Write([]byte("end"))
	pw.Close()
	if err := <-done; err != nil {
		t.Fatalf("a long upload failed: %v", err)
	}
	o, _ := s.HeadObject("uploads", "slow")
	if err := s.WriteObject(ctx, io.Discard, o, 0, o.Size); err != nil {
		t.Fatalf("a long upload lost a block: %v", err)
	}

	// A block gone before the commit: the upload fails, nothing is stored.
	wr, err := s.writeBlocks(ctx, "uploads", strings.NewReader("doomed"), 0)
	if err != nil {
		t.Fatal(err)
	}
	s.DB.Exec(`DELETE FROM blocks WHERE id = ?`, wr.blocks[0].ID)
	err = s.inTx(func(tx *sql.Tx) error {
		return s.commitObject(tx, &ObjectInfo{Bucket: "uploads", Key: "doomed", Blocks: wr.blocks, Modified: time.Now()})
	})
	if !errors.Is(err, errBlockGone) {
		t.Fatalf("commit with a missing block: %v", err)
	}
	if _, err := s.HeadObject("uploads", "doomed"); err != ErrNoSuchKey {
		t.Fatalf("a broken file was stored: %v", err)
	}

	// Blocks of an upload interrupted by a restart are released and collected.
	wr, _ = s.writeBlocks(ctx, "uploads", strings.NewReader("interrupted"), 0)
	s.Close()
	s2, err := Open(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.collectGarbage(ctx)
	var n int
	s2.DB.QueryRow(`SELECT COUNT(*) FROM blocks WHERE id = ?`, wr.blocks[0].ID).Scan(&n)
	if n != 0 {
		t.Fatal("the block of an interrupted upload is still there after a restart")
	}
}

// An unfinished multipart upload is removed an hour after anything last
// arrived for it, and not while a part is still arriving.
func TestStaleUploads(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("multipart")
	old, _ := s.CreateUpload("multipart", "old", "", nil)
	s.UploadPart(ctx, "multipart", "old", old, 1, strings.NewReader("part"))
	recent, _ := s.CreateUpload("multipart", "recent", "", nil)
	s.UploadPart(ctx, "multipart", "recent", recent, 1, strings.NewReader("part"))
	day := time.Now().Add(-2 * time.Hour)
	if _, err := s.DB.Exec(`UPDATE uploads SET created = ?`, day.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE parts SET modified = ? WHERE upload_id = ?`, day.UnixMilli(), old); err != nil {
		t.Fatal(err)
	}
	// A part of the old upload is still arriving: it stays.
	s.uploadActive(old, 1)
	s.collectGarbage(ctx)
	if ups, _ := s.ListUploads("multipart", ""); len(ups) != 2 {
		t.Fatalf("%d uploads left; one receiving a part must stay", len(ups))
	}
	s.uploadActive(old, -1)
	s.collectGarbage(ctx)
	ups, _ := s.ListUploads("multipart", "")
	if len(ups) != 1 || ups[0].ID != recent {
		t.Fatalf("uploads left: %+v; want only the one with a recent part", ups)
	}
	var blocks int
	s.DB.QueryRow(`SELECT COUNT(*) FROM blocks`).Scan(&blocks)
	if blocks != 1 {
		t.Fatalf("%d blocks; the stale upload's part should be gone", blocks)
	}
}

func TestListObjectsDelimiterAndPaging(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("list")
	keys := []string{"a.txt", "dir/1", "dir/2", "dir/sub/3", "other/x", "z.txt"}
	for _, k := range keys {
		s.PutObject(ctx, "list", k, strings.NewReader(k), "", nil)
	}
	res, err := s.ListObjects("list", "", "/", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(names(res.Objects), res.CommonPrefixes)
	if got != "[a.txt z.txt] [dir/ other/]" {
		t.Fatalf("got %s", got)
	}
	res, _ = s.ListObjects("list", "dir/", "/", "", 1000)
	if got := fmt.Sprint(names(res.Objects), res.CommonPrefixes); got != "[dir/1 dir/2] [dir/sub/]" {
		t.Fatalf("got %s", got)
	}
	// Page through with max-keys 2 and make sure every entry appears once.
	var all []string
	marker := ""
	for {
		res, err := s.ListObjects("list", "", "/", marker, 2)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, names(res.Objects)...)
		all = append(all, res.CommonPrefixes...)
		if !res.Truncated {
			break
		}
		marker = res.NextMarker
	}
	sort.Strings(all)
	if fmt.Sprint(all) != "[a.txt dir/ other/ z.txt]" {
		t.Fatalf("paging got %v", all)
	}
}

func TestMultipart(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("mpu")
	id, err := s.CreateUpload("mpu", "big", "text/plain", map[string]string{"k": "v"})
	if err != nil {
		t.Fatal(err)
	}
	e1, _ := s.UploadPart(ctx, "mpu", "big", id, 1, strings.NewReader("hello "))
	e2, _ := s.UploadPart(ctx, "mpu", "big", id, 2, strings.NewReader("world"))
	if _, err := s.CompleteUpload("mpu", "big", id, []CompletePart{{2, e2}, {1, e1}}); err != ErrInvalidPartOrder {
		t.Fatalf("expected part order error, got %v", err)
	}
	o, err := s.CompleteUpload("mpu", "big", id, []CompletePart{{1, e1}, {2, e2}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(o.ETag, `-2"`) || o.Meta["k"] != "v" {
		t.Fatalf("unexpected object %+v", o)
	}
	var out bytes.Buffer
	s.WriteObject(ctx, &out, o, 0, o.Size)
	if out.String() != "hello world" {
		t.Fatalf("got %q", out.String())
	}
	var parts int
	s.DB.QueryRow(`SELECT COUNT(*) FROM parts`).Scan(&parts)
	if parts != 0 {
		t.Fatal("parts not cleaned up")
	}
}

func TestBlockCorruptionDetected(t *testing.T) {
	key, _ := age.GenerateHybridIdentity()
	id, ct, _ := blockcodec.EncryptAge(key.Recipient(), []byte("secret"))
	ct[len(ct)-1] ^= 1
	if _, err := blockcodec.DecryptAge(key, id, ct); err != blockcodec.ErrCorrupt {
		t.Fatalf("expected corruption error, got %v", err)
	}
}

func names(objs []ObjectInfo) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o.Key)
	}
	return out
}

func TestNodeQuota(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("quota")
	s.cfg.StorageLimit = 1 << 20 // IBS_STORAGE_LIMIT=1M on the manager
	s.refreshLocalNode(ctx)
	big := make([]byte, 2<<20)
	rand.Read(big)
	if _, err := s.PutObject(ctx, "quota", "big", bytes.NewReader(big), "", nil); err == nil {
		t.Fatal("expected a write beyond the node limit to fail")
	}
	small := make([]byte, 600<<10)
	rand.Read(small)
	if _, err := s.PutObject(ctx, "quota", "small", bytes.NewReader(small), "", nil); err != nil {
		t.Fatalf("write within the limit failed: %v", err)
	}
	s.refreshLocalNode(ctx)
	nodes, _ := s.Nodes()
	n := nodes[0]
	if n.Quota != 1<<20 || n.Capacity != 1<<20 || n.Used < 600<<10 || n.Free > (1<<20)-(600<<10) {
		t.Fatalf("unexpected node figures: %+v", n)
	}
	// Without a limit IBS may use what it stores plus the disk's free space
	// (not the disk's size: the disk may hold other things).
	s.cfg.StorageLimit = 0
	s.refreshLocalNode(ctx)
	nodes, _ = s.Nodes()
	if nodes[0].Capacity != nodes[0].Used+nodes[0].DiskFree || nodes[0].Capacity >= nodes[0].DiskCapacity {
		t.Fatalf("capacity should be what IBS stores plus the free space: %+v", nodes[0])
	}
}

func TestReplicable(t *testing.T) {
	cases := []struct {
		free []int64
		r    int
		want int64
	}{
		{[]int64{100, 100}, 1, 200},
		{[]int64{100, 100}, 2, 100},
		{[]int64{1000, 10}, 2, 10},     // the small site limits two-copy data
		{[]int64{1000, 10, 10}, 2, 20}, // the big site pairs with either small one
		{[]int64{30, 30, 30}, 2, 45},   // copies spread over all three
		{[]int64{100}, 2, 0},
	}
	for _, c := range cases {
		if got := replicable(c.free, c.r); got != c.want {
			t.Errorf("replicable(%v, %d) = %d, want %d", c.free, c.r, got, c.want)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "1024": 1024, "500G": 500 << 30, "1.5T": 3 << 39, "750GiB": 750 << 30, "10m": 10 << 20, "2TB": 2 << 40} {
		got, err := config.ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"lots", "-5G", "5X"} {
		if _, err := config.ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) should fail", bad)
		}
	}
}

func TestManagerSharingNothing(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("none")
	s.cfg.StorageLimit = config.NoStorage // IBS_STORAGE_LIMIT=0
	s.refreshLocalNode(ctx)
	if _, err := s.PutObject(ctx, "none", "f", strings.NewReader("data"), "", nil); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("a manager sharing nothing must not store blocks, got %v", err)
	}
	nodes, _ := s.Nodes()
	if nodes[0].Capacity != 0 || nodes[0].Free != 0 || nodes[0].Quota != config.NoStorage {
		t.Fatalf("unexpected manager node: %+v", nodes[0])
	}
}

func TestDrainManagerSharingNothing(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("drain")
	data := make([]byte, blockcodec.BlockSize+1000)
	rand.Read(data)
	if _, err := s.PutObject(ctx, "drain", "f", bytes.NewReader(data), "", nil); err != nil {
		t.Fatal(err)
	}

	// Add a second storage node, then make the manager share nothing.
	node2 := &blockstore.FS{Dir: t.TempDir()}
	s.nodesMu.Lock()
	s.nodes["node2"] = node2
	s.nodesMu.Unlock()
	s.DB.Exec(`INSERT INTO nodes(id, name, address, secret, last_seen) VALUES('node2', 'node2', 'test', '', ?)`, time.Now().Unix())
	c, f, _ := node2.Stat(ctx)
	s.updateNodeStorage("node2", 0, c, f)
	s.cfg.StorageLimit = config.NoStorage
	s.refreshLocalNode(ctx)

	s.drainOnce(ctx)
	var onManager, onNode2 int
	s.DB.QueryRow(`SELECT COUNT(*) FROM blocks WHERE node_id = ?`, LocalNodeID).Scan(&onManager)
	s.DB.QueryRow(`SELECT COUNT(*) FROM blocks WHERE node_id = 'node2'`).Scan(&onNode2)
	if onManager != 0 || onNode2 != 2 {
		t.Fatalf("expected all blocks moved to node2, manager %d node2 %d", onManager, onNode2)
	}
	o, _ := s.HeadObject("drain", "f")
	var out bytes.Buffer
	if err := s.WriteObject(ctx, &out, o, 0, o.Size); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("object unreadable after the move: %v", err)
	}
	if ids, _ := s.nodeStore(LocalNodeID).List(ctx, blockstore.OwnNS); len(ids) != 0 {
		t.Fatalf("manager still has %d block files", len(ids))
	}
}

func TestRecoveryPhraseAndPasswordReset(t *testing.T) {
	s := newTestSite(t)
	dir := s.cfg.DataDir
	phrase, err := RecoveryPhrase(dir)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := keys.DecodePhrase(phrase)
	if err != nil || !bytes.Equal(seed, s.ID.Seed) {
		t.Fatalf("phrase does not match the site seed: %v", err)
	}
	if err := ResetPassword(dir, "short"); err == nil {
		t.Fatal("short password accepted")
	}
	if err := ResetPassword(dir, "new-password-123"); err != nil {
		t.Fatal(err)
	}
	if !s.CheckAdminPassword("new-password-123") || s.CheckAdminPassword("password123") {
		t.Fatal("password was not reset")
	}
}

func TestNodeLimitFromWebInterface(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.refreshLocalNode(ctx)

	// Set in the web interface: applies now and after the next refresh.
	if err := s.SetNodeLimit(LocalNodeID, 1<<20); err != nil {
		t.Fatal(err)
	}
	s.refreshLocalNode(ctx)
	nodes, _ := s.Nodes()
	if n := nodes[0]; n.Quota != 1<<20 || n.Capacity != 1<<20 || n.LimitFromEnv {
		t.Fatalf("web limit not applied: %+v", n)
	}
	// Nothing: the node stores nothing.
	if err := s.SetNodeLimit(LocalNodeID, config.NoStorage); err != nil {
		t.Fatal(err)
	}
	if nodes, _ = s.Nodes(); nodes[0].Quota != config.NoStorage || nodes[0].Free != 0 {
		t.Fatalf("'nothing' not applied: %+v", nodes[0])
	}
	// Back to the whole disk.
	if err := s.SetNodeLimit(LocalNodeID, 0); err != nil {
		t.Fatal(err)
	}
	if nodes, _ = s.Nodes(); nodes[0].Quota != 0 || nodes[0].Capacity != nodes[0].Used+nodes[0].DiskFree {
		t.Fatalf("whole disk not restored: %+v", nodes[0])
	}

	// IBS_STORAGE_LIMIT takes precedence, and the web interface can't change it.
	s.SetNodeLimit(LocalNodeID, 5<<20)
	s.cfg.StorageLimit = 2 << 20
	s.refreshLocalNode(ctx)
	if nodes, _ = s.Nodes(); nodes[0].Quota != 2<<20 || !nodes[0].LimitFromEnv {
		t.Fatalf("variable should win: %+v", nodes[0])
	}
	if err := s.SetNodeLimit(LocalNodeID, 1<<20); err == nil {
		t.Fatal("expected a refusal while IBS_STORAGE_LIMIT is set")
	}
	// Without the variable, the web limit applies again.
	s.cfg.StorageLimit = 0
	s.refreshLocalNode(ctx)
	if nodes, _ = s.Nodes(); nodes[0].Quota != 5<<20 || nodes[0].LimitFromEnv {
		t.Fatalf("web limit should apply again: %+v", nodes[0])
	}
}

func TestEveryFileHasTwoCopiesElsewhere(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir(), PeerGrace: time.Minute, NodeGrace: time.Minute, CheckInterval: time.Second, RoomCooldown: time.Hour, RebuildRate: 1 << 30, CleanupRate: 1 << 30}
	s, _, err := Create(cfg, "test", "password123")
	if err != nil {
		t.Fatal(err)
	}
	s.CreateBucket("new")
	s.CreateBucket("old")
	// A bucket from before copies were fixed, with its own count.
	s.DB.Exec(`UPDATE buckets SET replication = 0 WHERE name = 'old'`)
	s.Close()

	s, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bs, _ := s.ListBuckets()
	for _, b := range bs {
		if b.Replication != Copies {
			t.Errorf("bucket %s keeps %d copies elsewhere, want %d", b.Name, b.Replication, Copies)
		}
	}
	// Without other sites, files are stored here and the copies follow later.
	c := s.StorableCapacity(Copies + 1)
	if c.Copies != 0 || c.LimitedBy != "sites" || c.Storable != c.Local {
		t.Errorf("capacity without other sites: %+v", c)
	}
	if h := s.IBSCapacity(s.ReplicationHealth()); h.Copies != Copies || h.Fits {
		t.Errorf("IBS capacity without other sites should not fit: %+v", h)
	}
}

// Blocks written at the same time, like the parts of a large upload, can't
// together go past a node's storage limit.
func TestNodeQuotaConcurrentWrites(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("quota")
	s.cfg.StorageLimit = 5 << 20
	s.refreshLocalNode(ctx)
	var wg sync.WaitGroup
	var stored atomic.Int64
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := make([]byte, 1<<20)
			rand.Read(data)
			id, ct := testBlock(data)
			if s.storeBlock(ctx, "quota", "up", id, ct) == nil {
				stored.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := stored.Load(); n > 5 || n == 0 {
		t.Fatalf("stored %d blocks of 1 MiB within a 5 MiB limit", n)
	}
}

// Other sites' copies may use all free space but a small margin, and none
// for a while after the site had to make room for its own files.
func TestOfferedStorage(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.cfg.StorageLimit = 100 << 20
	s.refreshLocalNode(ctx)
	capacity, free := s.offeredStorage()
	if capacity != 100<<20 || free != 95<<20 {
		t.Fatalf("offered %d, %d free; want 95 MiB of 100", capacity, free)
	}
	s.SetSetting("room_made_at", itoa(time.Now().Unix()))
	if _, free := s.offeredStorage(); free != 0 {
		t.Fatalf("offered %d right after making room", free)
	}
}

// A site gives back an owner's copy only when the block keeps its original
// and a copy on another online site.
func TestReleaseCopies(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("room")
	data := make([]byte, 3*blockcodec.BlockSize)
	rand.Read(data)
	if _, err := s.PutObject(ctx, "room", "f", bytes.NewReader(data), "", nil); err != nil {
		t.Fatal(err)
	}
	var ids []string
	rows, _ := s.DB.Query(`SELECT id FROM blocks ORDER BY id`)
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 3 {
		t.Fatalf("%d blocks", len(ids))
	}
	now := time.Now().Unix()
	for _, p := range []struct {
		id     string
		online int
	}{{"holder-site", 1}, {"other-site", 1}, {"away-site", 0}} {
		s.DB.Exec(`INSERT INTO peers(site_id, name, pubkey, address, added, last_seen, online, free) VALUES(?, ?, x'', '', ?, ?, ?, 0)`,
			p.id, p.id, now, now, p.online)
	}
	// Block 0: also on an online site. Block 1: also on an offline one. Block 2: only on the holder.
	for _, r := range [][2]string{{ids[0], "holder-site"}, {ids[0], "other-site"}, {ids[1], "holder-site"}, {ids[1], "away-site"}, {ids[2], "holder-site"}} {
		s.DB.Exec(`INSERT INTO replicas(block_id, site_id, state, verified) VALUES(?, ?, 'ok', ?)`, r[0], r[1], now)
	}
	got := s.releaseCopies(ctx, "holder-site", append(ids, "gone"), 1<<40)
	if len(got) != 2 || got[0] != ids[0] || got[1] != "gone" {
		t.Fatalf("released %v; want the block with another online copy and the unknown one", got)
	}
	var st string
	s.DB.QueryRow(`SELECT state FROM replicas WHERE block_id = ? AND site_id = 'holder-site'`, ids[0]).Scan(&st)
	if st != "delete" {
		t.Fatalf("released copy is %q", st)
	}
	// It stops once enough is released.
	s.DB.Exec(`UPDATE replicas SET state = 'ok'`)
	s.DB.Exec(`INSERT INTO replicas(block_id, site_id, state, verified) VALUES(?, 'other-site', 'ok', ?)`, ids[2], now)
	if got := s.releaseCopies(ctx, "holder-site", ids, 1); len(got) != 1 {
		t.Fatalf("released %d for 1 byte", len(got))
	}
}

// Blocks every online site already holds don't fill the replication batches,
// and each batch goes on after the previous one, so all blocks get their turn.
func TestReplicationBatches(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("many")
	for i := range replicateBatch + 44 {
		data := make([]byte, 1000)
		rand.Read(data)
		if _, err := s.PutObject(ctx, "many", fmt.Sprint(i), bytes.NewReader(data), "", nil); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Unix()
	s.DB.Exec(`INSERT INTO peers(site_id, name, pubkey, address, added, last_seen, online) VALUES('only-peer', 'only-peer', x'', '', ?, ?, 1)`, now, now)
	// Stored long ago: not new blocks waiting to be combined.
	s.DB.Exec(`UPDATE blocks SET touched = touched - 3600`)

	batch := func() []replNeed {
		needs, err := s.replicationNeeds(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return needs
	}
	if n := len(batch()); n != replicateBatch {
		t.Fatalf("first batch %d", n)
	}
	if n := len(batch()); n != 44 {
		t.Fatalf("second batch %d, want the 44 after the first", n)
	}
	if n := len(batch()); n != replicateBatch {
		t.Fatalf("third batch %d: should start over", n)
	}

	// The one other site holds the first 256 blocks: they need a second site,
	// which doesn't exist, so the other 44 come up at once.
	s.replCursor = ""
	s.DB.Exec(`INSERT INTO replicas(block_id, site_id, state, verified)
		SELECT id, 'only-peer', 'ok', ? FROM blocks ORDER BY id LIMIT ?`, now, replicateBatch)
	if n := len(batch()); n != 44 {
		t.Fatalf("batch %d, want only the 44 blocks the other site doesn't hold", n)
	}
}

// With one other site, each block can have one copy elsewhere: progress counts
// those, not the second copy that needs another site.
func TestCopyProgress(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("progress")
	for i := range 3 {
		data := make([]byte, 1000)
		rand.Read(data)
		s.PutObject(ctx, "progress", fmt.Sprint(i), bytes.NewReader(data), "", nil)
	}
	now := time.Now().Unix()
	s.DB.Exec(`INSERT INTO peers(site_id, name, pubkey, address, added, last_seen, online) VALUES('only-peer', 'only-peer', x'', '', ?, ?, 1)`, now, now)
	s.DB.Exec(`INSERT INTO replicas(block_id, site_id, state, verified) SELECT id, 'only-peer', 'ok', ? FROM blocks ORDER BY id LIMIT 1`, now)
	h := s.ReplicationHealth()
	if h.CopiesMade != 1 || h.CopiesPossible != 3 || h.Copying != 2 || h.UnderReplicated != 3 {
		t.Fatalf("made %d of %d possible, %d copying, %d under-replicated; want 1 of 3, 2, 3",
			h.CopiesMade, h.CopiesPossible, h.Copying, h.UnderReplicated)
	}
}

// Keys can be limited to reading, and to some buckets; deleting a bucket
// takes it off the keys that list it.
func TestAccessKeyPolicy(t *testing.T) {
	s := newTestSite(t)
	s.CreateBucket("photos")
	s.CreateBucket("music")
	all, _ := s.CreateAccessKey("all", AccessFull, nil)
	read, _ := s.CreateAccessKey("read", AccessRead, []string{"photos"})
	if _, err := s.CreateAccessKey("bad", "admin", nil); err == nil {
		t.Fatal("accepted an unknown access")
	}
	if _, err := s.CreateAccessKey("bad", AccessFull, []string{"nope"}); err == nil {
		t.Fatal("accepted a bucket that doesn't exist")
	}
	if _, err := s.CreateAccessKey("bad", AccessFull, []string{}); err == nil {
		t.Fatal("accepted a key for no buckets")
	}
	p, _ := s.AccessKeyPolicy(all.ID)
	if !p.CanWrite("music") || !p.CanWrite("later") || !p.CanRead("") {
		t.Fatalf("full key for all buckets: %+v", p)
	}
	p, _ = s.AccessKeyPolicy(read.ID)
	if p.CanWrite("photos") || !p.CanRead("photos") || p.CanRead("music") {
		t.Fatalf("read-only key for photos: %+v", p)
	}
	if err := s.UpdateAccessKey(read.ID, "now full", AccessFull, []string{"photos", "music"}); err != nil {
		t.Fatal(err)
	}
	if p, _ = s.AccessKeyPolicy(read.ID); !p.CanWrite("music") {
		t.Fatalf("after update: %+v", p)
	}
	s.DeleteBucket("music")
	s.CreateBucket("music") // the same name again: no access through the old key
	if p, _ = s.AccessKeyPolicy(read.ID); p.CanRead("music") || !p.CanWrite("photos") {
		t.Fatalf("after deleting a bucket: %+v", p)
	}
}

// Pages read the expensive figures from the cache, and say when they aren't
// counted yet instead of showing zeros.
func TestStatsCache(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("cached")
	s.PutObject(ctx, "cached", "f", strings.NewReader("data"), "", nil)
	s.statsRunning.Store(true) // as with statsLoop, before its first calculation
	if d, _ := s.Dashboard(); d.Counted || d.Site.Objects != 0 {
		t.Fatalf("figures before the first count: counted %v, %d objects", d.Counted, d.Site.Objects)
	}
	if b, _ := s.ListBuckets(); b[0].Counted {
		t.Fatal("bucket counted before the first count")
	}
	s.refreshStats()
	d, _ := s.Dashboard()
	b, _ := s.ListBuckets()
	n, _ := s.Nodes()
	if !d.Counted || d.Site.Objects != 1 || d.Replication.Blocks != 1 || !b[0].Counted || b[0].Objects != 1 || n[0].Blocks != 1 {
		t.Fatalf("figures from the cache: dashboard %+v %+v, bucket %+v, node %+v", d.Site, d.Replication, b[0], n[0])
	}
}

// Deleting a folder deletes everything under it, and nothing beside it.
func TestDeleteFolder(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("folders")
	for _, k := range []string{"a/x", "a/b/y", "a.txt", "ab/z", "other"} {
		s.PutObject(ctx, "folders", k, strings.NewReader(k), "", nil)
	}
	if n, b, err := s.FolderSize("folders", "a/"); err != nil || n != 2 || b != int64(len("a/x")+len("a/b/y")) {
		t.Fatalf("folder size: %d files, %d bytes, %v", n, b, err)
	}
	if _, err := s.DeleteFolder("folders", "a"); err == nil {
		t.Fatal("deleted by a prefix that isn't a folder")
	}
	if n, err := s.DeleteFolder("folders", "a/"); err != nil || n != 2 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	res, _ := s.ListObjects("folders", "", "", "", 100)
	var keys []string
	for _, o := range res.Objects {
		keys = append(keys, o.Key)
	}
	if strings.Join(keys, ",") != "a.txt,ab/z,other" {
		t.Fatalf("left: %v", keys)
	}
}

// A folder is counted when first shown, and from then on follows every file
// stored and deleted; the running figures match a recount.
func TestFolderFigures(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("tree")
	put := func(k, v string) { s.PutObject(ctx, "tree", k, strings.NewReader(v), "", nil) }
	counted := func() {
		for {
			select {
			case f := <-s.folderQueue:
				s.countFolder(f[0], f[1])
			default:
				return
			}
		}
	}
	put("a/x", "12345")
	put("a/b/y", "123")
	if f := s.Folders("tree", []string{"a/", "a/b/"}); len(f) != 0 {
		t.Fatalf("figures before counting: %+v", f)
	}
	counted()
	put("a/b/z", "1")
	put("top", "1234567")
	put("a/x", "12") // replaced: smaller
	if err := s.DeleteObject("tree", "a/b/z"); err != nil {
		t.Fatal(err)
	}
	s.refreshFolderLatest()
	f := s.Folders("tree", []string{"a/", "a/b/"})
	if f["a/"].Objects != 2 || f["a/"].Bytes != 5 || f["a/b/"].Objects != 1 || f["a/b/"].Bytes != 3 || f["a/"].Latest <= 0 {
		t.Fatalf("figures: %+v", f)
	}
	// The same as counting from scratch.
	s.DB.Exec(`DELETE FROM folders`)
	s.Folders("tree", []string{"a/", "a/b/"})
	counted()
	if again := s.Folders("tree", []string{"a/", "a/b/"}); again["a/"] != f["a/"] || again["a/b/"] != f["a/b/"] {
		t.Fatalf("running %+v, recounted %+v", f, again)
	}
	// A deleted folder is gone.
	s.DeleteFolder("tree", "a/")
	if f := s.Folders("tree", []string{"a/", "a/b/"}); len(f) != 0 {
		t.Fatalf("figures of a deleted folder: %+v", f)
	}
}

// Browsing sorts the whole folder, files a page at a time; subfolders come
// first, sorted by their figures.
func TestBrowse(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("browse")
	for _, f := range []struct{ k, v string }{
		{"d/small", "1"}, {"d/large", "123456789"}, {"d/mid", "12345"},
		{"d/sub1/x", "12"}, {"d/sub2/y", "1234567890123"}, {"d/sub2/z", "1"}, {"top", "x"},
	} {
		s.PutObject(ctx, "browse", f.k, strings.NewReader(f.v), "", nil)
	}
	l, err := s.Browse("browse", "d/", "size", true, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if l.Files != 3 || len(l.Objects) != 2 || l.Objects[0].Key != "d/large" || l.Objects[1].Key != "d/mid" || l.Next != 2 {
		t.Fatalf("first page by size: %d files, next %d, %+v", l.Files, l.Next, l.Objects)
	}
	if len(l.Folders) != 2 || l.Folders[0].Counted || l.Folders[1].Counted {
		t.Fatalf("folders before counting: %+v", l.Folders)
	}
	for {
		select {
		case f := <-s.folderQueue:
			s.countFolder(f[0], f[1])
			continue
		default:
		}
		break
	}
	l, _ = s.Browse("browse", "d/", "size", true, 0, 2)
	if l.Folders[0].Prefix != "d/sub2/" || !l.Folders[0].Counted || l.Folders[0].Bytes != 14 {
		t.Fatalf("folders by size: %+v", l.Folders)
	}
	next, _ := s.Browse("browse", "d/", "size", true, 2, 2)
	if len(next.Objects) != 1 || next.Objects[0].Key != "d/small" || next.Next != 0 || len(next.Folders) != 0 {
		t.Fatalf("second page: %+v", next)
	}
	root, _ := s.Browse("browse", "", "name", false, 0, 10)
	if len(root.Folders) != 1 || root.Folders[0].Prefix != "d/" || root.Files != 1 {
		t.Fatalf("root: %+v", root)
	}
}

// After a restore, blocks still to be fetched back are counted, and the room
// for files is what's left once they are here.
func TestRestoringCountsAsUsed(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("back")
	s.SetNodeLimit(LocalNodeID, 1<<20) // so the room depends on what IBS holds, not the disk
	s.PutObject(ctx, "back", "f", strings.NewReader(strings.Repeat("x", 5000)), "", nil)
	s.refreshStats()
	before := s.StorableCapacity(Copies + 1).Local
	var size int64
	s.DB.QueryRow(`SELECT SUM(size) FROM blocks`).Scan(&size)
	s.DB.Exec(`UPDATE blocks SET node_id = ''`) // as Restore leaves them
	s.refreshStats()
	h := s.stats().Health
	if h.MissingLocally != 1 || h.MissingBytes != size {
		t.Fatalf("missing: %d blocks, %d bytes; want 1, %d", h.MissingLocally, h.MissingBytes, size)
	}
	// The room counts the block as used while it is still to come back. (The
	// node's free space isn't refreshed here; on a real restore it is higher
	// by the blocks not back yet, which this takes off again.)
	if got := s.StorableCapacity(Copies + 1).Local; got != before-size {
		t.Fatalf("room = %d; want %d, less the %d bytes still to come back", got, before-size, size)
	}
}
