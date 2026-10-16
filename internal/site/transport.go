package site

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"ibs/internal/keys"
	"ibs/internal/relay"
)

// RelayRef is a relay through which a site can be reached.
type RelayRef struct {
	Address string `json:"address"`
	Key     []byte `json:"key"`
}

// Routes describe how a site can be reached: a direct address (LAN, VPN or
// port forward) and/or relays. Sites announce them in their status.
type Routes struct {
	Address string     `json:"address,omitempty"`
	Relays  []RelayRef `json:"relays,omitempty"`
}

func (r Routes) empty() bool { return r.Address == "" && len(r.Relays) == 0 }

func (s *Site) OwnRoutes() Routes {
	rt := Routes{Address: s.Setting("peer_advertise")}
	for _, rc := range s.relayClients() {
		rt.Relays = append(rt.Relays, tokenRef(rc.Token))
	}
	return rt
}

// directRetryEvery is how often a peer reached through a relay is tried at
// its direct address again.
const directRetryEvery = time.Minute

// Path kinds, from best to worst.
const (
	PathQUIC   = "quic"   // direct, hole punched or same network
	PathDirect = "direct" // direct TCP to the site's address
	PathRelay  = "relay"  // through a relay
)

type peerPath struct {
	kind   string
	relay  string // relay id for PathRelay
	base   string
	client *http.Client
}

func (p peerPath) key() string {
	if p.kind == PathRelay {
		return PathRelay + ":" + p.relay
	}
	return p.kind
}

// quicPath is an established direct QUIC connection to a peer.
type quicPath struct {
	endpoint *net.UDPAddr
	mu       sync.Mutex
	conn     *quic.Conn
	tr       *http3.Transport
	client   *http.Client
}

func (q *quicPath) alive() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.conn != nil && q.conn.Context().Err() == nil
}

func (q *quicPath) rtt() time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.conn == nil {
		return 0
	}
	return q.conn.ConnectionStats().SmoothedRTT
}

func (q *quicPath) close() {
	q.tr.Close()
	q.mu.Lock()
	if q.conn != nil {
		q.conn.CloseWithError(0, "")
	}
	q.mu.Unlock()
}

// peerClient talks to one site over the best working path, with mutual TLS
// and the site's key pinned on every path.
type peerClient struct {
	s       *Site
	siteID  string
	pub     ed25519.PublicKey
	routes  Routes
	routeID string

	mu          sync.Mutex
	direct      *http.Client
	viaRelay    map[string]*http.Client
	quic        *quicPath
	active      string // key of the last path that worked
	activeAt    time.Time
	rtt         time.Duration
	bps         float64
	speedAt     time.Time
	directTry   time.Time
	directRetry time.Time // when the direct address was last tried while a relay worked
	directFails int
	directErr   string
	tryingQUIC  bool
}

func routesKey(r Routes) string {
	b, _ := json.Marshal(r)
	return string(b)
}

func (s *Site) newPeerClient(siteID string, pub ed25519.PublicKey, routes Routes) *peerClient {
	return &peerClient{s: s, siteID: siteID, pub: pub, routes: routes, routeID: routesKey(routes), viaRelay: map[string]*http.Client{}}
}

// peer returns the client for a paired site, rebuilding it when the site's
// routes changed.
func (s *Site) peer(siteID string) (*peerClient, error) {
	var addr, routesJSON string
	var pub []byte
	err := s.DB.QueryRow(`SELECT address, pubkey, routes FROM peers WHERE site_id = ?`, siteID).Scan(&addr, &pub, &routesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("unknown peer %s", siteID)
	}
	if err != nil {
		return nil, err
	}
	var routes Routes
	json.Unmarshal([]byte(routesJSON), &routes)
	routes.Address = addr
	key := routesKey(routes)
	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	if c, ok := s.peerClients[siteID]; ok && c.routeID == key {
		return c, nil
	}
	c := s.newPeerClient(siteID, ed25519.PublicKey(pub), routes)
	if old := s.peerClients[siteID]; old != nil {
		// Keep a working direct connection and the measurements. A new or
		// changed direct address is tried first, not the path that worked.
		old.mu.Lock()
		c.quic, c.rtt, c.bps, c.speedAt = old.quic, old.rtt, old.bps, old.speedAt
		if old.routes.Address == routes.Address {
			c.active = old.active
		}
		old.mu.Unlock()
	}
	s.peerClients[siteID] = c
	return c, nil
}

func (pc *peerClient) tlsConfig() (*http.Transport, error) {
	cert, err := pc.s.ID.Certificate()
	if err != nil {
		return nil, err
	}
	return &http.Transport{
		TLSClientConfig:     keys.ClientTLS(cert, pc.pub),
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}, nil
}

// paths lists the ways to reach the peer, the last working one first.
func (pc *peerClient) paths() []peerPath {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	var out []peerPath
	if pc.quic != nil {
		if pc.quic.alive() {
			out = append(out, peerPath{kind: PathQUIC, base: "https://peer.ibs", client: pc.quic.client})
		} else {
			pc.quic.close()
			pc.quic = nil
		}
	}
	if pc.routes.Address != "" {
		if pc.direct == nil {
			if tr, err := pc.tlsConfig(); err == nil {
				tr.DialContext = (&net.Dialer{Timeout: 4 * time.Second, KeepAlive: 30 * time.Second}).DialContext
				pc.direct = &http.Client{Transport: tr, Timeout: peerClientTimeout}
			}
		}
		if pc.direct != nil {
			out = append(out, peerPath{kind: PathDirect, base: "https://" + pc.routes.Address, client: pc.direct})
		}
	}
	// Relays the peer announced first, then any other relay this site is on:
	// the peer may have joined it without us knowing yet (the relay answers
	// at once if it is not there), which is how routes are rediscovered.
	tried := map[string]bool{}
	var relays []*relay.Client
	for _, r := range pc.routes.Relays {
		id := relay.ID(ed25519.PublicKey(r.Key))
		if rc := pc.s.relayClient(id); rc != nil && !tried[id] {
			tried[id] = true
			relays = append(relays, rc)
		}
	}
	for _, rc := range pc.s.relayClients() {
		if !tried[rc.ID()] {
			tried[rc.ID()] = true
			relays = append(relays, rc)
		}
	}
	for _, rc := range relays {
		id := rc.ID()
		if !rc.Connected() {
			continue
		}
		cl := pc.viaRelay[id]
		if cl == nil {
			tr, err := pc.tlsConfig()
			if err != nil {
				continue
			}
			siteID := pc.siteID
			tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return rc.Dial(ctx, siteID) }
			cl = &http.Client{Transport: tr, Timeout: peerClientTimeout}
			pc.viaRelay[id] = cl
		}
		out = append(out, peerPath{kind: PathRelay, relay: id, base: "https://peer.ibs", client: cl})
	}
	return pc.order(out, time.Now())
}

// order puts the path that worked last first (after QUIC, which is always
// best), except that while a relay works, the direct address is tried again
// now and then: it may have become reachable (it was just added, a VPN came
// up, a port was opened) and is faster. pc.mu must be held.
func (pc *peerClient) order(out []peerPath, now time.Time) []peerPath {
	if pc.routes.Address != "" && strings.HasPrefix(pc.active, PathRelay) && now.Sub(pc.directRetry) > directRetryEvery {
		pc.directRetry = now
		return out
	}
	for i, p := range out {
		if p.key() == pc.active && i > 0 && out[0].kind != PathQUIC {
			out[0], out[i] = out[i], out[0]
			break
		}
	}
	return out
}

// call performs a request on the best working path and falls back to the
// others on connection errors. out may be nil, a *[]byte, or a value to
// decode JSON into.
func (pc *peerClient) call(ctx context.Context, method, path string, body []byte, out any) (http.Header, error) {
	paths := pc.paths()
	if len(paths) == 0 {
		return nil, fmt.Errorf("no route to site %s: it shares no connected relay and has no direct address", pc.siteID[:8])
	}
	var cancel context.CancelFunc
	if ctx.Value(streamingKey{}) != nil {
		// A download: no limit while data keeps coming.
		ctx, _, cancel = withStall(ctx)
	} else {
		ctx, cancel = context.WithTimeout(ctx, peerTimeout+time.Duration(len(body)>>20)*time.Second)
	}
	defer cancel()
	var lastErr error
	for _, p := range paths {
		if w, ok := ctx.Value(stallWatchKey{}).(*stallWatch); ok {
			w.touch() // each path gets the full time to start
		}
		start := time.Now()
		hdr, n, err := pc.s.roundTrip(ctx, p, method, path, body, out)
		var pe *PeerError
		if err == nil || errors.As(err, &pe) {
			pc.record(p, method, path, time.Since(start), int64(len(body))+n)
			return hdr, err
		}
		lastErr = err
		if p.kind == PathQUIC {
			pc.mu.Lock()
			if pc.quic != nil {
				pc.quic.close()
				pc.quic = nil
			}
			pc.mu.Unlock()
		}
		if ctx.Err() != nil {
			break
		}
	}
	if errors.Is(context.Cause(ctx), errStalled) {
		return nil, errStalled
	}
	return nil, lastErr
}

// record updates the measurements: round trip from small requests and
// throughput from large transfers.
func (pc *peerClient) record(p peerPath, method, path string, d time.Duration, n int64) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.active != p.key() {
		pc.activeAt = time.Now()
	}
	pc.active = p.key()
	if n < 4096 && d > 0 {
		sample := d
		if p.kind == PathQUIC && pc.quic != nil {
			if r := pc.quic.rtt(); r > 0 {
				sample = r
			}
		}
		if pc.rtt == 0 {
			pc.rtt = sample
		} else {
			pc.rtt = (pc.rtt*3 + sample) / 4
		}
	}
	if n >= 256<<10 && d > 0 {
		sample := float64(n) / d.Seconds()
		if pc.bps == 0 || time.Since(pc.speedAt) > 30*time.Minute {
			pc.bps = sample
		} else {
			pc.bps = pc.bps*0.6 + sample*0.4
		}
		pc.speedAt = time.Now()
	}
}

// roundTrip performs one request on one path and returns the bytes received.
var errStalled = errors.New("no data arrived for a while (stallTimeout); the other side stopped sending")

type stallWatchKey struct{}

// withStall returns a context that is cancelled (cause errStalled) when the
// stallWatch isn't touched for stallTimeout; wrap response bodies in a
// touchReader so that arriving data touches it.
func withStall(ctx context.Context) (context.Context, *stallWatch, context.CancelFunc) {
	ctx, stop := context.WithCancelCause(ctx)
	w := &stallWatch{t: time.AfterFunc(stallTimeout, func() { stop(errStalled) })}
	ctx = context.WithValue(ctx, stallWatchKey{}, w)
	return ctx, w, func() { w.t.Stop(); stop(nil) }
}

// stalled reports err as errStalled when that is why ctx ended.
func stalled(ctx context.Context, err error) error {
	if err != nil && errors.Is(context.Cause(ctx), errStalled) {
		return errStalled
	}
	return err
}

// stallWatch cancels a streaming request when no data arrived for stallTimeout.
type stallWatch struct{ t *time.Timer }

func (w *stallWatch) touch() { w.t.Reset(stallTimeout) }

// touchReader resets the stall watch whenever data arrives.
type touchReader struct {
	r io.Reader
	w *stallWatch
}

func (t touchReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.w.touch()
	}
	return n, err
}

func (s *Site) roundTrip(ctx context.Context, p peerPath, method, path string, body []byte, out any) (http.Header, int64, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, rd)
	if err != nil {
		return nil, 0, err
	}
	if adv := s.Setting("peer_advertise"); adv != "" {
		req.Header.Set("X-IBS-Address", adv)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var rb io.Reader = resp.Body
	if w, ok := ctx.Value(stallWatchKey{}).(*stallWatch); ok {
		w.touch()
		rb = touchReader{resp.Body, w}
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(rb, 512))
		return resp.Header, 0, &PeerError{resp.StatusCode, strings.TrimSpace(string(msg))}
	}
	var n int64
	switch o := out.(type) {
	case nil:
		n, err = io.Copy(io.Discard, rb)
	case *[]byte:
		*o, err = io.ReadAll(rb)
		n = int64(len(*o))
	default:
		cr := &countingReader{r: rb}
		err = json.NewDecoder(cr).Decode(out)
		n = cr.n
	}
	return resp.Header, n, err
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (s *Site) peerCall(ctx context.Context, siteID, method, path string, body []byte, out any) (http.Header, error) {
	pc, err := s.peer(siteID)
	if err != nil {
		return nil, err
	}
	return pc.call(ctx, method, path, body, out)
}

// PathInfo describes how this site currently reaches a peer.
type PathInfo struct {
	Kind      string  `json:"kind"` // quic, direct, relay or "" when unreachable
	Relay     string  `json:"relay,omitempty"`
	RelayName string  `json:"relay_name,omitempty"`
	RTTMillis float64 `json:"rtt_ms"`
	Bps       float64 `json:"bps"`
	Since     int64   `json:"since,omitempty"`
	DirectErr string  `json:"direct_error,omitempty"`
}

func (s *Site) pathInfo(siteID string) PathInfo {
	s.peerMu.Lock()
	pc := s.peerClients[siteID]
	s.peerMu.Unlock()
	if pc == nil {
		return PathInfo{}
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	info := PathInfo{RTTMillis: float64(pc.rtt.Microseconds()) / 1000, Bps: pc.bps, DirectErr: pc.directErr}
	if !pc.activeAt.IsZero() {
		info.Since = pc.activeAt.Unix()
	}
	kind, rid, _ := strings.Cut(pc.active, ":")
	if kind == PathQUIC && (pc.quic == nil || !pc.quic.alive()) {
		kind = ""
	}
	info.Kind, info.Relay = kind, rid
	if rc := s.relayClient(rid); rc != nil {
		info.RelayName = relayName(rc.Token.Address)
	}
	return info
}
