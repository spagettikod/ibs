// Package node implements a storage node and the join token that connects
// it to its site manager.
package node

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

const tokenPrefix = "ibsn2." // key + secret; the manager address is given separately (IBS_MANAGER)

// Token carries what a node needs to join a site and never needs changing:
// the manager's key (pinned for all traffic) and the join secret. The
// manager's address is a separate, visible setting (IBS_MANAGER).
type Token struct {
	Key    ed25519.PublicKey // manager's (site) key
	Secret string
}

type tokenJSON struct {
	K string `json:"k"`
	S string `json:"s"`
}

func (t Token) Encode() string {
	b, _ := json.Marshal(tokenJSON{K: base64.RawURLEncoding.EncodeToString(t.Key), S: t.Secret})
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func ParseToken(s string) (Token, error) {
	s = strings.TrimSpace(s)
	bad := errors.New("join token is malformed")
	body, ok := strings.CutPrefix(s, tokenPrefix)
	if !ok {
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
	if err != nil || len(key) != ed25519.PublicKeySize || tj.S == "" {
		return Token{}, bad
	}
	return Token{Key: key, Secret: tj.S}, nil
}

// Registration is what a node sends the manager on start and as heartbeat.
type Registration struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Secret    string `json:"secret,omitempty"`    // join secret; only needed until the node is known
	Port      int    `json:"port"`                // node listener port; the manager uses the connection's IP
	Advertise string `json:"advertise,omitempty"` // explicit host:port when the IP is not reachable (NAT)
	Capacity  int64  `json:"capacity"`
	Free      int64  `json:"free"`
	Limit     int64  `json:"limit"` // IBS_STORAGE_LIMIT on the node: 0 = whole disk, -1 = nothing
	Version   string `json:"version,omitempty"`
}

type RegistrationReply struct {
	SiteName string `json:"site_name"`
	Address  string `json:"address"` // how the manager will reach the node
	// TargetVersion is the version the node should run (the manager's), when
	// it is the node's turn to update; TrustedKeys are the release keys the
	// manager trusts besides the built-in one.
	TargetVersion string   `json:"target_version,omitempty"`
	TrustedKeys   []string `json:"trusted_keys,omitempty"`
}
