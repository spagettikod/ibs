package site

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
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
	cfg := config.Config{DataDir: t.TempDir(), PeerGrace: time.Minute, CheckInterval: time.Second}
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

func TestDedupWithinBucket(t *testing.T) {
	s := newTestSite(t)
	ctx := context.Background()
	s.CreateBucket("dedup")
	data := bytes.Repeat([]byte("x"), 1000)
	s.PutObject(ctx, "dedup", "one", bytes.NewReader(data), "", nil)
	s.PutObject(ctx, "dedup", "two", bytes.NewReader(data), "", nil)
	var blocks, refs int
	s.DB.QueryRow(`SELECT COUNT(*), SUM(refs) FROM blocks`).Scan(&blocks, &refs)
	if blocks != 1 || refs != 2 {
		t.Fatalf("expected 1 shared block with 2 refs, got %d blocks %d refs", blocks, refs)
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
	key := keys.RandomKey()
	id, ct := blockcodec.Encrypt(key, []byte("secret"))
	ct[len(ct)-1] ^= 1
	if _, err := blockcodec.Decrypt(key, id, ct); err != blockcodec.ErrCorrupt {
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
	// Removing the limit makes the whole disk available again.
	s.cfg.StorageLimit = 0
	s.refreshLocalNode(ctx)
	nodes, _ = s.Nodes()
	if nodes[0].Capacity != nodes[0].DiskCapacity {
		t.Fatalf("capacity should be the disk size without a limit: %+v", nodes[0])
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
	if nodes, _ = s.Nodes(); nodes[0].Quota != 0 || nodes[0].Capacity != nodes[0].DiskCapacity {
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
	cfg := config.Config{DataDir: t.TempDir(), PeerGrace: time.Minute, CheckInterval: time.Second}
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
	c := s.StorableCapacity()
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
			id, ct := blockcodec.Encrypt(keys.RandomKey(), data)
			if s.storeBlock(ctx, "quota", id, ct) == nil {
				stored.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := stored.Load(); n > 5 || n == 0 {
		t.Fatalf("stored %d blocks of 1 MiB within a 5 MiB limit", n)
	}
}
