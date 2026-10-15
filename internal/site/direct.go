package site

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"ibs/internal/keys"
	"ibs/internal/relay"
)

// How direct connections come about:
//
//  1. Site A asks a relay both sites use for B's address candidates (B's
//     public address as the relay sees it, plus B's LAN addresses). The
//     relay also sends A's candidates to B.
//  2. Both sides send small UDP "punch" packets to each other's candidates
//     from their QUIC socket. That creates NAT mappings on both routers, so
//     each side's packets are let in.
//  3. A dials QUIC to every candidate at once; the first handshake that
//     completes (with B's key pinned) wins. B does the same towards A for its
//     own requests.
//
// Only one side of each pair initiates (the lower site ID); the other answers
// the relay's punch request. Two simultaneous initiations make NATs that
// track flows (Linux conntrack) assign clashing ports.
//
// If no candidate works (e.g. both sides behind symmetric NAT), traffic
// keeps flowing through the relay and the attempt is repeated: soon at first,
// since stuck NAT entries from a failed attempt expire within about 30
// seconds, then every couple of minutes.

const (
	punchWindow  = 2 * time.Second
	dialDeadline = 5 * time.Second
)

// directBackoff is the wait before the next attempt after n failures.
func directBackoff(n int) time.Duration {
	return min(20*time.Second<<min(n, 3), 2*time.Minute)
}

func (s *Site) directLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, p := range s.onlinePeers() {
			pc, err := s.peer(p.siteID)
			if err != nil {
				continue
			}
			if s.ID.SiteID > p.siteID {
				continue // the peer initiates; we answer its punch requests
			}
			pc.mu.Lock()
			due := (pc.quic == nil || !pc.quic.alive()) && !pc.tryingQUIC && time.Since(pc.directTry) > directBackoff(pc.directFails)
			pc.mu.Unlock()
			if due {
				go s.tryDirect(ctx, pc, nil)
			}
		}
	}
}

// tryDirect attempts a direct QUIC connection to the peer. endpoints, when
// given, are the peer's candidates (from its punch request); otherwise they
// are requested through a common relay.
func (s *Site) tryDirect(ctx context.Context, pc *peerClient, endpoints []string) {
	s.netMu.Lock()
	tr := s.quicTr
	s.netMu.Unlock()
	if tr == nil {
		return
	}
	pc.mu.Lock()
	if pc.tryingQUIC || (pc.quic != nil && pc.quic.alive()) {
		pc.mu.Unlock()
		return
	}
	pc.tryingQUIC, pc.directTry = true, time.Now()
	pc.mu.Unlock()
	defer func() {
		pc.mu.Lock()
		pc.tryingQUIC = false
		pc.mu.Unlock()
	}()

	setErr := func(msg string) {
		pc.mu.Lock()
		pc.directErr = msg
		pc.mu.Unlock()
	}
	if endpoints == nil {
		if rc := s.commonRelay(pc.routes); rc != nil {
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			eps, err := rc.RequestDirect(rctx, pc.siteID)
			cancel()
			if err != nil && pc.routes.Address == "" {
				setErr(err.Error())
				return
			}
			endpoints = eps
		} else if pc.routes.Address == "" {
			setErr("no relay in common to arrange a direct connection")
			return
		}
	}
	// The peer's direct address is a candidate too: what a relay reports is
	// the address it sees and the peer's own interfaces, which inside Docker
	// are the container's, not the host's.
	if pc.routes.Address != "" {
		endpoints = append(endpoints, pc.routes.Address)
	}
	conn, ep, err := s.punchAndDial(ctx, tr, pc, endpoints)
	if err != nil {
		pc.mu.Lock()
		pc.directFails++
		pc.mu.Unlock()
		setErr("no direct path found (NAT or firewall); using the relay")
		s.log.Debug("direct connection failed", "peer", pc.siteID[:8], "err", err)
		return
	}
	pc.setQUIC(conn, ep)
	pc.mu.Lock()
	pc.directFails = 0
	pc.mu.Unlock()
	setErr("")
	s.log.Info("direct connection established", "peer", pc.siteID[:8], "endpoint", ep.String())
	s.notify(TopicPeers)
}

func (s *Site) quicClientTLS(pc *peerClient) (*tls.Config, error) {
	cert, err := s.ID.Certificate()
	if err != nil {
		return nil, err
	}
	c := keys.ClientTLS(cert, pc.pub)
	c.NextProtos = []string{http3.NextProtoH3}
	return c, nil
}

func resolveEndpoints(eps []string) []*net.UDPAddr {
	seen := map[string]bool{}
	var out []*net.UDPAddr
	for _, e := range eps {
		a, err := net.ResolveUDPAddr("udp", e)
		if err != nil || seen[a.String()] {
			continue
		}
		seen[a.String()] = true
		out = append(out, a)
	}
	return out
}

func (s *Site) punch(tr *quic.Transport, addrs []*net.UDPAddr, d time.Duration) {
	pkt := relay.PunchPacket(s.ID.SiteID)
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		for _, a := range addrs {
			tr.WriteTo(pkt, a)
		}
	}
}

func (s *Site) punchAndDial(ctx context.Context, tr *quic.Transport, pc *peerClient, eps []string) (*quic.Conn, *net.UDPAddr, error) {
	addrs := resolveEndpoints(eps)
	if len(addrs) == 0 {
		return nil, nil, errors.New("peer has no address candidates")
	}
	tlsConf, err := s.quicClientTLS(pc)
	if err != nil {
		return nil, nil, err
	}
	go s.punch(tr, addrs, punchWindow)
	dctx, cancel := context.WithTimeout(ctx, dialDeadline)
	defer cancel()
	type result struct {
		conn *quic.Conn
		addr *net.UDPAddr
		err  error
	}
	results := make(chan result, len(addrs))
	for i, a := range addrs {
		go func() {
			// Give the punch packets a moment to open the way.
			time.Sleep(time.Duration(100+50*i) * time.Millisecond)
			c, err := tr.Dial(dctx, a, tlsConf, quicConfig)
			results <- result{c, a, err}
		}()
	}
	var best result
	var lastErr error
	for range addrs {
		r := <-results
		if r.err != nil {
			lastErr = r.err
			continue
		}
		if best.conn == nil {
			best = r
			cancel() // stop the other attempts
		} else {
			r.conn.CloseWithError(0, "duplicate")
		}
	}
	if best.conn == nil {
		return nil, nil, lastErr
	}
	return best.conn, best.addr, nil
}

// setQUIC switches the peer to an established direct connection. HTTP/3
// requests use it; if it drops, the transport redials the same endpoint, and
// when that fails the peer falls back to its other paths.
func (pc *peerClient) setQUIC(conn *quic.Conn, ep *net.UDPAddr) {
	q := &quicPath{endpoint: ep, conn: conn}
	s := pc.s
	tlsConf, _ := s.quicClientTLS(pc)
	q.tr = &http3.Transport{
		TLSClientConfig: tlsConf,
		QUICConfig:      quicConfig,
		Dial: func(ctx context.Context, _ string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
			q.mu.Lock()
			defer q.mu.Unlock()
			if q.conn != nil && q.conn.Context().Err() == nil {
				return q.conn, nil
			}
			s.netMu.Lock()
			tr := s.quicTr
			s.netMu.Unlock()
			dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			c, err := tr.Dial(dctx, q.endpoint, tlsCfg, cfg)
			if err != nil {
				return nil, err
			}
			q.conn = c
			return c, nil
		},
	}
	q.client = &http.Client{Transport: q.tr, Timeout: peerClientTimeout}
	pc.mu.Lock()
	if pc.quic != nil {
		pc.quic.close()
	}
	pc.quic = q
	pc.active, pc.activeAt = PathQUIC, time.Now()
	pc.mu.Unlock()
}

// onPunch handles a paired site's request (via a relay) to connect
// directly: open our NAT towards it, then connect back for our own requests.
func (s *Site) onPunch(from string, endpoints []string) {
	pc, err := s.peer(from)
	if err != nil {
		return // only paired sites
	}
	s.netMu.Lock()
	tr := s.quicTr
	ctx := s.runCtx
	s.netMu.Unlock()
	if tr == nil || ctx == nil {
		return
	}
	s.punch(tr, resolveEndpoints(endpoints), punchWindow)
	pc.mu.Lock()
	have := pc.quic != nil && pc.quic.alive()
	pc.mu.Unlock()
	if !have {
		s.tryDirect(ctx, pc, endpoints)
	}
}

// connectBack uses an incoming direct connection from a paired site to
// connect back to it: the path through both NATs is known to work now.
func (s *Site) connectBack(ctx context.Context, c *quic.Conn) {
	id, _, ok := s.verifyPeerCert(c.ConnectionState().TLS.PeerCertificates)
	if !ok {
		return
	}
	pc, err := s.peer(id)
	if err != nil {
		return
	}
	pc.mu.Lock()
	have := (pc.quic != nil && pc.quic.alive()) || pc.tryingQUIC
	pc.mu.Unlock()
	if !have {
		s.tryDirect(ctx, pc, []string{c.RemoteAddr().String()})
	}
}

// probeLoop measures the speed of each peer path now and then by fetching
// 1 MiB, unless real transfers measured it recently.
func (s *Site) probeLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, p := range s.onlinePeers() {
			pc, err := s.peer(p.siteID)
			if err != nil {
				continue
			}
			pc.mu.Lock()
			stale := time.Since(pc.speedAt) > 10*time.Minute
			pc.mu.Unlock()
			if stale {
				pc.call(ctx, http.MethodGet, "/peer/v1/probe?bytes=1048576", nil, nil)
			}
		}
	}
}
