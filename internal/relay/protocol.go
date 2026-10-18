// Package relay lets IBS sites reach each other without exposing ports.
//
// A relay is a small, stateless service on a machine with one open port
// (TCP and UDP). Sites keep an outbound control connection to it. When site A
// wants to talk to site B, A opens a connection to the relay asking for B;
// the relay asks B (over its control connection) to open a matching
// connection and then splices the two. Sites run their normal mutually
// authenticated TLS end to end through the splice, so the relay only ever
// forwards ciphertext and cannot read or impersonate anything.
//
// The relay also helps sites connect directly: over UDP it tells a site its
// public address, and on request it exchanges the address candidates of two
// sites so they can punch through their NATs (see the site package).
//
// Only registered sites may use a relay. A site registers by presenting the
// relay token's secret once; after that its key is on the relay's allowlist.
// A site also registers without the secret when a connected site vouches for
// it as a peer: that is how a site restored from its recovery phrase gets back
// in through any relay its peers use.
package relay

import (
	"bufio"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const (
	tokenPrefix  = "ibsr2." // key + secret; the address is given separately
	legacyPrefix = "ibsr1." // address + key + secret (older tokens, and inside invites)
)

// Token holds the relay's key (pinned) and registration secret, which never
// need changing, plus the relay's address. The address is a separate,
// visible setting: Encode leaves it out, so a relay can move without new
// tokens.
type Token struct {
	Address string
	Key     ed25519.PublicKey
	Secret  string
}

type tokenJSON struct {
	A string `json:"a,omitempty"`
	K string `json:"k"`
	S string `json:"s"`
}

// Encode returns the token to give to site admins: key and secret only.
func (t Token) Encode() string {
	b, _ := json.Marshal(tokenJSON{K: base64.RawURLEncoding.EncodeToString(t.Key), S: t.Secret})
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// EncodeWithAddress includes the address; used inside invites, which sites
// exchange without anyone reading them.
func (t Token) EncodeWithAddress() string {
	b, _ := json.Marshal(tokenJSON{A: t.Address, K: base64.RawURLEncoding.EncodeToString(t.Key), S: t.Secret})
	return legacyPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// ParseToken reads either token form. Address is empty for the plain form.
func ParseToken(s string) (Token, error) {
	s = strings.TrimSpace(s)
	bad := errors.New("relay token is malformed")
	var body string
	switch {
	case strings.HasPrefix(s, tokenPrefix):
		body = strings.TrimPrefix(s, tokenPrefix)
	case strings.HasPrefix(s, legacyPrefix):
		body = strings.TrimPrefix(s, legacyPrefix)
	default:
		return Token{}, bad
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Token{}, bad
	}
	var tj tokenJSON
	if err := json.Unmarshal(raw, &tj); err != nil {
		return Token{}, bad
	}
	key, err := base64.RawURLEncoding.DecodeString(tj.K)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return Token{}, bad
	}
	return Token{Address: tj.A, Key: key, Secret: tj.S}, nil
}

// ID identifies a relay by its key.
func ID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// Msg is one line of the relay protocol (JSON, newline terminated).
//
// First line from a site on a new connection (op):
//
//	control  register/keep a control connection (secret needed the first time)
//	connect  open a relayed connection to target
//	accept   complete the relayed connection id (answer to "incoming")
//
// Relay to site on the control connection: welcome, incoming, punch,
// endpoints (answer to direct), pong. On connect: ok or error, then raw bytes.
type Msg struct {
	Op        string   `json:"op"`
	Secret    string   `json:"secret,omitempty"`
	Name      string   `json:"name,omitempty"`
	Target    string   `json:"target,omitempty"`
	From      string   `json:"from,omitempty"`
	ID        string   `json:"id,omitempty"`
	Req       int64    `json:"req,omitempty"`
	Token     string   `json:"token,omitempty"`
	Endpoints []string `json:"endpoints,omitempty"`
	Local     []string `json:"local,omitempty"`
	Time      int64    `json:"time,omitempty"`
	Error     string   `json:"error,omitempty"`
	Version   string   `json:"version,omitempty"` // in "welcome": the relay's version
	Keys      [][]byte `json:"keys,omitempty"`    // in "vouch": the keys of the site's peers
	Sites     []string `json:"sites,omitempty"`   // in "vouchers": sites vouching for this one
}

func writeMsg(w io.Writer, m Msg) error {
	b, _ := json.Marshal(m)
	_, err := w.Write(append(b, '\n'))
	return err
}

func readMsg(r *bufio.Reader) (Msg, error) {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return Msg{}, err
	}
	var m Msg
	err = json.Unmarshal(line, &m)
	return m, err
}

// UDP packets share the site's QUIC socket. Their first byte is 0, which
// QUIC never uses, so the QUIC stack hands them to the application.
var (
	udpProbe = []byte("\x00IBSP") // site -> relay: probe + token; relay records the observed address
	udpReply = []byte("\x00IBSR") // relay -> site: the observed address
	udpPunch = []byte("\x00IBSH") // site -> site: opens NAT mappings for a direct connection
)

func ProbePacket(token string) []byte { return append(append([]byte{}, udpProbe...), token...) }

// PunchPacket is sent between sites to open NAT mappings before dialing.
func PunchPacket(siteID string) []byte { return append(append([]byte{}, udpPunch...), siteID...) }

// ParseReply returns the observed address in a relay reply packet.
func ParseReply(b []byte) (string, bool) {
	if len(b) <= len(udpReply) || string(b[:len(udpReply)]) != string(udpReply) {
		return "", false
	}
	return string(b[len(udpReply):]), true
}

// IsPunch reports whether a UDP packet is a site-to-site punch packet.
func IsPunch(b []byte) bool {
	return len(b) >= len(udpPunch) && string(b[:len(udpPunch)]) == string(udpPunch)
}
