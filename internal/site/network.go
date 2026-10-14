package site

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"ibs/internal/relay"
)

type origin int

const (
	originTCP   origin = iota // the site's TLS listener: direct address and own storage nodes
	originRelay               // spliced through a relay
	originQUIC                // direct QUIC connection (hole punched or LAN)
)

const originKey ctxKey = 1

func originOf(ctx context.Context) origin {
	o, _ := ctx.Value(originKey).(origin)
	return o
}

var quicConfig = &quic.Config{
	KeepAlivePeriod:      10 * time.Second, // also keeps NAT mappings open
	MaxIdleTimeout:       45 * time.Second,
	HandshakeIdleTimeout: 4 * time.Second,
}

// Serve starts everything other sites and storage nodes connect to:
//   - the TLS listener on IBS_PEER_ADDR (storage nodes, direct addresses),
//   - connections relayed through the relays this site is registered with,
//   - direct QUIC connections on IBS_QUIC_ADDR (UDP), set up by hole punching.
//
// All of them serve the same handler with mutual TLS and pinned keys.
func (s *Site) Serve(ctx context.Context) error {
	cert, err := s.ID.Certificate()
	if err != nil {
		return err
	}
	serverTLS := func(protos ...string) *tls.Config {
		return &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAnyClientCert,
			MinVersion: tls.VersionTLS13, NextProtos: protos}
	}
	handler := s.peerHandler()
	connCtx := func(ctx context.Context, c net.Conn) context.Context {
		if tc, ok := c.(*tls.Conn); ok {
			if _, relayed := tc.NetConn().(*relayedConn); relayed {
				return context.WithValue(ctx, originKey, originRelay)
			}
		}
		return context.WithValue(ctx, originKey, originTCP)
	}

	tcpSrv := &http.Server{Addr: s.cfg.PeerAddr, Handler: handler, TLSConfig: serverTLS(), ConnContext: connCtx,
		ReadHeaderTimeout: 30 * time.Second}
	relaySrv := &http.Server{Handler: handler, TLSConfig: serverTLS(), ConnContext: connCtx, ReadHeaderTimeout: 30 * time.Second}
	tcpLn, err := net.Listen("tcp", s.cfg.PeerAddr)
	if err != nil {
		return err
	}
	go tcpSrv.ServeTLS(tcpLn, "", "")
	go relaySrv.ServeTLS(s.relayed, "", "")

	// Direct connections over QUIC share one UDP socket for listening,
	// dialing, relay address probes and hole punching.
	var h3 *http3.Server
	if udp, err := net.ListenPacket("udp", s.cfg.QUICAddr); err != nil {
		s.log.Warn("direct connections disabled: cannot open UDP socket", "addr", s.cfg.QUICAddr, "err", err)
	} else {
		tr := &quic.Transport{Conn: udp}
		ln, err := tr.Listen(serverTLS(http3.NextProtoH3), quicConfig)
		if err != nil {
			return err
		}
		h3 = &http3.Server{Handler: handler, ConnContext: func(cctx context.Context, c *quic.Conn) context.Context {
			go s.connectBack(ctx, c)
			return context.WithValue(cctx, originKey, originQUIC)
		}}
		go h3.ServeListener(ln)
		s.netMu.Lock()
		s.quicTr = tr
		s.netMu.Unlock()
		go s.readUDP(ctx, tr)
	}

	s.startRelays(ctx)
	go s.directLoop(ctx)
	go s.probeLoop(ctx)

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tcpSrv.Shutdown(sctx)
		relaySrv.Shutdown(sctx)
		s.relayed.Close()
		if h3 != nil {
			h3.Close()
		}
	}()
	return nil
}

// readUDP handles the non-QUIC packets on the shared socket: address replies
// from relays. Punch packets need no handling; they only open NAT mappings.
func (s *Site) readUDP(ctx context.Context, tr *quic.Transport) {
	buf := make([]byte, 1500)
	for {
		n, from, err := tr.ReadNonQUICPacket(ctx, buf)
		if err != nil {
			return
		}
		if relay.IsPunch(buf[:n]) {
			continue
		}
		for _, rc := range s.relayClients() {
			if rc.HandleReply(buf[:n], from) {
				break
			}
		}
	}
}

// localEndpoints lists this site's own UDP addresses (LAN interfaces) that
// peers on the same network can reach directly.
func (s *Site) localEndpoints() []string {
	s.netMu.Lock()
	tr := s.quicTr
	s.netMu.Unlock()
	if tr == nil {
		return nil
	}
	port := tr.Conn.LocalAddr().(*net.UDPAddr).Port
	ifaces, _ := net.Interfaces()
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, net.JoinHostPort(ipn.IP.String(), strconv.Itoa(port)))
		}
	}
	return out
}

// relayedConn marks connections that arrived through a relay.
type relayedConn struct {
	net.Conn
	from string
}

// connQueue is a net.Listener fed with relayed connections.
type connQueue struct {
	ch     chan net.Conn
	once   sync.Once
	closed chan struct{}
}

func newConnQueue() *connQueue {
	return &connQueue{ch: make(chan net.Conn, 64), closed: make(chan struct{})}
}

func (q *connQueue) push(c net.Conn) {
	select {
	case q.ch <- c:
	case <-q.closed:
		c.Close()
	}
}

func (q *connQueue) Accept() (net.Conn, error) {
	select {
	case c := <-q.ch:
		return c, nil
	case <-q.closed:
		return nil, errors.New("listener closed")
	}
}

func (q *connQueue) Close() error {
	q.once.Do(func() { close(q.closed) })
	return nil
}

func (q *connQueue) Addr() net.Addr { return &net.TCPAddr{} }
