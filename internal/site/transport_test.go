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
