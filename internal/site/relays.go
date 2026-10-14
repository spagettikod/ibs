package site

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"ibs/internal/relay"
)

type relayEntry struct {
	client *relay.Client
	cancel context.CancelFunc
	added  int64
}

// RelayInfo is a relay this site is registered with and its live status.
type RelayInfo struct {
	relay.Status
	Name  string `json:"name"`
	Added int64  `json:"added"`
}

// relayName is a short display name for a relay: the host of its address.
func relayName(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// startRelays connects to every relay in the database and, when configured,
// registers with the relay from IBS_RELAY_TOKEN or IBS_RELAY_TOKEN_FILE.
func (s *Site) startRelays(ctx context.Context) {
	s.netMu.Lock()
	s.runCtx = ctx
	s.netMu.Unlock()
	rows, err := s.DB.Query(`SELECT address, pubkey, secret, added FROM relays`)
	if err == nil {
		var toks []struct {
			t     relay.Token
			added int64
		}
		for rows.Next() {
			var t relay.Token
			var key []byte
			var added int64
			rows.Scan(&t.Address, &key, &t.Secret, &added)
			t.Key = key
			toks = append(toks, struct {
				t     relay.Token
				added int64
			}{t, added})
		}
		rows.Close()
		for _, x := range toks {
			s.runRelay(x.t, x.added)
		}
	}
	if s.cfg.RelayToken != "" {
		if _, err := s.AddRelay(s.cfg.RelayAddress, s.cfg.RelayToken); err != nil {
			s.log.Warn("IBS_RELAY_TOKEN", "err", err)
		}
	}
	if s.cfg.RelayTokenFile != "" {
		go s.watchRelayTokenFile(ctx)
	}
}

// watchRelayTokenFile adds the relay from a token file, which may appear
// after the site starts (e.g. while the relay container starts).
func (s *Site) watchRelayTokenFile(ctx context.Context) {
	last := ""
	for {
		if b, err := os.ReadFile(s.cfg.RelayTokenFile); err == nil {
			if tok := strings.TrimSpace(string(b)); tok != "" && tok != last {
				if _, err := s.AddRelay(s.cfg.RelayAddress, tok); err != nil {
					s.log.Warn("relay token file", "err", err)
				} else {
					last = tok
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (s *Site) runRelay(t relay.Token, added int64) *relay.Client {
	id := relay.ID(t.Key)
	cert, _ := s.ID.Certificate()
	rc := &relay.Client{
		Token: t, Cert: cert, Name: s.Name(), Local: s.localEndpoints,
		OnIncoming: func(conn net.Conn, from string) { s.relayed.push(&relayedConn{Conn: conn, from: from}) },
		OnPunch:    s.onPunch,
		OnSecret: func(secret string) {
			if _, err := s.DB.Exec(`UPDATE relays SET secret = ? WHERE id = ?`, secret, id); err != nil {
				s.log.Warn("store new relay token", "relay", t.Address, "err", err)
				return
			}
			s.log.Info("relay token changed", "relay", t.Address)
		},
	}
	s.netMu.Lock()
	if s.quicTr != nil {
		rc.UDP = s.quicTr
	}
	if old := s.relays[id]; old != nil {
		old.cancel()
	}
	ctx := s.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	rctx, cancel := context.WithCancel(ctx)
	s.relays[id] = &relayEntry{client: rc, cancel: cancel, added: added}
	s.netMu.Unlock()
	go rc.Run(rctx)
	return rc
}

func (s *Site) relayClient(id string) *relay.Client {
	s.netMu.Lock()
	defer s.netMu.Unlock()
	if e := s.relays[id]; e != nil {
		return e.client
	}
	return nil
}

func (s *Site) relayClients() []*relay.Client {
	s.netMu.Lock()
	defer s.netMu.Unlock()
	out := make([]*relay.Client, 0, len(s.relays))
	for _, e := range s.relays {
		out = append(out, e.client)
	}
	return out
}

// relayAddr normalizes a relay address, adding the default port.
func relayAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return net.JoinHostPort(addr, "7444")
	}
	return addr
}

// AddRelay registers this site with a relay, given its address and token,
// and keeps a connection to it. address may be empty for tokens that include
// it (older tokens and invites). Peers learn the new route from this site's
// status.
func (s *Site) AddRelay(address, token string) (*RelayInfo, error) {
	t, err := relay.ParseToken(token)
	if err != nil {
		return nil, err
	}
	if a := relayAddr(address); a != "" {
		t.Address = a
	}
	if t.Address == "" {
		return nil, errors.New("the relay's address is required (host:7444)")
	}
	now := time.Now().Unix()
	if _, err := s.DB.Exec(`INSERT INTO relays(id, address, pubkey, secret, added) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET address = excluded.address, secret = excluded.secret`,
		relay.ID(t.Key), t.Address, []byte(t.Key), t.Secret, now); err != nil {
		return nil, err
	}
	if rc := s.relayClient(relay.ID(t.Key)); rc != nil && rc.Token.Address == t.Address && rc.CurrentToken().Secret == t.Secret {
		return &RelayInfo{Status: rc.Status(), Name: relayName(t.Address), Added: now}, nil
	}
	rc := s.runRelay(t, now)
	s.markDirty()
	s.notify(TopicPeers)
	s.log.Info("relay added", "relay", t.Address)
	return &RelayInfo{Status: rc.Status(), Name: relayName(t.Address), Added: now}, nil
}

// waitRelay waits until the relay is connected or the timeout passes.
func (s *Site) waitRelay(id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rc := s.relayClient(id); rc != nil && rc.Connected() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// SetRelayAddress changes the address of a relay, e.g. after it moved. The
// relay keeps its key, so registration and routes stay valid.
func (s *Site) SetRelayAddress(id, address string) error {
	address = relayAddr(address)
	if address == "" {
		return errors.New("the relay's address is required (host:7444)")
	}
	var key []byte
	var secret string
	var added int64
	if err := s.DB.QueryRow(`SELECT pubkey, secret, added FROM relays WHERE id = ?`, id).Scan(&key, &secret, &added); err != nil {
		return errors.New("no such relay")
	}
	if _, err := s.DB.Exec(`UPDATE relays SET address = ? WHERE id = ?`, address, id); err != nil {
		return err
	}
	s.runRelay(relay.Token{Address: address, Key: key, Secret: secret}, added)
	s.markDirty()
	s.notify(TopicPeers)
	return nil
}

func (s *Site) RemoveRelay(id string) error {
	res, err := s.DB.Exec(`DELETE FROM relays WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no such relay")
	}
	s.netMu.Lock()
	if e := s.relays[id]; e != nil {
		e.cancel()
		delete(s.relays, id)
	}
	s.netMu.Unlock()
	s.markDirty()
	s.notify(TopicPeers)
	return nil
}

func (s *Site) Relays() []RelayInfo {
	s.netMu.Lock()
	defer s.netMu.Unlock()
	out := []RelayInfo{}
	for _, e := range s.relays {
		out = append(out, RelayInfo{Status: e.client.Status(), Name: relayName(e.client.Token.Address), Added: e.added})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// relayTokens returns this site's relays, address included. Invites carry
// them, so a joining site can register with the same relays.
func (s *Site) relayTokens() []string {
	var out []string
	for _, rc := range s.relayClients() {
		out = append(out, rc.CurrentToken().EncodeWithAddress())
	}
	sort.Strings(out)
	return out
}

func tokenRef(t relay.Token) RelayRef { return RelayRef{Address: t.Address, Key: t.Key} }

// commonRelay returns a connected relay that the peer is also registered
// with, preferring the lowest round trip.
func (s *Site) commonRelay(routes Routes) *relay.Client {
	var best *relay.Client
	for _, r := range routes.Relays {
		rc := s.relayClient(relay.ID(ed25519.PublicKey(r.Key)))
		if rc == nil || !rc.Connected() {
			continue
		}
		if best == nil || rc.RTT() < best.RTT() {
			best = rc
		}
	}
	return best
}
