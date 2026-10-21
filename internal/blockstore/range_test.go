package blockstore

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Part of a block, from the disk, from a node, and from a node from before
// range reads (which sends the whole block).
func TestGetRange(t *testing.T) {
	ctx := context.Background()
	fs := &FS{Dir: t.TempDir()}
	id := "ab" + string(bytes.Repeat([]byte("c"), 62))
	data := []byte("0123456789abcdefghij")
	if err := fs.Put(ctx, OwnNS, id, data); err != nil {
		t.Fatal(err)
	}
	want := data[5:12]
	check := func(name string, st Store) {
		t.Helper()
		got, err := GetRange(ctx, st, OwnNS, id, 5, 7)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %q, %v; want %q", name, got, err, want)
		}
	}
	check("disk", fs)

	node := httptest.NewServer(Handler(fs, nil))
	defer node.Close()
	check("node", NewRemote(node.URL, node.Client()))

	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.Get(r.Context(), OwnNS, id) // ignores Range, like nodes before range reads
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer old.Close()
	check("older node", NewRemote(old.URL, old.Client()))

	if _, err := GetRange(ctx, fs, OwnNS, id, 15, 10); err == nil {
		t.Fatal("read beyond the block")
	}
}
