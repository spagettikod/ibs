package site

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"ibs/internal/keys"
	"ibs/internal/relay"
)

// The long invite ("ibs3.", since 1.8.5) is as short as it can be: it only
// says how to reach the inviting site and holds the one-time secret. The
// full invite (relay tokens and all, the "ibs2." form) is fetched from the
// inviting site with the secret, over TLS pinned to the site's ID:
//
//	siteID[16] secret[12] len direct { len relay }
//
// Through a relay, the joining site needs no relay token: the inviting site
// claims a name on each of its relays, made from the secret, and the relay
// puts the joining site through to it once (like a short code). Addresses
// leave out their default ports.
const longInvitePrefix = "ibs3."

type longInvite struct {
	SiteID  string   // hex, as keys.SiteID
	Secret  string   // as stored in invites: base64url of 12 bytes
	Address string   // direct address, or ""
	Relays  []string // relay addresses
}

func encodeLongInvite(li longInvite) (string, error) {
	id, err := hex.DecodeString(li.SiteID)
	if err != nil || len(id) != 16 {
		return "", errors.New("invalid site ID")
	}
	secret, err := base64.RawURLEncoding.DecodeString(li.Secret)
	if err != nil || len(secret) != 12 {
		return "", errors.New("invalid invite secret")
	}
	var b bytes.Buffer
	b.Write(id)
	b.Write(secret)
	str := func(s string) error {
		if len(s) > 255 {
			return errors.New("invite address too long")
		}
		b.WriteByte(byte(len(s)))
		b.WriteString(s)
		return nil
	}
	if err := str(strings.TrimSuffix(li.Address, ":7443")); err != nil {
		return "", err
	}
	for _, r := range li.Relays {
		if err := str(strings.TrimSuffix(r, ":7444")); err != nil {
			return "", err
		}
	}
	return longInvitePrefix + base64.RawURLEncoding.EncodeToString(b.Bytes()), nil
}

func decodeLongInvite(token string) (longInvite, error) {
	token = strings.Join(strings.Fields(token), "") // pasted invites may wrap
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, longInvitePrefix))
	if err != nil || len(raw) < 29 {
		return longInvite{}, errBadInvite
	}
	li := longInvite{SiteID: hex.EncodeToString(raw[:16]), Secret: base64.RawURLEncoding.EncodeToString(raw[16:28])}
	r := bytes.NewReader(raw[28:])
	str := func() (string, error) {
		n, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(r, p); err != nil {
			return "", errBadInvite
		}
		return string(p), nil
	}
	if li.Address, err = str(); err != nil {
		return longInvite{}, errBadInvite
	}
	if li.Address != "" {
		if _, _, err := net.SplitHostPort(li.Address); err != nil {
			li.Address = net.JoinHostPort(li.Address, "7443")
		}
	}
	for r.Len() > 0 {
		a, err := str()
		if err != nil {
			return longInvite{}, errBadInvite
		}
		li.Relays = append(li.Relays, relayAddr(a))
	}
	return li, nil
}

// inviteName is the name an invite is claimed under on the inviting site's
// relays: from the secret, which the relay never sees.
func inviteName(secret string) string {
	h := sha256.Sum256([]byte("ibs invite " + secret))
	return "i" + hex.EncodeToString(h[:10])
}

// claimInvite claims an invite's name on every connected relay, so a site
// joining with it gets through to this one without a relay token.
func (s *Site) claimInvite(ctx context.Context, secret string, expires time.Time) {
	name := inviteName(secret)
	if _, err := s.DB.Exec(`INSERT INTO invite_codes(number, relay_id, words, invite, expires) VALUES(?, '*', '', '', ?)
		ON CONFLICT(number) DO NOTHING`, name, expires.Unix()); err != nil {
		s.log.Warn("store invite claim", "err", err)
	}
	for _, rc := range s.relayClients() {
		if !rc.Connected() {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := rc.ClaimCode(cctx, name, expires); err != nil {
			s.log.Warn("claim invite on relay", "relay", rc.Token.Address, "err", err)
		}
		cancel()
	}
}

// reclaimInvite claims a long invite's name on a relay again after a site
// came through with it, unless the invite has been used by then: a joining
// site that fails halfway can try again.
func (s *Site) reclaimInvite(rc *relay.Client, name string) {
	time.Sleep(5 * time.Second)
	if rc == nil {
		return
	}
	var expires int64
	if s.DB.QueryRow(`SELECT expires FROM invite_codes WHERE number = ? AND used = 0 AND expires > ?`, name, time.Now().Unix()).Scan(&expires) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rc.ClaimCode(ctx, name, time.Unix(expires, 0))
}

// fullInvite answers a joining site that holds a valid, unused invite secret
// with the full invite. Pairing (redeemInvite) spends the secret.
func (s *Site) fullInvite(secret string) (string, error) {
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM invites WHERE secret = ? AND used_by = '' AND expires > ?`, secret, time.Now().Unix()).Scan(&n)
	if n == 0 {
		return "", errors.New("invite is invalid, expired or already used")
	}
	return encodeInvite(invite{Address: s.Setting("peer_advertise"), PubKey: base64.RawURLEncoding.EncodeToString(s.ID.Pub),
		Secret: secret, Relays: s.relayTokens()})
}

func (s *Site) handleInviteFetch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	full, err := s.fullInvite(req.Secret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	writeJSON(w, map[string]string{"invite": full})
}

// JoinWithLongInvite reaches the inviting site by its direct address or one
// of its relays, fetches the full invite and joins with it.
func (s *Site) JoinWithLongInvite(ctx context.Context, token string) (*PeerInfo, error) {
	li, err := decodeLongInvite(token)
	if err != nil {
		return nil, err
	}
	if li.SiteID == s.ID.SiteID {
		return nil, errors.New("this invite was created by this site")
	}
	cert, err := s.ID.Certificate()
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"secret": li.Secret})
	// fetch asks the inviting site over one route for the full invite.
	fetch := func(h *http.Client, base string, seen *ed25519.PublicKey) (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/peer/v1/invite", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if keys.SiteID(*seen) != li.SiteID {
			return "", errors.New("the site there isn't the one that made the invite")
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode != http.StatusOK {
			return "", errors.New(strings.TrimSpace(string(b)))
		}
		var out struct {
			Invite string `json:"invite"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return "", err
		}
		return out.Invite, nil
	}
	var errs []string
	var full string
	if li.Address != "" {
		var seen ed25519.PublicKey
		h := pinnedHTTP(cert, nil, &seen)
		h.Timeout = 30 * time.Second
		if full, err = fetch(h, peerURL(li.Address, ""), &seen); err != nil {
			errs = append(errs, li.Address+": "+err.Error())
		}
	}
	for _, addr := range li.Relays {
		if full != "" {
			break
		}
		var seen ed25519.PublicKey
		h := pinnedHTTP(cert, nil, &seen)
		h.Timeout = 30 * time.Second
		h.Transport.(*http.Transport).DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return relay.DialCode(ctx, addr, inviteName(li.Secret), cert)
		}
		h.Transport.(*http.Transport).DisableKeepAlives = true
		if full, err = fetch(h, "https://peer.ibs", &seen); err != nil {
			errs = append(errs, "relay "+addr+": "+err.Error())
		}
	}
	if full == "" {
		return nil, fmt.Errorf("can't reach the inviting site (%s)", strings.Join(errs, "; "))
	}
	inv, err := decodeInvite(full)
	if err != nil {
		return nil, err
	}
	if pub, err := base64.RawURLEncoding.DecodeString(inv.PubKey); err != nil || keys.SiteID(ed25519.PublicKey(pub)) != li.SiteID {
		return nil, errors.New("the inviting site sent an invite that isn't its own")
	}
	return s.JoinWithInvite(ctx, full)
}
