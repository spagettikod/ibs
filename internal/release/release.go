// Package release handles signed IBS releases: the manifest describing a
// build, its Ed25519 signature, trusted keys, the .ibsrelease file that
// carries a release, and the store of installed versions shared by the
// launcher and the IBS program.
//
// A release is three files: the program ("ibs"), its manifest
// ("manifest.json") and the signature of the manifest ("manifest.sig"). The
// manifest holds the program's SHA-256, so the signature covers the program
// too. Releases are only accepted when signed by a trusted key; the
// maintainer's key is built in and can't be removed.
package release

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
)

// Platform is the platform this program runs on, e.g. "linux/amd64".
const Platform = runtime.GOOS + "/" + runtime.GOARCH

// Manifest describes one build of IBS.
type Manifest struct {
	Version       string `json:"version"`
	Platform      string `json:"platform"`
	SHA256        string `json:"sha256"` // of the program
	Size          int64  `json:"size"`
	MinCompatible string `json:"min_compatible"`  // oldest IBS version this one works with
	DBLevel       int    `json:"db_level"`        // database changes it knows; it can't run on a database changed further
	Released      int64  `json:"released"`        // unix time
	Notes         string `json:"notes,omitempty"` // what changed, from the changelog (Markdown)
}

// Key is a public key that signs releases.
type Key struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Pub     ed25519.PublicKey `json:"-"`
	Default bool              `json:"default"` // the maintainer's, built in
}

const keyPrefix = "ibspub1:"

// String encodes the key for sharing: "ibspub1:<base64url>".
func (k Key) String() string { return keyPrefix + base64.RawURLEncoding.EncodeToString(k.Pub) }

// ParseKey reads a key in the form String produces.
func ParseKey(s string) (Key, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), keyPrefix))
	if err != nil || len(raw) != ed25519.PublicKeySize || !strings.HasPrefix(strings.TrimSpace(s), keyPrefix) {
		return Key{}, errors.New("not a release key (expected ibspub1:...)")
	}
	return Key{ID: KeyID(raw), Pub: ed25519.PublicKey(raw)}, nil
}

// KeyID is a short, stable name for a public key.
func KeyID(pub []byte) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:6])
}

// Sign signs a manifest; the result is the manifest's JSON and the signature.
func Sign(priv ed25519.PrivateKey, m Manifest) (manifestJSON, sig []byte) {
	manifestJSON, _ = json.MarshalIndent(m, "", "  ")
	return manifestJSON, ed25519.Sign(priv, manifestJSON)
}

// Verify checks a manifest's signature against the trusted keys and returns
// the manifest and the key that signed it.
func Verify(manifestJSON, sig []byte, keys []Key) (Manifest, Key, error) {
	for _, k := range keys {
		if ed25519.Verify(k.Pub, manifestJSON, sig) {
			var m Manifest
			if err := json.Unmarshal(manifestJSON, &m); err != nil {
				return Manifest{}, Key{}, fmt.Errorf("manifest: %w", err)
			}
			if m.Version == "" || len(m.SHA256) != 64 {
				return Manifest{}, Key{}, errors.New("manifest is incomplete")
			}
			return m, k, nil
		}
	}
	return Manifest{}, Key{}, errors.New("not signed by a trusted key")
}

// SignerID returns the ID of the key that signed a manifest, among keys, or ""
// when none did.
func SignerID(manifestJSON, sig []byte, keys []Key) string {
	if _, k, err := Verify(manifestJSON, sig, keys); err == nil {
		return k.ID
	}
	return ""
}

// Bundle is a release in memory: the three files.
type Bundle struct {
	Program      []byte
	ManifestJSON []byte
	Signature    []byte
}

// Check verifies the signature and that the program matches the manifest.
func (b Bundle) Check(keys []Key) (Manifest, Key, error) {
	m, k, err := Verify(b.ManifestJSON, b.Signature, keys)
	if err != nil {
		return m, k, err
	}
	sum := sha256.Sum256(b.Program)
	if hex.EncodeToString(sum[:]) != m.SHA256 || int64(len(b.Program)) != m.Size {
		return m, k, errors.New("the program doesn't match its signed manifest")
	}
	return m, k, nil
}

// File names inside a release.
const (
	ProgramFile   = "ibs"
	ManifestFile  = "manifest.json"
	SignatureFile = "manifest.sig"
)

// WriteTar writes the bundle as an .ibsrelease file (a tar archive).
func (b Bundle) WriteTar(w io.Writer) error {
	tw := tar.NewWriter(w)
	for _, f := range []struct {
		name string
		mode int64
		data []byte
	}{{ManifestFile, 0o644, b.ManifestJSON}, {SignatureFile, 0o644, b.Signature}, {ProgramFile, 0o755, b.Program}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data))}); err != nil {
			return err
		}
		if _, err := tw.Write(f.data); err != nil {
			return err
		}
	}
	return tw.Close()
}

// MaxSize bounds what is read from an .ibsrelease file.
const MaxSize = 256 << 20

// ReadTar reads an .ibsrelease file. It doesn't verify anything; call Check.
func ReadTar(r io.Reader) (Bundle, error) {
	var b Bundle
	tr := tar.NewReader(io.LimitReader(r, MaxSize))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return b, fmt.Errorf("not a release file: %w", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return b, err
		}
		switch h.Name {
		case ProgramFile:
			b.Program = data
		case ManifestFile:
			b.ManifestJSON = data
		case SignatureFile:
			b.Signature = data
		}
	}
	if b.Program == nil || b.ManifestJSON == nil || b.Signature == nil {
		return b, errors.New("not a release file: it must hold the program, its manifest and signature")
	}
	return b, nil
}

// MakeBundle builds a signed release for a program.
func MakeBundle(priv ed25519.PrivateKey, program []byte, m Manifest) Bundle {
	sum := sha256.Sum256(program)
	m.SHA256 = hex.EncodeToString(sum[:])
	m.Size = int64(len(program))
	mj, sig := Sign(priv, m)
	return Bundle{Program: program, ManifestJSON: mj, Signature: sig}
}

// Compare compares two versions like "1.2.0" or "1.3.0-beta1": -1, 0 or 1.
// A pre-release sorts before its release; anything that isn't a version
// (e.g. "dev") sorts before every version.
func Compare(a, b string) int {
	pa, okA := parse(a)
	pb, okB := parse(b)
	switch {
	case !okA && !okB:
		return strings.Compare(a, b)
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for i := range 3 {
		if pa.n[i] != pb.n[i] {
			if pa.n[i] < pb.n[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case pa.pre == pb.pre:
		return 0
	case pa.pre == "":
		return 1
	case pb.pre == "":
		return -1
	}
	return strings.Compare(pa.pre, pb.pre)
}

// IsVersion reports whether v is a release version (not e.g. "dev").
func IsVersion(v string) bool { _, ok := parse(v); return ok }

type parsed struct {
	n   [3]int
	pre string
}

func parse(v string) (parsed, bool) {
	var p parsed
	core, pre, _ := strings.Cut(strings.TrimPrefix(v, "v"), "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return p, false
	}
	for i, s := range parts {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return p, false
		}
		p.n[i] = n
	}
	p.pre = pre
	return p, true
}

// Compatible reports whether two versions work together: each is at least the
// other's oldest compatible version. Unknown versions ("dev", or a site that
// didn't say) are treated as compatible.
func Compatible(a, aMin, b, bMin string) bool {
	if !IsVersion(a) || !IsVersion(b) {
		return true
	}
	return (bMin == "" || Compare(a, bMin) >= 0) && (aMin == "" || Compare(b, aMin) >= 0)
}

// BundleBytes returns the .ibsrelease file of a bundle.
func (b Bundle) BundleBytes() []byte {
	var buf bytes.Buffer
	b.WriteTar(&buf)
	return buf.Bytes()
}
