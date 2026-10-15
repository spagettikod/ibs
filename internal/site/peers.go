package site

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"ibs/internal/blockstore"
	"ibs/internal/db"
	"ibs/internal/keys"
	"ibs/internal/relay"
	"ibs/internal/version"
)

const (
	peerTimeout = 30 * time.Second
	inviteTTL   = 24 * time.Hour
)

var errPeerStatus = errors.New("peer request failed")

type PeerError struct {
	Status int
	Msg    string
}

func (e *PeerError) Error() string { return fmt.Sprintf("peer returned %d: %s", e.Status, e.Msg) }
func (e *PeerError) Unwrap() error { return errPeerStatus }

// newPinnedHTTP builds a client presenting our site certificate. If pin is
// non-nil the server must present that exact key; otherwise the key seen is
// reported through seen (trust on first use).
func (s *Site) newPinnedHTTP(pin ed25519.PublicKey, seen *ed25519.PublicKey) (*http.Client, error) {
	cert, err := s.ID.Certificate()
	if err != nil {
		return nil, err
	}
	return pinnedHTTP(cert, pin, seen), nil
}

func pinnedHTTP(cert tls.Certificate, pin ed25519.PublicKey, seen *ed25519.PublicKey) *http.Client {
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // replaced by key pinning below
		MinVersion:         tls.VersionTLS13,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("peer presented no certificate")
			}
			pub, err := keys.PubFromCert(cs.PeerCertificates[0])
			if err != nil {
				return err
			}
			if pin != nil && !pub.Equal(pin) {
				return errors.New("peer key does not match the pinned key for this site")
			}
			if seen != nil {
				*seen = pub
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   2 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: tlsCfg, MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second},
	}
}

func peerURL(addr, path string) string {
	addr = strings.TrimPrefix(strings.TrimPrefix(addr, "https://"), "http://")
	return "https://" + strings.TrimRight(addr, "/") + path
}

func isStatus(err error, code int) bool {
	var pe *PeerError
	return errors.As(err, &pe) && pe.Status == code
}

func (s *Site) peerGetBlock(ctx context.Context, siteID, id string) ([]byte, error) {
	var data []byte
	_, err := s.peerCall(ctx, siteID, http.MethodGet, "/peer/v1/blocks/"+id, nil, &data)
	if isStatus(err, http.StatusNotFound) {
		return nil, blockstore.ErrNotFound
	}
	return data, err
}

func (s *Site) peerPutBlock(ctx context.Context, siteID, id string, data []byte) error {
	_, err := s.peerCall(ctx, siteID, http.MethodPut, "/peer/v1/blocks/"+id, data, nil)
	return err
}

func (s *Site) peerDeleteBlocks(ctx context.Context, siteID string, ids []string) error {
	body, _ := json.Marshal(map[string][]string{"ids": ids})
	_, err := s.peerCall(ctx, siteID, http.MethodPost, "/peer/v1/blocks/delete", body, nil)
	return err
}

// ---- Status and network view ----

type NetSite struct {
	SiteID   string    `json:"site_id"`
	Name     string    `json:"name"`
	UpSince  int64     `json:"up_since"`
	Capacity int64     `json:"capacity"`
	Free     int64     `json:"free"`
	Online   bool      `json:"online"`
	LastSeen int64     `json:"last_seen"`
	Location *Location `json:"location,omitempty"` // where it's shown on the map, if it set one
	Version  string    `json:"version,omitempty"`
	// The oldest version it works with, as it reported it.
	MinCompatible string `json:"min_compatible,omitempty"`
}

// Member is what a site needs to connect to another site in the IBS. Sites
// share their members with the sites they are connected to, so a site that
// joins through one invite connects to every site in the IBS. Only sent over
// the mutually authenticated site-to-site connection.
type Member struct {
	SiteID      string   `json:"site_id"`
	Name        string   `json:"name"`
	PubKey      []byte   `json:"pubkey"`
	Address     string   `json:"address,omitempty"`
	RelayTokens []string `json:"relay_tokens,omitempty"` // with address and secret, like in an invite
	Joined      int64    `json:"joined,omitempty"`       // when it last joined, by its own clock
}

type SiteStatus struct {
	SiteID   string    `json:"site_id"`
	Name     string    `json:"name"`
	UpSince  int64     `json:"up_since"`
	Capacity int64     `json:"capacity"`
	Free     int64     `json:"free"`
	Network  []NetSite `json:"network"`
	Routes   Routes    `json:"routes"`
	Links    []Link    `json:"links"`
	// RelayTokens are this site's relays, so its members can register with
	// them; Members are the other sites in the IBS it knows how to reach.
	RelayTokens []string  `json:"relay_tokens,omitempty"`
	Members     []Member  `json:"members,omitempty"`
	Joined      int64     `json:"joined,omitempty"`
	Location    *Location `json:"location,omitempty"`
	Version     string    `json:"version,omitempty"`
	// Updates: the oldest version this one works with, and the versions it
	// holds (other sites may fetch those signed with a key they trust).
	MinCompatible string       `json:"min_compatible,omitempty"`
	Releases      []ReleaseRef `json:"releases,omitempty"`
}

func (s *Site) Status() SiteStatus {
	capacity, free := s.offeredStorage()
	st := SiteStatus{SiteID: s.ID.SiteID, Name: s.Name(), UpSince: s.started.Unix(), Capacity: capacity, Free: free}
	st.Network, _ = s.Network()
	st.Network = nonNil(st.Network)
	st.Routes = s.OwnRoutes()
	st.Links = s.NetworkLinks()
	st.RelayTokens = s.relayTokens()
	st.Members = s.members()
	st.Joined = s.SettingInt("joined_at")
	st.Location = s.Location()
	st.Version = version.Version
	st.MinCompatible = version.MinCompatible
	st.Releases = s.releaseRefs()
	return st
}

// members lists the sites this site can tell others how to reach: its own
// peers and the members it learned about from them.
func (s *Site) members() []Member {
	rows, err := s.DB.Query(`
		SELECT site_id, name, pubkey, address, relay_tokens, joined FROM peers
		UNION ALL
		SELECT site_id, name, pubkey, address, relay_tokens, joined FROM network
			WHERE length(pubkey) > 0 AND site_id NOT IN (SELECT site_id FROM peers)
				AND site_id NOT IN (SELECT site_id FROM left_sites)`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var tokens string
		if rows.Scan(&m.SiteID, &m.Name, &m.PubKey, &m.Address, &tokens, &m.Joined) != nil {
			continue
		}
		json.Unmarshal([]byte(tokens), &m.RelayTokens)
		out = append(out, m)
	}
	return out
}

// learnMembers records the members a peer reports, so this site can connect
// to them (see joinMembers). A member's site ID must match its key.
func (s *Site) learnMembers(ms []Member) {
	for _, m := range ms {
		if m.SiteID == s.ID.SiteID || len(m.PubKey) != ed25519.PublicKeySize || keys.SiteID(m.PubKey) != m.SiteID {
			continue
		}
		// A site that left is only taken back if it joined again since.
		var leftAt sql.NullInt64
		s.DB.QueryRow(`SELECT at FROM left_sites WHERE site_id = ?`, m.SiteID).Scan(&leftAt)
		if leftAt.Valid {
			if m.Joined <= leftAt.Int64 {
				continue
			}
			s.DB.Exec(`DELETE FROM left_sites WHERE site_id = ?`, m.SiteID)
		}
		tokens, _ := json.Marshal(nonNil(m.RelayTokens))
		s.DB.Exec(`INSERT INTO network(site_id, name, up_since, capacity, free, online, last_seen, pubkey, address, relay_tokens, joined)
			VALUES(?, ?, 0, 0, 0, 0, ?, ?, ?, ?, ?)
			ON CONFLICT(site_id) DO UPDATE SET pubkey = excluded.pubkey, address = excluded.address,
				relay_tokens = excluded.relay_tokens, joined = max(joined, excluded.joined)`,
			m.SiteID, m.Name, time.Now().Unix(), m.PubKey, m.Address, string(tokens), m.Joined)
	}
}

// joinMembers connects to the IBS members this site knows about but isn't
// connected to yet, except those it disconnected from. Every site in the IBS
// does the same, so a site that joined through one invite ends up connected
// to all of them, and they to it.
func (s *Site) joinMembers(ctx context.Context) {
	if s.OwnRoutes().empty() {
		return // nobody could reach this site
	}
	rows, err := s.DB.Query(`SELECT site_id FROM network WHERE length(pubkey) > 0
		AND site_id NOT IN (SELECT site_id FROM peers) AND site_id NOT IN (SELECT site_id FROM left_sites)`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := s.connectMember(ctx, id); err != nil {
			s.log.Warn("connect to IBS member", "site", id[:8], "err", err)
		}
	}
}

// connectMember adds a known IBS member as a peer. If the two sites share no
// relay and it has no direct address, this site registers with one of its
// relays first, as joining with an invite does.
func (s *Site) connectMember(ctx context.Context, siteID string) error {
	var name, address, tokensJSON string
	var pub []byte
	err := s.DB.QueryRow(`SELECT name, pubkey, address, relay_tokens FROM network WHERE site_id = ?`, siteID).
		Scan(&name, &pub, &address, &tokensJSON)
	if err != nil {
		return err
	}
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("its key is not known yet")
	}
	var tokens []string
	json.Unmarshal([]byte(tokensJSON), &tokens)
	routes := Routes{Address: address}
	mine := map[string]bool{}
	for _, rc := range s.relayClients() {
		mine[relay.ID(rc.Token.Key)] = true
	}
	shared := false
	var parsed []relay.Token
	for _, tok := range tokens {
		t, err := relay.ParseToken(tok)
		if err != nil || t.Address == "" {
			continue
		}
		parsed = append(parsed, t)
		routes.Relays = append(routes.Relays, tokenRef(t))
		shared = shared || mine[relay.ID(t.Key)]
	}
	if routes.empty() {
		return errors.New("no way to reach it is known yet")
	}
	if !shared && address == "" {
		if _, err := s.AddRelay("", parsed[0].EncodeWithAddress()); err != nil {
			return fmt.Errorf("register with its relay %s: %w", parsed[0].Address, err)
		}
	}
	if err := s.addPeer(siteID, name, ed25519.PublicKey(pub), routes); err != nil {
		return err
	}
	s.DB.Exec(`UPDATE peers SET relay_tokens = ? WHERE site_id = ?`, tokensJSON, siteID)
	s.log.Info("connected to a site in the IBS", "peer", siteID[:8], "name", name)
	go s.checkPeer(context.Background(), siteID)
	return nil
}

// leftPrefix starts the answer a site that left the IBS gives its former
// members: "ibs-left:<unix time it left>: <message>".
const leftPrefix = "ibs-left:"

// leftAt reports whether a peer answered that it left the IBS, and when.
func leftAt(err error) (int64, bool) {
	var pe *PeerError
	if !errors.As(err, &pe) || pe.Status != http.StatusForbidden || !strings.HasPrefix(pe.Msg, leftPrefix) {
		return 0, false
	}
	at, _ := strconv.ParseInt(strings.SplitN(strings.TrimPrefix(pe.Msg, leftPrefix), ":", 2)[0], 10, 64)
	return at, true
}

// memberLeft forgets a site that left the IBS, and records when, so other
// sites' lists don't bring it back unless it rejoins later.
func (s *Site) memberLeft(ctx context.Context, siteID string, at int64) {
	s.log.Info("site left the IBS", "site", siteID[:8])
	s.DB.Exec(`INSERT INTO left_sites(site_id, at) VALUES(?, ?) ON CONFLICT(site_id) DO UPDATE SET at = max(at, excluded.at)`, siteID, at)
	s.DB.Exec(`DELETE FROM network WHERE site_id = ?`, siteID)
	s.forgetPeer(ctx, siteID)
	s.notify(TopicPeers)
}

// admitMember connects to an IBS member that contacts this site before this
// site has connected to it: its key must be one this site learned from its
// peers. It reports whether the caller is now a peer.
func (s *Site) admitMember(pub ed25519.PublicKey) bool {
	id := keys.SiteID(pub)
	var known []byte
	s.DB.QueryRow(`SELECT pubkey FROM network WHERE site_id = ? AND site_id NOT IN (SELECT site_id FROM left_sites)`, id).Scan(&known)
	if !ed25519.PublicKey(known).Equal(pub) {
		return false
	}
	if err := s.connectMember(context.Background(), id); err != nil {
		s.log.Warn("admit IBS member", "site", id[:8], "err", err)
		return false
	}
	return true
}

// isFormerMember reports whether this site left the IBS that siteID is in.
func (s *Site) isFormerMember(siteID string) bool {
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM former_members WHERE site_id = ?`, siteID).Scan(&n)
	return n > 0
}

// LeaveIBS takes this site out of the IBS: it tells every site it is
// connected to, which forget it and delete the copies they keep for it (and
// make new copies of their own data elsewhere). This site forgets them and
// deletes the copies it keeps for them; its own files stay. Sites that can't
// be reached now are told when they next call. It can join again later with
// an invite.
func (s *Site) LeaveIBS(ctx context.Context) error {
	now := time.Now().Unix()
	s.DB.Exec(`INSERT OR IGNORE INTO former_members(site_id) SELECT site_id FROM peers UNION SELECT site_id FROM network`)
	db.SetSetting(s.DB, "left_at", strconv.FormatInt(now, 10))
	rows, err := s.DB.Query(`SELECT site_id FROM peers`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	body, _ := json.Marshal(map[string]int64{"at": now})
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			s.peerCall(cctx, id, http.MethodPost, "/peer/v1/leave", body, nil)
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if err := s.forgetPeer(ctx, id); err != nil {
			return err
		}
	}
	s.DB.Exec(`DELETE FROM network`)
	s.log.Info("left the IBS", "sites", len(ids))
	s.markDirty()
	s.notify(TopicPeers)
	return nil
}

// joined records that this site joined an IBS (now); other members use it to
// tell a rejoin from an old leave.
func (s *Site) joined() {
	db.SetSetting(s.DB, "joined_at", strconv.FormatInt(time.Now().Unix(), 10))
	s.DB.Exec(`DELETE FROM former_members`)
}

// Network lists every known site except this one: directly paired peers
// first-hand, and the rest as reported by peers.
func (s *Site) Network() ([]NetSite, error) {
	rows, err := s.DB.Query(`
		SELECT site_id, name, up_since, capacity, free, online, last_seen, location, version, min_compatible FROM peers
		UNION ALL
		SELECT site_id, name, up_since, capacity, free, online, last_seen, location, version, min_compatible FROM network
			WHERE site_id NOT IN (SELECT site_id FROM peers)
		ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NetSite
	for rows.Next() {
		var n NetSite
		var loc string
		if err := rows.Scan(&n.SiteID, &n.Name, &n.UpSince, &n.Capacity, &n.Free, &n.Online, &n.LastSeen, &loc, &n.Version, &n.MinCompatible); err != nil {
			return nil, err
		}
		n.Location = parseLocation(loc)
		if n.SiteID != s.ID.SiteID {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}

func (s *Site) checkPeers(ctx context.Context) {
	rows, err := s.DB.Query(`SELECT site_id FROM peers`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	done := make(chan struct{}, len(ids))
	for _, id := range ids {
		go func() {
			s.checkPeer(ctx, id)
			done <- struct{}{}
		}()
	}
	for range ids {
		<-done
	}
	// Forget sites nobody has reported for a week.
	s.DB.Exec(`DELETE FROM network WHERE last_seen < ?`, time.Now().Add(-7*24*time.Hour).Unix())
	s.joinMembers(ctx)
}

func (s *Site) checkPeer(ctx context.Context, siteID string) {
	var st SiteStatus
	_, err := s.peerCall(ctx, siteID, http.MethodGet, "/peer/v1/status", nil, &st)
	now := time.Now().Unix()
	if at, ok := leftAt(err); ok {
		s.memberLeft(ctx, siteID, at)
		return
	}
	if err != nil {
		res, _ := s.DB.Exec(`UPDATE peers SET online = 0, last_error = ? WHERE site_id = ? AND online = 1`, err.Error(), siteID)
		if n, _ := res.RowsAffected(); n > 0 {
			s.notify(TopicPeers)
			s.log.Warn("peer went offline", "peer", siteID[:8], "err", err)
		} else {
			s.DB.Exec(`UPDATE peers SET last_error = ? WHERE site_id = ?`, err.Error(), siteID)
		}
		return
	}
	var wasOnline int
	s.DB.QueryRow(`SELECT online FROM peers WHERE site_id = ?`, siteID).Scan(&wasOnline)
	s.DB.Exec(`UPDATE peers SET name = ?, online = 1, last_seen = ?, up_since = ?, capacity = ?, free = ?, last_error = ''
		WHERE site_id = ?`, st.Name, now, st.UpSince, st.Capacity, st.Free, siteID)
	if !st.Routes.empty() {
		s.setPeerRoutes(siteID, st.Routes)
	}
	s.learnLinks(st.Links)
	if len(st.RelayTokens) > 0 {
		tj, _ := json.Marshal(st.RelayTokens)
		s.DB.Exec(`UPDATE peers SET relay_tokens = ? WHERE site_id = ? AND relay_tokens != ?`, string(tj), siteID, string(tj))
	}
	s.DB.Exec(`UPDATE peers SET joined = ? WHERE site_id = ? AND joined != ?`, st.Joined, siteID, st.Joined)
	loc := locationJSON(st.Location)
	s.DB.Exec(`UPDATE peers SET location = ? WHERE site_id = ? AND location != ?`, loc, siteID, loc)
	s.DB.Exec(`UPDATE peers SET version = ? WHERE site_id = ? AND version != ?`, st.Version, siteID, st.Version)
	rj, _ := json.Marshal(nonNil(st.Releases))
	s.DB.Exec(`UPDATE peers SET releases = ?, min_compatible = ? WHERE site_id = ?`, string(rj), st.MinCompatible, siteID)
	s.learnMembers(st.Members)
	for _, n := range st.Network {
		if n.SiteID == s.ID.SiteID || n.SiteID == "" {
			continue
		}
		s.DB.Exec(`INSERT INTO network(site_id, name, up_since, capacity, free, online, last_seen, location, version, min_compatible) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(site_id) DO UPDATE SET name = excluded.name, up_since = excluded.up_since, capacity = excluded.capacity,
				free = excluded.free, online = excluded.online, last_seen = excluded.last_seen, location = excluded.location,
				version = excluded.version, min_compatible = excluded.min_compatible
			WHERE excluded.last_seen >= network.last_seen`,
			n.SiteID, n.Name, n.UpSince, n.Capacity, n.Free, n.Online, n.LastSeen, locationJSON(n.Location), n.Version, n.MinCompatible)
	}
	if wasOnline == 0 {
		s.notify(TopicPeers)
		s.log.Info("peer is online", "peer", siteID[:8], "name", st.Name)
		select {
		case s.reconcile <- siteID:
		default:
		}
	}
}

// ---- Pairing ----

type PeerInfo struct {
	SiteID      string   `json:"site_id"`
	Name        string   `json:"name"`
	Address     string   `json:"address"`
	Added       int64    `json:"added"`
	Online      bool     `json:"online"`
	LastSeen    int64    `json:"last_seen"`
	UpSince     int64    `json:"up_since"`
	Capacity    int64    `json:"capacity"`
	Free        int64    `json:"free"`
	LastError   string   `json:"last_error"`
	Reconciled  int64    `json:"reconciled"`
	ReplicasOut int64    `json:"replicas_out"` // our blocks stored there
	BytesOut    int64    `json:"bytes_out"`
	HeldBlocks  int64    `json:"held_blocks"` // their blocks stored here
	HeldBytes   int64    `json:"held_bytes"`
	Path        PathInfo `json:"path"`
	Relays      int      `json:"relays"` // relays the site can be reached through
	Version     string   `json:"version"`
}

func (s *Site) Peers() ([]PeerInfo, error) {
	rows, err := s.DB.Query(`SELECT p.site_id, p.name, p.address, p.added, p.online, p.last_seen, p.up_since,
			p.capacity, p.free, p.last_error, p.reconciled,
			(SELECT COUNT(*) FROM replicas r WHERE r.site_id = p.site_id AND r.state = 'ok'),
			(SELECT COALESCE(SUM(b.size), 0) FROM replicas r JOIN blocks b ON b.id = r.block_id WHERE r.site_id = p.site_id AND r.state = 'ok'),
			(SELECT COUNT(*) FROM held_blocks h WHERE h.owner = p.site_id),
			(SELECT COALESCE(SUM(size), 0) FROM held_blocks h WHERE h.owner = p.site_id),
			p.routes, p.version
		FROM peers p ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeerInfo
	for rows.Next() {
		var p PeerInfo
		var routesJSON string
		if err := rows.Scan(&p.SiteID, &p.Name, &p.Address, &p.Added, &p.Online, &p.LastSeen, &p.UpSince,
			&p.Capacity, &p.Free, &p.LastError, &p.Reconciled, &p.ReplicasOut, &p.BytesOut, &p.HeldBlocks, &p.HeldBytes, &routesJSON, &p.Version); err != nil {
			return nil, err
		}
		p.Path = s.pathInfo(p.SiteID)
		var rt Routes
		json.Unmarshal([]byte(routesJSON), &rt)
		p.Relays = len(rt.Relays)
		out = append(out, p)
	}
	return out, rows.Err()
}

// invite carries what a site needs to pair with the issuer: its key, a
// one-time secret, and how to reach it. Relay tokens let the joining site
// register with the issuer's relays. See invite.go for how it's written.
type invite struct {
	Address string   `json:"a,omitempty"`
	PubKey  string   `json:"k"`
	Secret  string   `json:"s"`
	Name    string   `json:"n,omitempty"` // only in ibs1 invites
	Relays  []string `json:"r,omitempty"`
}

// CreateInvite returns a token another site's admin pastes to pair with us.
func (s *Site) CreateInvite() (string, error) {
	addr := s.Setting("peer_advertise")
	relays := s.relayTokens()
	if addr == "" && len(relays) == 0 {
		return "", errors.New("other sites cannot reach this site yet: add a relay or set a direct address on The Known Universe page")
	}
	secret := keys.RandomToken(12) // one use within a day: 96 bits is plenty
	now := time.Now()
	if _, err := s.DB.Exec(`INSERT INTO invites(secret, created, expires) VALUES(?, ?, ?)`,
		secret, now.Unix(), now.Add(inviteTTL).Unix()); err != nil {
		return "", err
	}
	return encodeInvite(invite{Address: addr, PubKey: base64.RawURLEncoding.EncodeToString(s.ID.Pub), Secret: secret, Relays: relays})
}

type pairRequest struct {
	Secret  string `json:"secret"`
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
	Routes  Routes `json:"routes"`
}

type pairResponse struct {
	SiteID string `json:"site_id"`
	Name   string `json:"name"`
	Routes Routes `json:"routes"`
}

// JoinWithInvite pairs this site with the site that issued the invite.
func (s *Site) JoinWithInvite(ctx context.Context, token string) (*PeerInfo, error) {
	inv, err := decodeInvite(token)
	if err != nil {
		return nil, err
	}
	if inv.Name == "" {
		inv.Name = "the inviting site"
	}
	pubRaw, err := base64.RawURLEncoding.DecodeString(inv.PubKey)
	if err != nil || len(pubRaw) != ed25519.PublicKeySize {
		return nil, errors.New("invite token has an invalid key")
	}
	pub := ed25519.PublicKey(pubRaw)
	if keys.SiteID(pub) == s.ID.SiteID {
		return nil, errors.New("this invite was created by this site")
	}
	// Register with the issuer's relays so both sites share a way to talk.
	routes := Routes{Address: inv.Address}
	for _, tok := range inv.Relays {
		t, err := relay.ParseToken(tok)
		if err != nil {
			continue
		}
		if _, err := s.AddRelay("", tok); err != nil {
			return nil, fmt.Errorf("register with relay %s: %w", t.Address, err)
		}
		if !s.waitRelay(relay.ID(t.Key), 15*time.Second) {
			s.log.Warn("relay from invite not reachable", "relay", t.Address)
		}
		routes.Relays = append(routes.Relays, tokenRef(t))
	}
	own := s.OwnRoutes()
	if own.empty() {
		return nil, errors.New("other sites cannot reach this site yet: add a relay or set a direct address on The Known Universe page")
	}
	siteID := keys.SiteID(pub)
	pc := s.newPeerClient(siteID, pub, routes)
	body, _ := json.Marshal(pairRequest{Secret: inv.Secret, Name: s.Name(), Address: own.Address, Routes: own})
	var resp pairResponse
	if _, err := pc.call(ctx, http.MethodPost, "/peer/v1/pair", body, &resp); err != nil {
		return nil, fmt.Errorf("contact %s: %w", inv.Name, err)
	}
	if !resp.Routes.empty() {
		routes = resp.Routes
	}
	s.DB.Exec(`DELETE FROM left_sites WHERE site_id = ?`, siteID)
	if err := s.addPeer(siteID, resp.Name, pub, routes); err != nil {
		return nil, err
	}
	s.joined()
	s.checkPeer(ctx, siteID)
	return &PeerInfo{SiteID: siteID, Name: resp.Name, Address: routes.Address}, nil
}

// setPeerRoutes records the routes a peer announces in its status.
func (s *Site) setPeerRoutes(siteID string, routes Routes) {
	rj, _ := json.Marshal(Routes{Relays: routes.Relays})
	s.DB.Exec(`UPDATE peers SET address = ?, routes = ? WHERE site_id = ? AND (address != ? OR routes != ?)`,
		routes.Address, string(rj), siteID, routes.Address, string(rj))
}

func (s *Site) addPeer(siteID, name string, pub ed25519.PublicKey, routes Routes) error {
	rj, _ := json.Marshal(Routes{Relays: routes.Relays})
	_, err := s.DB.Exec(`INSERT INTO peers(site_id, name, pubkey, address, routes, added) VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(site_id) DO UPDATE SET name = excluded.name, address = excluded.address, routes = excluded.routes`,
		siteID, name, []byte(pub), routes.Address, string(rj), time.Now().Unix())
	if err == nil {
		s.markDirty()
		s.DB.Exec(`DELETE FROM network WHERE site_id = ?`, siteID)
		s.notify(TopicPeers)
	}
	return err
}

// redeemInvite is the server side of pairing.
func (s *Site) redeemInvite(pub ed25519.PublicKey, req pairRequest) (*pairResponse, error) {
	now := time.Now().Unix()
	res, err := s.DB.Exec(`UPDATE invites SET used_by = ? WHERE secret = ? AND used_by = '' AND expires > ?`,
		keys.SiteID(pub), req.Secret, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, errors.New("invite is invalid, expired or already used")
	}
	routes := req.Routes
	if routes.Address == "" {
		routes.Address = req.Address
	}
	if routes.empty() {
		return nil, errors.New("joining site must provide a way to reach it (relay or address)")
	}
	name := req.Name
	if name == "" {
		name = keys.SiteID(pub)[:8]
	}
	s.DB.Exec(`DELETE FROM left_sites WHERE site_id = ?`, keys.SiteID(pub))
	var peers int
	s.DB.QueryRow(`SELECT COUNT(*) FROM peers`).Scan(&peers)
	if err := s.addPeer(keys.SiteID(pub), name, pub, routes); err != nil {
		return nil, err
	}
	if peers == 0 {
		s.joined() // a site on its own that invites another starts (or rejoins) an IBS
	}
	s.log.Info("paired with site", "peer", keys.SiteID(pub)[:8], "name", name)
	go s.checkPeer(context.Background(), keys.SiteID(pub))
	return &pairResponse{SiteID: s.ID.SiteID, Name: s.Name(), Routes: s.OwnRoutes()}, nil
}

func (s *Site) forgetPeer(ctx context.Context, siteID string) error {
	err := s.inTx(func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM peers WHERE site_id = ?`,
			`DELETE FROM replicas WHERE site_id = ?`,
		} {
			if _, err := tx.Exec(q, siteID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.peerMu.Lock()
	if pc := s.peerClients[siteID]; pc != nil {
		pc.mu.Lock()
		if pc.quic != nil {
			pc.quic.close()
		}
		pc.mu.Unlock()
	}
	delete(s.peerClients, siteID)
	s.peerMu.Unlock()
	s.dropHeldBlocks(ctx, siteID)
	s.notify(TopicPeers, TopicReplication)
	return nil
}

func (s *Site) dropHeldBlocks(ctx context.Context, owner string) {
	rows, err := s.DB.Query(`SELECT id, node_id FROM held_blocks WHERE owner = ?`, owner)
	if err != nil {
		return
	}
	type hb struct{ id, node string }
	var all []hb
	for rows.Next() {
		var h hb
		rows.Scan(&h.id, &h.node)
		all = append(all, h)
	}
	rows.Close()
	for _, h := range all {
		if st := s.nodeStore(h.node); st != nil {
			st.Delete(ctx, owner, h.id)
		}
	}
	s.DB.Exec(`DELETE FROM held_blocks WHERE owner = ?`, owner)
	s.DB.Exec(`DELETE FROM peer_backups WHERE owner = ?`, owner)
	removeBackupFile(s.cfg.DataDir, owner)
}

// verifyPeerCert maps a TLS client certificate to a paired site id.
func (s *Site) verifyPeerCert(certs []*x509.Certificate) (string, ed25519.PublicKey, bool) {
	if len(certs) == 0 {
		return "", nil, false
	}
	pub, err := keys.PubFromCert(certs[0])
	if err != nil {
		return "", nil, false
	}
	id := keys.SiteID(pub)
	var stored []byte
	if err := s.DB.QueryRow(`SELECT pubkey FROM peers WHERE site_id = ?`, id).Scan(&stored); err != nil {
		return id, pub, false
	}
	return id, pub, pub.Equal(ed25519.PublicKey(stored))
}

func parseInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
