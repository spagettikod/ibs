package site

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"ibs/internal/relay"
)

// Invites are "ibs2." and the base64url of compact bytes (since 1.4.0; the
// older JSON "ibs1." invites were read until 1.5.0). They hold the issuer's key, the one-time secret, its direct
// address and its relays (address, key and registration secret), each string
// prefixed with its length:
//
//	key[32] len secret len address count { len address key[32] len secret }
const invitePrefix = "ibs2."

var errBadInvite = errors.New("invite token is malformed")

func encodeInvite(inv invite) (string, error) {
	pub, err := base64.RawURLEncoding.DecodeString(inv.PubKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", errors.New("invalid site key")
	}
	var b bytes.Buffer
	str := func(s string) error {
		if len(s) > 255 {
			return errors.New("invite field too long")
		}
		b.WriteByte(byte(len(s)))
		b.WriteString(s)
		return nil
	}
	b.Write(pub)
	if err := str(inv.Secret); err != nil {
		return "", err
	}
	if err := str(inv.Address); err != nil {
		return "", err
	}
	if len(inv.Relays) > 255 {
		return "", errors.New("too many relays")
	}
	b.WriteByte(byte(len(inv.Relays)))
	for _, tok := range inv.Relays {
		t, err := relay.ParseToken(tok)
		if err != nil {
			return "", err
		}
		if err := str(t.Address); err != nil {
			return "", err
		}
		b.Write(t.Key)
		if err := str(t.Secret); err != nil {
			return "", err
		}
	}
	return invitePrefix + base64.RawURLEncoding.EncodeToString(b.Bytes()), nil
}

// decodeInvite reads an invite. Relays come back as relay
// tokens with their address, as the rest of joining expects.
func decodeInvite(token string) (invite, error) {
	token = strings.Join(strings.Fields(token), "") // pasted invites may wrap
	if !strings.HasPrefix(token, invitePrefix) {
		return invite{}, errBadInvite
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, invitePrefix))
	if err != nil {
		return invite{}, errBadInvite
	}
	r := bytes.NewReader(raw)
	fixed := func(n int) ([]byte, error) {
		p := make([]byte, n)
		if _, err := io.ReadFull(r, p); err != nil {
			return nil, errBadInvite
		}
		return p, nil
	}
	str := func() (string, error) {
		n, err := r.ReadByte()
		if err != nil {
			return "", errBadInvite
		}
		if n == 0 {
			return "", nil
		}
		p, err := fixed(int(n))
		return string(p), err
	}
	var inv invite
	pub, err := fixed(ed25519.PublicKeySize)
	if err != nil {
		return inv, err
	}
	inv.PubKey = base64.RawURLEncoding.EncodeToString(pub)
	if inv.Secret, err = str(); err != nil {
		return inv, err
	}
	if inv.Address, err = str(); err != nil {
		return inv, err
	}
	count, err := r.ReadByte()
	if err != nil {
		return inv, errBadInvite
	}
	for range count {
		var t relay.Token
		if t.Address, err = str(); err != nil {
			return inv, err
		}
		key, err := fixed(ed25519.PublicKeySize)
		if err != nil {
			return inv, err
		}
		t.Key = key
		if t.Secret, err = str(); err != nil {
			return inv, err
		}
		inv.Relays = append(inv.Relays, t.EncodeWithAddress())
	}
	if r.Len() != 0 || inv.Secret == "" {
		return inv, errBadInvite
	}
	return inv, nil
}
