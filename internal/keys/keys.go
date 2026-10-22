// Package keys handles site identity: the recovery phrase, key derivation,
// symmetric sealing and the TLS certificate built from the site key.
package keys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"filippo.io/age"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const SeedSize = 32

// Identity is everything derived from a site's recovery seed.
type Identity struct {
	Seed   []byte
	Priv   ed25519.PrivateKey
	Pub    ed25519.PublicKey
	Master []byte // AES-256 key wrapping bucket keys, access keys and metadata backups (before age)
	// Age is the site's age identity (post-quantum hybrid): bucket keys,
	// access keys and metadata backups are encrypted to it.
	Age    *age.HybridIdentity
	SiteID string
}

func NewSeed() []byte {
	b := make([]byte, SeedSize)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func FromSeed(seed []byte) (*Identity, error) {
	if len(seed) != SeedSize {
		return nil, errors.New("invalid seed size")
	}
	edSeed, err := hkdf.Key(sha256.New, seed, nil, "ibs/identity/v1", ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	master, err := hkdf.Key(sha256.New, seed, nil, "ibs/master/v1", 32)
	if err != nil {
		return nil, err
	}
	ageSecret, err := hkdf.Key(sha256.New, seed, nil, "ibs/age/v1", 32)
	if err != nil {
		return nil, err
	}
	ageID, err := AgeIdentityFromSecret(ageSecret)
	if err != nil {
		return nil, err
	}
	priv := ed25519.NewKeyFromSeed(edSeed)
	pub := priv.Public().(ed25519.PublicKey)
	return &Identity{Seed: seed, Priv: priv, Pub: pub, Master: master, Age: ageID, SiteID: SiteID(pub)}, nil
}

// SiteID is the stable identifier of a site, derived from its public key.
func SiteID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:16])
}

var phraseEnc = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// EncodePhrase renders a seed as a human-writable recovery phrase with a checksum.
func EncodePhrase(seed []byte) string {
	sum := sha256.Sum256(seed)
	s := phraseEnc.EncodeToString(append(append([]byte{}, seed...), sum[:2]...))
	var groups []string
	for i := 0; i < len(s); i += 5 {
		groups = append(groups, s[i:min(i+5, len(s))])
	}
	return strings.Join(groups, "-")
}

func DecodePhrase(phrase string) ([]byte, error) {
	r := strings.NewReplacer("-", "", " ", "", "\n", "", "\t", "", "O", "0", "I", "1", "L", "1")
	s := r.Replace(strings.ToUpper(strings.TrimSpace(phrase)))
	b, err := phraseEnc.DecodeString(s)
	if err != nil || len(b) != SeedSize+2 {
		return nil, errors.New("recovery phrase is malformed")
	}
	seed := b[:SeedSize]
	sum := sha256.Sum256(seed)
	if !hmac.Equal(sum[:2], b[SeedSize:]) {
		return nil, errors.New("recovery phrase checksum mismatch")
	}
	return seed, nil
}

// Seal encrypts with AES-256-GCM using a random nonce. Output: nonce || ciphertext.
func Seal(key, plaintext, aad []byte) []byte {
	gcm := mustGCM(key)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad)
}

func Open(key, sealed, aad []byte) ([]byte, error) {
	gcm := mustGCM(key)
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("sealed data too short")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], aad)
}

func mustGCM(key []byte) cipher.AEAD {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return gcm
}

func RandomKey() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Certificate builds a self-signed TLS certificate for the site key. Peers pin
// the public key, so the certificate chain itself is not trusted.
func (id *Identity) Certificate() (tls.Certificate, error) {
	return SelfSignedCert(id.Priv, "ibs-site-"+id.SiteID)
}

// SelfSignedCert wraps an Ed25519 key in a long-lived self-signed certificate
// usable for both TLS server and client authentication.
func SelfSignedCert(priv ed25519.PrivateKey, name string) (tls.Certificate, error) {
	pub := priv.Public().(ed25519.PublicKey)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(20 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// ClientTLS is a TLS client config presenting cert and requiring the server
// to present exactly the pinned key. Chain validation is replaced by pinning.
func ClientTLS(cert tls.Certificate, pin ed25519.PublicKey) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // replaced by key pinning in VerifyConnection
		MinVersion:         tls.VersionTLS13,
		VerifyConnection:   requireKey(pin),
	}
}

// ServerTLS is a TLS server config presenting cert that only accepts clients
// presenting the pinned key.
func ServerTLS(cert tls.Certificate, pin ed25519.PublicKey) *tls.Config {
	return &tls.Config{
		Certificates:     []tls.Certificate{cert},
		ClientAuth:       tls.RequireAnyClientCert,
		MinVersion:       tls.VersionTLS13,
		VerifyConnection: requireKey(pin),
	}
}

func requireKey(pin ed25519.PublicKey) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("no certificate presented")
		}
		pub, err := PubFromCert(cs.PeerCertificates[0])
		if err != nil {
			return err
		}
		if !pub.Equal(pin) {
			return errors.New("presented key does not match the pinned key")
		}
		return nil
	}
}

func PubFromCert(cert *x509.Certificate) (ed25519.PublicKey, error) {
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("peer certificate is not ed25519")
	}
	return pub, nil
}

const pwIter = 210000

func HashPassword(pw string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	k, _ := pbkdf2.Key(sha256.New, pw, salt, pwIter, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pwIter, hex.EncodeToString(salt), hex.EncodeToString(k))
}

func CheckPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	var iter int
	if _, err := fmt.Sscanf(parts[1], "%d", &iter); err != nil {
		return false
	}
	salt, err1 := hex.DecodeString(parts[2])
	want, err2 := hex.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	k, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && hmac.Equal(k, want)
}
