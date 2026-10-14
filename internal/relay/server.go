package relay

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"ibs/internal/keys"
)

const (
	handshakeTimeout = 10 * time.Second
	acceptTimeout    = 10 * time.Second
	maxSplices       = 2000
)

// state is what a relay persists: its key, its registration secret and the
// keys of registered sites. No site data ever passes through it unencrypted.
type state struct {
	Seed   []byte               `json:"seed"`
	Secret string               `json:"secret"`
	Sites  map[string]siteEntry `json:"sites"`
}

type siteEntry struct {
	Key   []byte `json:"key"`
	Name  string `json:"name"`
	Added int64  `json:"added"`
}

type control struct {
	siteID string
	name   string
	conn   net.Conn
	wmu    sync.Mutex
	local  []string
	token  string
}

func (c *control) send(m Msg) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return writeMsg(c.conn, m)
}

type pendingConn struct {
	target string
	ch     chan net.Conn
}

// Server is a relay.
type Server struct {
	dir       string
	advertise string
	priv      ed25519.PrivateKey
	log       *slog.Logger

	mu       sync.Mutex
	st       state
	loaded   time.Time // modification time of the state file when last read or written
	controls map[string]*control
	pending  map[string]pendingConn
	tokens   map[string]string // udp probe token -> site id
	observed map[string]string // site id -> public UDP address seen by the relay
	splices  int

	// OnTokenChange is called when "ibs-relay new-token" replaced the token
	// while the relay runs.
	OnTokenChange func(token string)
}

// New loads or creates the relay state in dir. secret, when non-empty,
// replaces the stored registration secret.
func New(dir, advertise, secret string) (*Server, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Server{
		dir: dir, advertise: advertise, log: slog.Default(),
		controls: map[string]*control{}, pending: map[string]pendingConn{},
		tokens: map[string]string{}, observed: map[string]string{},
	}
	b, err := os.ReadFile(s.statePath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.st = state{Seed: randomBytes(ed25519.SeedSize), Secret: randomHex(24), Sites: map[string]siteEntry{}}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &s.st); err != nil {
			return nil, fmt.Errorf("read relay state: %w", err)
		}
		if fi, err := os.Stat(s.statePath()); err == nil {
			s.loaded = fi.ModTime()
		}
	}
	if secret != "" {
		s.st.Secret = secret
	}
	if s.st.Sites == nil {
		s.st.Sites = map[string]siteEntry{}
	}
	if err := s.save(); err != nil {
		return nil, err
	}
	s.priv = ed25519.NewKeyFromSeed(s.st.Seed)
	return s, nil
}

func (s *Server) statePath() string { return filepath.Join(s.dir, "relay.json") }

func (s *Server) save() error {
	s.reload()
	if err := writeState(s.statePath(), s.st); err != nil {
		return err
	}
	if fi, err := os.Stat(s.statePath()); err == nil {
		s.loaded = fi.ModTime()
	}
	return nil
}

func writeState(path string, st state) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readState(path string) (state, error) {
	var st state
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("read relay state: %w", err)
	}
	return st, nil
}

// reload picks up changes made to the state file by "ibs-relay new-token"
// while the relay runs: the new secret, and any sites it knows. It reports
// whether the secret changed. Callers hold s.mu.
func (s *Server) reload() bool {
	fi, err := os.Stat(s.statePath())
	if err != nil || !fi.ModTime().After(s.loaded) {
		return false
	}
	disk, err := readState(s.statePath())
	if err != nil {
		return false
	}
	s.loaded = fi.ModTime()
	for id, e := range disk.Sites {
		if _, ok := s.st.Sites[id]; !ok {
			s.st.Sites[id] = e
		}
	}
	if disk.Secret == "" || disk.Secret == s.st.Secret {
		return false
	}
	s.st.Secret = disk.Secret
	return true
}

// watchSecret hands a new token to the connected sites, so the invites they
// create carry it.
func (s *Server) watchSecret(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		changed := s.reload()
		secret := s.st.Secret
		var ctls []*control
		if changed {
			for _, c := range s.controls {
				ctls = append(ctls, c)
			}
		}
		s.mu.Unlock()
		if changed {
			s.log.Info("relay token changed; sites already registered keep working")
			if s.OnTokenChange != nil {
				s.OnTokenChange(s.Token())
			}
			for _, c := range ctls {
				c.send(Msg{Op: "secret", Secret: secret})
			}
		}
	}
}

// ReadToken returns the token of the relay whose state is in dir.
func ReadToken(dir string) (string, error) {
	st, err := readState(filepath.Join(dir, "relay.json"))
	if err != nil {
		return "", err
	}
	return Token{Key: ed25519.NewKeyFromSeed(st.Seed).Public().(ed25519.PublicKey), Secret: st.Secret}.Encode(), nil
}

// NewToken replaces the registration secret of the relay whose state is in
// dir and returns the new token. Sites that are already registered keep
// working; the old token can no longer register new sites. A running relay
// picks up the change within seconds.
func NewToken(dir string) (string, error) {
	path := filepath.Join(dir, "relay.json")
	st, err := readState(path)
	if err != nil {
		return "", err
	}
	st.Secret = randomHex(24)
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if err := writeState(path, st); err != nil {
		return "", err
	}
	// Run as root (docker exec), keep the file owned by the relay's user.
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		os.Chown(path, int(sys.Uid), int(sys.Gid))
	}
	return ReadToken(dir)
}

func (s *Server) Pub() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }

// Token returns the token sites use to register with this relay.
func (s *Server) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Token{Key: s.Pub(), Secret: s.st.Secret}.Encode()
}

// Run serves the relay on addr (TCP for connections, UDP for address
// discovery) until ctx is done.
func (s *Server) Run(ctx context.Context, addr string) error {
	cert, err := keys.SelfSignedCert(s.priv, "ibs-relay-"+ID(s.Pub()))
	if err != nil {
		return err
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		return err
	}
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		ln.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
		udp.Close()
	}()
	go s.serveUDP(udp)
	go s.watchSecret(ctx)
	s.log.Info("relay running", "relay", ID(s.Pub()), "listen", addr, "advertise", s.advertise, "sites", len(s.st.Sites))
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(c.(*tls.Conn))
	}
}

// serveUDP answers address probes. A probe carries the token handed out on
// the site's control connection, so addresses cannot be registered for
// other sites.
func (s *Server) serveUDP(udp *net.UDPConn) {
	buf := make([]byte, 1500)
	for {
		n, from, err := udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n <= len(udpProbe) || string(buf[:len(udpProbe)]) != string(udpProbe) {
			continue
		}
		token := string(buf[len(udpProbe):n])
		s.mu.Lock()
		site, ok := s.tokens[token]
		if ok {
			s.observed[site] = from.String()
		}
		s.mu.Unlock()
		if ok {
			udp.WriteToUDP(append(append([]byte{}, udpReply...), from.String()...), from)
		}
	}
}

func (s *Server) authorize(pub ed25519.PublicKey, secret, name string) bool {
	id := keys.SiteID(pub)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.st.Sites[id]; ok && ed25519.PublicKey(e.Key).Equal(pub) {
		if name != "" && name != e.Name {
			e.Name = name
			s.st.Sites[id] = e
			s.save()
		}
		return true
	}
	s.reload()
	if secret == "" || !hmac.Equal([]byte(secret), []byte(s.st.Secret)) {
		return false
	}
	s.st.Sites[id] = siteEntry{Key: pub, Name: name, Added: time.Now().Unix()}
	if err := s.save(); err != nil {
		s.log.Error("save relay state", "err", err)
	}
	s.log.Info("site registered", "site", id[:8], "name", name)
	return true
}

func (s *Server) registered(pub ed25519.PublicKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.st.Sites[keys.SiteID(pub)]
	return ok && ed25519.PublicKey(e.Key).Equal(pub)
}

func (s *Server) handle(c *tls.Conn) {
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := c.Handshake(); err != nil {
		c.Close()
		return
	}
	pub, err := keys.PubFromCert(c.ConnectionState().PeerCertificates[0])
	if err != nil {
		c.Close()
		return
	}
	r := bufio.NewReader(c)
	hello, err := readMsg(r)
	if err != nil {
		c.Close()
		return
	}
	c.SetDeadline(time.Time{})
	siteID := keys.SiteID(pub)
	switch hello.Op {
	case "control":
		if !s.authorize(pub, hello.Secret, hello.Name) {
			writeMsg(c, Msg{Op: "error", Error: "not registered with this relay: a valid relay token is needed"})
			c.Close()
			return
		}
		s.runControl(c, r, siteID, hello)
	case "connect":
		if !s.registered(pub) {
			writeMsg(c, Msg{Op: "error", Error: "not registered with this relay"})
			c.Close()
			return
		}
		s.connect(c, r, siteID, hello.Target)
	case "accept":
		if !s.registered(pub) {
			c.Close()
			return
		}
		s.accept(c, r, siteID, hello.ID)
	default:
		c.Close()
	}
}

func (s *Server) runControl(c net.Conn, r *bufio.Reader, siteID string, hello Msg) {
	ctl := &control{siteID: siteID, name: hello.Name, conn: c, local: hello.Local, token: randomHex(16)}
	s.mu.Lock()
	if old := s.controls[siteID]; old != nil {
		old.conn.Close()
		delete(s.tokens, old.token)
	}
	s.controls[siteID] = ctl
	s.tokens[ctl.token] = siteID
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.controls[siteID] == ctl {
			delete(s.controls, siteID)
			delete(s.observed, siteID)
		}
		delete(s.tokens, ctl.token)
		s.mu.Unlock()
		c.Close()
	}()
	s.mu.Lock()
	secret := s.st.Secret
	s.mu.Unlock()
	// The current secret lets the site put a valid token in its invites.
	if err := ctl.send(Msg{Op: "welcome", Token: ctl.token, ID: siteID, Secret: secret, Version: Version}); err != nil {
		return
	}
	for {
		c.SetReadDeadline(time.Now().Add(90 * time.Second))
		m, err := readMsg(r)
		if err != nil {
			return
		}
		switch m.Op {
		case "ping":
			ctl.send(Msg{Op: "pong", Time: m.Time})
		case "local":
			s.mu.Lock()
			ctl.local = m.Local
			s.mu.Unlock()
		case "direct":
			// Exchange address candidates so both sides can punch through
			// their NATs and connect directly.
			s.mu.Lock()
			target := s.controls[m.Target]
			mine := endpoints(s.observed[siteID], ctl.local)
			var theirs []string
			if target != nil {
				theirs = endpoints(s.observed[m.Target], target.local)
			}
			s.mu.Unlock()
			if target == nil {
				ctl.send(Msg{Op: "endpoints", Req: m.Req, Error: "site is not connected to this relay"})
				continue
			}
			target.send(Msg{Op: "punch", From: siteID, Endpoints: mine})
			ctl.send(Msg{Op: "endpoints", Req: m.Req, Endpoints: theirs})
		}
	}
}

func endpoints(observed string, local []string) []string {
	var out []string
	if observed != "" {
		out = append(out, observed)
	}
	for _, l := range local {
		if l != observed {
			out = append(out, l)
		}
	}
	return out
}

func (s *Server) connect(c net.Conn, r *bufio.Reader, from, target string) {
	defer c.Close()
	s.mu.Lock()
	ctl := s.controls[target]
	if ctl == nil || s.splices >= maxSplices {
		s.mu.Unlock()
		msg := "site is not connected to this relay"
		if ctl != nil {
			msg = "relay is at capacity"
		}
		writeMsg(c, Msg{Op: "error", Error: msg})
		return
	}
	id := randomHex(16)
	ch := make(chan net.Conn, 1)
	s.pending[id] = pendingConn{target: target, ch: ch}
	s.splices++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.splices--
		s.mu.Unlock()
	}()
	if err := ctl.send(Msg{Op: "incoming", ID: id, From: from}); err != nil {
		writeMsg(c, Msg{Op: "error", Error: "site did not answer"})
		return
	}
	var peer net.Conn
	select {
	case peer = <-ch:
	case <-time.After(acceptTimeout):
		writeMsg(c, Msg{Op: "error", Error: "site did not answer"})
		return
	}
	defer peer.Close()
	if err := writeMsg(c, Msg{Op: "ok"}); err != nil {
		return
	}
	splice(readerConn{c, r}, peer)
}

func (s *Server) accept(c net.Conn, r *bufio.Reader, siteID, id string) {
	s.mu.Lock()
	p, ok := s.pending[id]
	s.mu.Unlock()
	if !ok || p.target != siteID {
		c.Close()
		return
	}
	done := make(chan struct{})
	p.ch <- &closeNotify{Conn: readerConn{c, r}, done: done}
	<-done
}

// splice copies both ways until either side is done.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	a.Close()
	b.Close()
	<-done
}

// readerConn reads through a bufio.Reader that may hold buffered bytes.
type readerConn struct {
	net.Conn
	r *bufio.Reader
}

func (c readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }

type closeNotify struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *closeNotify) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func randomHex(n int) string { return hex.EncodeToString(randomBytes(n)) }
