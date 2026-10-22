package keys

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
)

// age (age-encryption.org) encrypts everything IBS stores, with hybrid
// post-quantum keys (ML-KEM-768 with X25519): safe while either holds. The
// site's identity comes from the recovery seed, so the phrase is all anyone
// needs to keep; buckets have random identities, stored encrypted to it.

// ageHeader is how every age file starts.
var ageHeader = []byte("age-encryption.org/v1\n")

// IsAge reports whether b is an age file (rather than IBS's earlier
// AES-GCM format, which starts with a random nonce).
func IsAge(b []byte) bool { return bytes.HasPrefix(b, ageHeader) }

// AgeIdentityFromSecret builds a hybrid age identity from a 32-byte secret.
func AgeIdentityFromSecret(secret []byte) (*age.HybridIdentity, error) {
	if len(secret) != 32 {
		return nil, errors.New("age identity secret must be 32 bytes")
	}
	s, err := bech32Encode("age-secret-key-pq-", secret)
	if err != nil {
		return nil, err
	}
	return age.ParseHybridIdentity(strings.ToUpper(s))
}

// NewAgeIdentity makes a random hybrid age identity, e.g. for a bucket.
func NewAgeIdentity() (*age.HybridIdentity, error) { return age.GenerateHybridIdentity() }

// SealAge encrypts plain to r as an age file. label binds it to what it is
// (age has no associated data): OpenAge checks it.
func SealAge(r age.Recipient, label string, plain []byte) ([]byte, error) {
	var b bytes.Buffer
	w, err := age.Encrypt(&b, r)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, label+"\n"); err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// OpenAge decrypts an age file made by SealAge with the same label.
func OpenAge(id age.Identity, label string, sealed []byte) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(sealed), id)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	rest, ok := bytes.CutPrefix(b, []byte(label+"\n"))
	if !ok {
		return nil, fmt.Errorf("sealed data is not %s", label)
	}
	return rest, nil
}

// Bech32 (BIP 173), the text form of age keys.
const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := range 5 {
			if (top>>i)&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func bech32Encode(hrp string, data []byte) (string, error) {
	hrp = strings.ToLower(hrp)
	var values []byte
	acc, bits := 0, 0
	for _, b := range data {
		acc = acc<<8 | int(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			values = append(values, byte(acc>>bits&31))
		}
	}
	if bits > 0 {
		values = append(values, byte(acc<<(5-bits)&31))
	}
	var expanded []byte
	for _, c := range hrp {
		expanded = append(expanded, byte(c>>5))
	}
	expanded = append(expanded, 0)
	for _, c := range hrp {
		expanded = append(expanded, byte(c&31))
	}
	mod := bech32Polymod(append(append(expanded, values...), 0, 0, 0, 0, 0, 0)) ^ 1
	var s strings.Builder
	s.WriteString(hrp)
	s.WriteByte('1')
	for _, v := range values {
		s.WriteByte(bech32Charset[v])
	}
	for i := range 6 {
		s.WriteByte(bech32Charset[(mod>>(5*(5-i)))&31])
	}
	return s.String(), nil
}
