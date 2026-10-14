package relay

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"ibs/internal/keys"
)

// PacketWriter sends UDP packets from the site's shared QUIC socket.
type PacketWriter interface {
	WriteTo(b []byte, addr net.Addr) (int, error)
}

// Client keeps a site connected to one relay.
type Client struct {
	Token Token
	Cert  tls.Certificate // the site's certificate
	Name  string          // the site's name, shown in relay logs

	// Local returns the site's local UDP address candidates (LAN addresses
	// of the QUIC socket), offered to peers for direct connections.
	Local func() []string
	// UDP sends address probes from the QUIC socket; may be nil.
	UDP PacketWriter
	// OnIncoming receives connections relayed from other sites.
	OnIncoming func(conn net.Conn, from string)
	// OnPunch is called when a site asks to connect directly to this one.
	OnPunch func(from string, endpoints []string)

	// OnSecret is called when the relay hands out a new registration secret
	// (its token was regenerated), so the site can store it.
	OnSecret func(secret string)

	mu        sync.Mutex
	secret    string // the current registration secret, when it differs from Token's
	ctl       net.Conn
	wmu       sync.Mutex
	udpToken  string
	version   string
	observed  string
	rtt       time.Duration
	lastErr   string
	since     time.Time
	reqs      map[int64]chan Msg
	nextReq   atomic.Int64
	connected atomic.Bool
}

type Status struct {
	ID        string  `json:"id"`
	Address   string  `json:"address"`
	Connected bool    `json:"connected"`
	Since     int64   `json:"since,omitempty"`
	RTTMillis float64 `json:"rtt_ms"`
	Public    string  `json:"public,omitempty"` // this site's public UDP address as seen by the relay
	Error     string  `json:"error,omitempty"`
	Version   string  `json:"version,omitempty"` // the relay's, as it reported it
}

func (c *Client) ID() string { return ID(c.Token.Key) }

func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{ID: c.ID(), Address: c.Token.Address, Connected: c.connected.Load(), RTTMillis: float64(c.rtt.Microseconds()) / 1000,
		Public: c.observed, Error: c.lastErr, Version: c.version}
	if st.Connected {
		st.Since = c.since.Unix()
	}
	return st
}

func (c *Client) Connected() bool { return c.connected.Load() }

func (c *Client) RTT() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rtt
}

func (c *Client) dialRelay(ctx context.Context) (*tls.Conn, error) {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}, Config: keys.ClientTLS(c.Cert, c.Token.Key)}
	conn, err := d.DialContext(ctx, "tcp", c.Token.Address)
	if err != nil {
		return nil, err
	}
	return conn.(*tls.Conn), nil
}

// Run keeps the control connection up until ctx is done.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx)
		c.connected.Store(false)
		c.mu.Lock()
		if err != nil && ctx.Err() == nil {
			c.lastErr = err.Error()
		}
		c.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		slog.Debug("relay connection lost", "relay", c.Token.Address, "err", err, "retry", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Client) session(ctx context.Context) error {
	conn, err := c.dialRelay(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	var local []string
	if c.Local != nil {
		local = c.Local()
	}
	if err := writeMsg(conn, Msg{Op: "control", Secret: c.CurrentToken().Secret, Name: c.Name, Local: local}); err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	welcome, err := readMsg(r)
	if err != nil {
		return err
	}
	if welcome.Op != "welcome" {
		return fmt.Errorf("relay refused: %s", welcome.Error)
	}
	c.setSecret(welcome.Secret)
	c.mu.Lock()
	c.ctl, c.udpToken, c.lastErr, c.since, c.version = conn, welcome.Token, "", time.Now(), welcome.Version
	c.reqs = map[int64]chan Msg{}
	c.mu.Unlock()
	c.connected.Store(true)
	v := welcome.Version
	if v == "" {
		v = "not reported" // relays before 1.3.0
	}
	slog.Info("connected to relay", "relay", c.Token.Address, "version", v)

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-sctx.Done()
		conn.Close()
	}()
	go c.keepalive(sctx)
	for {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		m, err := readMsg(r)
		if err != nil {
			return err
		}
		switch m.Op {
		case "secret":
			c.setSecret(m.Secret)
		case "pong":
			c.mu.Lock()
			rtt := time.Since(time.Unix(0, m.Time))
			if c.rtt == 0 {
				c.rtt = rtt
			} else {
				c.rtt = (c.rtt*3 + rtt) / 4
			}
			c.mu.Unlock()
		case "incoming":
			go c.acceptIncoming(ctx, m.ID, m.From)
		case "punch":
			if c.OnPunch != nil {
				go c.OnPunch(m.From, m.Endpoints)
			}
		case "endpoints":
			c.mu.Lock()
			ch := c.reqs[m.Req]
			delete(c.reqs, m.Req)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
}

func (c *Client) send(m Msg) error {
	c.mu.Lock()
	conn := c.ctl
	c.mu.Unlock()
	if conn == nil || !c.connected.Load() {
		return errors.New("not connected to relay")
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return writeMsg(conn, m)
}

// keepalive measures the round trip to the relay, refreshes the local
// address candidates and probes the relay over UDP, which also keeps the NAT
// mapping of the QUIC socket alive.
func (c *Client) keepalive(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		c.send(Msg{Op: "ping", Time: time.Now().UnixNano()})
		if c.Local != nil {
			c.send(Msg{Op: "local", Local: c.Local()})
		}
		c.probe()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Client) probe() {
	if c.UDP == nil {
		return
	}
	c.mu.Lock()
	token := c.udpToken
	c.mu.Unlock()
	addr, err := net.ResolveUDPAddr("udp", c.Token.Address)
	if err != nil || token == "" {
		return
	}
	c.UDP.WriteTo(ProbePacket(token), addr)
}

// HandleReply records the public address from a relay UDP reply. It reports
// whether the packet came from this relay.
func (c *Client) HandleReply(b []byte, from net.Addr) bool {
	addr, ok := ParseReply(b)
	if !ok {
		return false
	}
	relayAddr, err := net.ResolveUDPAddr("udp", c.Token.Address)
	if err != nil || relayAddr.String() != from.String() {
		return false
	}
	c.mu.Lock()
	c.observed = addr
	c.mu.Unlock()
	return true
}

func (c *Client) acceptIncoming(ctx context.Context, id, from string) {
	conn, err := c.dialRelay(ctx)
	if err != nil {
		return
	}
	if err := writeMsg(conn, Msg{Op: "accept", ID: id}); err != nil {
		conn.Close()
		return
	}
	if c.OnIncoming == nil {
		conn.Close()
		return
	}
	c.OnIncoming(conn, from)
}

// Dial opens a relayed connection to target. The caller runs TLS over it.
func (c *Client) Dial(ctx context.Context, target string) (net.Conn, error) {
	if !c.connected.Load() {
		return nil, errors.New("not connected to relay " + c.Token.Address)
	}
	conn, err := c.dialRelay(ctx)
	if err != nil {
		return nil, err
	}
	if err := writeMsg(conn, Msg{Op: "connect", Target: target}); err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(acceptTimeout + 5*time.Second))
	r := bufio.NewReader(conn)
	m, err := readMsg(r)
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadDeadline(time.Time{})
	if m.Op != "ok" {
		conn.Close()
		return nil, fmt.Errorf("relay %s: %s", c.Token.Address, m.Error)
	}
	return readerConn{conn, r}, nil
}

// RequestDirect asks the relay for target's address candidates. The relay
// also tells target to punch towards this site.
func (c *Client) RequestDirect(ctx context.Context, target string) ([]string, error) {
	req := c.nextReq.Add(1)
	ch := make(chan Msg, 1)
	c.mu.Lock()
	if c.reqs == nil {
		c.mu.Unlock()
		return nil, errors.New("not connected to relay")
	}
	c.reqs[req] = ch
	c.mu.Unlock()
	if err := c.send(Msg{Op: "direct", Req: req, Target: target}); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if m.Error != "" {
			return nil, errors.New(m.Error)
		}
		return m.Endpoints, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.reqs, req)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// CurrentToken is the relay's token with its current registration secret.
func (c *Client) CurrentToken() Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.Token
	if c.secret != "" {
		t.Secret = c.secret
	}
	return t
}

func (c *Client) setSecret(secret string) {
	if secret == "" {
		return
	}
	c.mu.Lock()
	changed := secret != c.Token.Secret && secret != c.secret
	if changed {
		c.secret = secret
	}
	c.mu.Unlock()
	if changed && c.OnSecret != nil {
		c.OnSecret(secret)
	}
}
