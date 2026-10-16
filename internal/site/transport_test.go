package site

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A peer reached through a relay is tried at its direct address again now
// and then, instead of staying on the relay for as long as it works.
func TestDirectAddressIsRetried(t *testing.T) {
	pc := &peerClient{routes: Routes{Address: "peer.lan:7443"}, active: PathRelay + ":r1"}
	paths := func() []peerPath {
		return []peerPath{{kind: PathDirect}, {kind: PathRelay, relay: "r1"}}
	}
	first := func(now time.Time) string { return pc.order(paths(), now)[0].key() }

	start := time.Now()
	if got := first(start); got != PathDirect {
		t.Fatalf("first try after the relay worked: %s, want the direct address", got)
	}
	if got := first(start.Add(10 * time.Second)); got != PathRelay+":r1" {
		t.Fatalf("right after that: %s, want the relay that works", got)
	}
	if got := first(start.Add(directRetryEvery + time.Second)); got != PathDirect {
		t.Fatalf("a minute later: %s, want the direct address again", got)
	}

	// Once the direct address works, it stays first.
	pc.active = PathDirect
	if got := first(start.Add(2 * time.Minute)); got != PathDirect {
		t.Fatalf("after the direct address worked: %s", got)
	}
	// Without a direct address, the relay that works stays first.
	pc = &peerClient{active: PathRelay + ":r1"}
	if got := pc.order([]peerPath{{kind: PathRelay, relay: "r2"}, {kind: PathRelay, relay: "r1"}}, start)[0].key(); got != PathRelay+":r1" {
		t.Fatalf("relay order: %s", got)
	}
}

// The host the web interface is opened with is offered as a guess at the
// site's LAN address; local-only names are not.
func TestWebHostGuess(t *testing.T) {
	s := newTestSite(t)
	for _, h := range []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080", "ibs.localhost:8080", "[fe80::1]:8080", ""} {
		s.NoteWebHost(h)
		if g := s.WebHost(); g != "" {
			t.Fatalf("%q was taken as a LAN address: %q", h, g)
		}
	}
	s.NoteWebHost("192.168.1.20:8080")
	if g := s.WebHost(); g != "192.168.1.20" {
		t.Fatalf("guess %q, want 192.168.1.20", g)
	}
	s.NoteWebHost("IBS.int.Example.com")
	if g := s.WebHost(); g != "ibs.int.example.com" {
		t.Fatalf("guess %q, want the name in lower case", g)
	}
	s.NoteWebHost("localhost:8080") // a local visit doesn't replace a good guess
	if g := s.WebHost(); g != "ibs.int.example.com" {
		t.Fatalf("guess %q after a local visit", g)
	}
}

// A download keeps going while data arrives, however long it takes, and
// fails soon after data stops.
func TestStallTimeout(t *testing.T) {
	old := stallTimeout
	stallTimeout = 300 * time.Millisecond
	defer func() { stallTimeout = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunks := 10 // over a second in all: longer than stallTimeout
		if r.URL.Query().Get("stop") != "" {
			chunks = 1
		}
		for i := 0; i < chunks; i++ {
			w.Write([]byte("data"))
			w.(http.Flusher).Flush()
			time.Sleep(120 * time.Millisecond)
		}
		if chunks == 1 {
			select { // stops sending, until the client gives up
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}
	}))
	defer srv.Close()
	get := func(url string) error {
		ctx, watch, cancel := withStall(context.Background())
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return stalled(ctx, err)
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(touchReader{resp.Body, watch})
		return stalled(ctx, err)
	}
	if err := get(srv.URL + "/peer/v1/backup"); err != nil {
		t.Fatalf("a slow but steady download failed: %v", err)
	}
	start := time.Now()
	if err := get(srv.URL + "/peer/v1/backup?stop=1"); !errors.Is(err, errStalled) {
		t.Fatalf("a download that stopped: %v, want errStalled", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v to notice", d)
	}
}
