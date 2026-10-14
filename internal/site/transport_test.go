package site

import (
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
