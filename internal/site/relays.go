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
	if s.cfg.RelayToken != "" && !s.relayRemoved(s.cfg.RelayToken, "IBS_RELAY_TOKEN") {
		if _, err := s.addConfiguredRelay(s.cfg.RelayToken); err != nil {
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
			if tok := strings.TrimSpace(string(b)); tok != "" && tok != last && !s.relayRemoved(tok, "IBS_RELAY_TOKEN_FILE") {
				if _, err := s.addConfiguredRelay(tok); err != nil {
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

// addConfiguredRelay adds the relay from IBS_RELAY_ADDRESS and the token. When
// the relay's address was changed since (here or on another site, see
// followRelayMoves), that address is kept: the configured one may be its old
// name.
func (s *Site) addConfiguredRelay(token string) (*RelayInfo, error) {
	address := s.cfg.RelayAddress
	if t, err := relay.ParseToken(token); err == nil {
		var current string
		var set int64
		if s.DB.QueryRow(`SELECT address, address_set FROM relays WHERE id = ?`, relay.ID(t.Key)).Scan(&current, &set) == nil &&
			set > 0 && current != relayAddr(address) {
			if address != "" {
				s.log.Info("the relay's address was changed since; using the new one instead of IBS_RELAY_ADDRESS", "relay", current, "configured", address)
			}
			address = current
		}
	}
	return s.AddRelay(address, token)
}

// relayRemoved reports whether the relay of a configured token was removed by
// hand (here or on another site) and isn't in use: the configuration doesn't
// bring it back.
func (s *Site) relayRemoved(token, setting string) bool {
	t, err := relay.ParseToken(token)
	if err != nil {
		return false
	}
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM removed_relays WHERE id = ? AND id NOT IN (SELECT id FROM relays)`, relay.ID(t.Key)).Scan(&n)
	if n > 0 {
		s.log.Warn(setting+" names a relay that was removed; remove it from the configuration, or add the relay again under The Known Universe", "relay", t.Address)
	}
	return n > 0
}

func (s *Site) runRelay(t relay.Token, added int64) *relay.Client {
	id := relay.ID(t.Key)
	cert, _ := s.ID.Certificate()
	rc := &relay.Client{
		Token: t, Cert: cert, Name: s.Name(), Local: s.localEndpoints,
		OnIncoming: func(conn net.Conn, from string) {
			if n, ok := strings.CutPrefix(from, "code:"); ok {
				if strings.HasPrefix(n, "i") {
					// A site joining with a long invite: it fetches the full
					// invite from the peer server. The relay put it through
					// once; claim the name again until the invite is used.
					s.relayed.push(&relayedConn{Conn: conn, from: from})
					go s.reclaimInvite(s.relayClient(id), n)
					return
				}
				go s.answerCode(conn, n) // a site joining with an invite code
				return
			}
			s.relayed.push(&relayedConn{Conn: conn, from: from})
		},
		OnPunch: s.onPunch,
		Vouched: s.peerKeys,
		OnSecret: func(secret string) {
			if _, err := s.DB.Exec(`UPDATE relays SET secret = ? WHERE id = ?`, secret, id); err != nil {
				s.log.Warn("store new relay token", "relay", t.Address, "err", err)
				return
			}
			s.log.Info("relay token changed", "relay", t.Address)
		},
	}
	rc.OnConnect = func() { s.reclaimCodes(rc) }
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

// peerKeys are the keys of this site's peers, which it vouches for on its
// relays: a peer restored from its recovery phrase can then come back through
// any of them.
func (s *Site) peerKeys() []ed25519.PublicKey {
	rows, err := s.DB.Query(`SELECT pubkey FROM peers WHERE length(pubkey) = ? ORDER BY site_id`, ed25519.PublicKeySize)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ed25519.PublicKey
	for rows.Next() {
		var k []byte
		if rows.Scan(&k) == nil {
			out = append(out, k)
		}
	}
	return out
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
	return s.addRelay(address, token, time.Now().Unix())
}

// addRelay adds a relay as added at the given time: now, or when it was added
// on the site this one learned it from (followRelayChanges).
func (s *Site) addRelay(address, token string, added int64) (*RelayInfo, error) {
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
	now := added
	if _, err := s.DB.Exec(`INSERT INTO relays(id, address, pubkey, secret, added) VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET address = excluded.address, secret = excluded.secret`,
		relay.ID(t.Key), t.Address, []byte(t.Key), t.Secret, now); err != nil {
		return nil, err
	}
	s.DB.Exec(`DELETE FROM removed_relays WHERE id = ? AND at <= ?`, relay.ID(t.Key), added)
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

// SetRelayAddress changes the address of a relay, e.g. after it moved or was
// renamed. The relay keeps its key, so registration and routes stay valid.
// Other sites using the relay take the new address too (followRelayMoves).
func (s *Site) SetRelayAddress(id, address string) error {
	address = relayAddr(address)
	if address == "" {
		return errors.New("the relay's address is required (host:7444)")
	}
	return s.setRelayAddress(id, address, time.Now().Unix())
}

// setRelayAddress changes a relay's address, recording when it was set (by
// hand here, or on the site this one learned it from).
func (s *Site) setRelayAddress(id, address string, set int64) error {
	var key []byte
	var secret string
	var added int64
	if err := s.DB.QueryRow(`SELECT pubkey, secret, added FROM relays WHERE id = ?`, id).Scan(&key, &secret, &added); err != nil {
		return errors.New("no such relay")
	}
	if _, err := s.DB.Exec(`UPDATE relays SET address = ?, address_set = ? WHERE id = ?`, address, set, id); err != nil {
		return err
	}
	s.runRelay(relay.Token{Address: address, Key: key, Secret: secret}, added)
	s.markDirty()
	s.notify(TopicPeers)
	return nil
}

// RemoveRelay stops using a relay. The other sites in the IBS stop using it too
// (followRelayChanges).
func (s *Site) RemoveRelay(id string) error {
	return s.removeRelay(id, time.Now().Unix())
}

// removeRelay removes a relay, recording when it was removed (here, or on the
// site this one learned it from).
func (s *Site) removeRelay(id string, at int64) error {
	res, err := s.DB.Exec(`DELETE FROM relays WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no such relay")
	}
	s.DB.Exec(`INSERT INTO removed_relays(id, at) VALUES(?, ?) ON CONFLICT(id) DO UPDATE SET at = max(at, excluded.at)`, id, at)
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

// RelayAddress is a relay a site uses: its address, when that was set by hand
// (0 = never), and when the relay was added. The most recently set address,
// and the latest of adding and removing a relay, spread to other sites.
type RelayAddress struct {
	Relay   string `json:"relay"`
	Address string `json:"address"`
	Set     int64  `json:"set"`
	Added   int64  `json:"added,omitempty"`
}

// RelayRemoval is a relay removed by hand on some site, and when.
type RelayRemoval struct {
	Relay string `json:"relay"`
	At    int64  `json:"at"`
}

func (s *Site) relayAddresses() []RelayAddress {
	rows, err := s.DB.Query(`SELECT id, address, address_set, added FROM relays ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []RelayAddress
	for rows.Next() {
		var a RelayAddress
		rows.Scan(&a.Relay, &a.Address, &a.Set, &a.Added)
		out = append(out, a)
	}
	return out
}

func (s *Site) relayRemovals() []RelayRemoval {
	rows, err := s.DB.Query(`SELECT id, at FROM removed_relays ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []RelayRemoval
	for rows.Next() {
		var r RelayRemoval
		rows.Scan(&r.Relay, &r.At)
		out = append(out, r)
	}
	return out
}
