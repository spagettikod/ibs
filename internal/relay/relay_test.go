package relay

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"ibs/internal/keys"
)

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func site(t *testing.T) (*keys.Identity, tls.Certificate) {
	id, err := keys.FromSeed(keys.NewSeed())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := id.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	return id, cert
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRelaySplicesEndToEndTLS(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	srv, err := New(t.TempDir(), addr, "")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Run(ctx, addr)
	tok, _ := ParseToken(srv.Token())
	tok.Address = addr

	idA, certA := site(t)
	idB, certB := site(t)

	// B answers relayed connections with TLS that only admits A's key.
	b := &Client{Token: tok, Cert: certB, Name: "b", OnIncoming: func(conn net.Conn, from string) {
		if from != idA.SiteID {
			conn.Close()
			return
		}
		tc := tls.Server(conn, keys.ServerTLS(certB, idA.Pub))
		defer tc.Close()
		io.Copy(tc, tc) // echo
	}}
	a := &Client{Token: tok, Cert: certA, Name: "a"}
	go b.Run(ctx)
	go a.Run(ctx)
	waitFor(t, "clients to connect", func() bool { return a.Connected() && b.Connected() })

	conn, err := a.Dial(ctx, idB.SiteID)
	if err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(conn, keys.ClientTLS(certA, idB.Pub))
	defer tc.Close()
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	msg := strings.Repeat("through the relay ", 1000)
	go tc.Write([]byte(msg))
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(tc, got); err != nil || string(got) != msg {
		t.Fatalf("echo failed: %v", err)
	}

	// Unknown target and unregistered sites are refused.
	if _, err := a.Dial(ctx, "0000000000000000"); err == nil {
		t.Fatal("dial to an unknown site should fail")
	}
	_, certX := site(t)
	x := &Client{Token: Token{Address: tok.Address, Key: tok.Key}, Cert: certX}
	go x.Run(ctx)
	time.Sleep(500 * time.Millisecond)
	if x.Connected() || !strings.Contains(x.Status().Error, "not registered") {
		t.Fatalf("site without the secret should be refused, status %+v", x.Status())
	}
}

func TestRelayDirectRendezvous(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	srv, _ := New(t.TempDir(), addr, "")
	go srv.Run(ctx, addr)
	tok, _ := ParseToken(srv.Token())
	tok.Address = addr

	idA, certA := site(t)
	idB, certB := site(t)
	udpB, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer udpB.Close()

	punched := make(chan []string, 1)
	a := &Client{Token: tok, Cert: certA, OnPunch: func(from string, eps []string) {
		if from == idB.SiteID {
			punched <- eps
		}
	}}
	b := &Client{Token: tok, Cert: certB, UDP: udpB, Local: func() []string { return []string{"10.0.0.2:7443"} }}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := udpB.ReadFrom(buf)
			if err != nil {
				return
			}
			b.HandleReply(buf[:n], from)
		}
	}()
	go a.Run(ctx)
	go b.Run(ctx)
	waitFor(t, "clients to connect", func() bool { return a.Connected() && b.Connected() })
	waitFor(t, "B's public address", func() bool { return b.Status().Public != "" })

	eps, err := a.RequestDirect(ctx, idB.SiteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 2 || eps[0] != udpB.LocalAddr().String() || eps[1] != "10.0.0.2:7443" {
		t.Fatalf("unexpected endpoints for B: %v", eps)
	}
	// B asks for A, and A is told to punch towards B.
	if _, err := b.RequestDirect(ctx, idA.SiteID); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-punched:
		if len(got) == 0 || got[0] != udpB.LocalAddr().String() {
			t.Fatalf("A got wrong punch endpoints: %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("A was not asked to punch")
	}
}

func TestTokenForms(t *testing.T) {
	id, _ := site(t)
	tok := Token{Address: "relay.example:7444", Key: id.Pub, Secret: "s3cret"}
	plain, err := ParseToken(tok.Encode())
	if err != nil || plain.Address != "" || plain.Secret != "s3cret" || !plain.Key.Equal(id.Pub) {
		t.Fatalf("plain token: %+v %v", plain, err)
	}
	full, err := ParseToken(tok.EncodeWithAddress())
	if err != nil || full.Address != "relay.example:7444" {
		t.Fatalf("token with address: %+v %v", full, err)
	}
}
