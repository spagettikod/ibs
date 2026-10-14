package release

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Store keeps installed versions and trusted keys in a data directory:
//
//	versions/<version>/{ibs, manifest.json, manifest.sig}
//	versions/current     the version to run
//	versions/previous    the one before, to go back to
//	versions/good        the last version that ran fine for a while
//	versions/bad/<v>     versions that failed to start; not chosen again
//	keys/<id>.pub        trusted keys besides the built-in one
type Store struct {
	Dir string // the data directory, e.g. /data
}

// Installed is a version in the store.
type Installed struct {
	Manifest
	KeyID string `json:"key_id"`
	Bad   bool   `json:"bad"`
}

func (s Store) versions() string        { return filepath.Join(s.Dir, "versions") }
func (s Store) path(v string) string    { return filepath.Join(s.versions(), v) }
func (s Store) marker(n string) string  { return filepath.Join(s.versions(), n) }
func (s Store) keysDir() string         { return filepath.Join(s.Dir, "keys") }
func (s Store) Program(v string) string { return filepath.Join(s.path(v), ProgramFile) }
func validVersion(v string) bool {
	return v != "" && !strings.ContainsAny(v, "/\\") && v != "." && v != ".."
}
func (s Store) read(n string) string {
	b, _ := os.ReadFile(s.marker(n))
	return strings.TrimSpace(string(b))
}
func (s Store) write(n, v string) error { return writeFile(s.marker(n), []byte(v+"\n"), 0o644) }
func (s Store) Current() string         { return s.read("current") }
func (s Store) Previous() string        { return s.read("previous") }
func (s Store) Good() string            { return s.read("good") }
func (s Store) SetGood(v string) error  { return s.write("good", v) }
func (s Store) IsBad(v string) bool {
	_, err := os.Stat(filepath.Join(s.versions(), "bad", v))
	return err == nil
}

// SetCurrent makes v the version to run, remembering the one before.
func (s Store) SetCurrent(v string) error {
	if cur := s.Current(); cur != "" && cur != v {
		if err := s.write("previous", cur); err != nil {
			return err
		}
	}
	os.Remove(filepath.Join(s.versions(), "bad", v)) // chosen again on purpose
	return s.write("current", v)
}

// MarkBad records that v failed, and goes back to the previous version.
func (s Store) MarkBad(v string) error {
	if err := os.MkdirAll(filepath.Join(s.versions(), "bad"), 0o755); err != nil {
		return err
	}
	writeFile(filepath.Join(s.versions(), "bad", v), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
	back := s.Good()
	if back == "" || back == v {
		back = s.Previous()
	}
	if back != "" && back != v {
		return s.write("current", back)
	}
	return nil
}

// Install verifies a release and adds it to the store. It doesn't change
// which version runs.
func (s Store) Install(b Bundle, keys []Key) (Installed, error) {
	m, k, err := b.Check(keys)
	if err != nil {
		return Installed{}, err
	}
	if m.Platform != Platform {
		return Installed{}, fmt.Errorf("version %s is built for %s, this machine is %s", m.Version, m.Platform, Platform)
	}
	if !validVersion(m.Version) {
		return Installed{}, errors.New("invalid version")
	}
	dir := s.path(m.Version)
	tmp := dir + ".tmp"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return Installed{}, err
	}
	for _, f := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{{ProgramFile, b.Program, 0o755}, {ManifestFile, b.ManifestJSON, 0o644}, {SignatureFile, b.Signature, 0o644}} {
		if err := os.WriteFile(filepath.Join(tmp, f.name), f.data, f.mode); err != nil {
			return Installed{}, err
		}
	}
	os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return Installed{}, err
	}
	return Installed{Manifest: m, KeyID: k.ID}, nil
}

// Load reads an installed version and verifies it again: signature, and that
// the program on disk is the one signed.
func (s Store) Load(v string, keys []Key) (Bundle, Installed, error) {
	if !validVersion(v) {
		return Bundle{}, Installed{}, errors.New("invalid version")
	}
	var b Bundle
	var err error
	if b.ManifestJSON, err = os.ReadFile(filepath.Join(s.path(v), ManifestFile)); err != nil {
		return b, Installed{}, err
	}
	if b.Signature, err = os.ReadFile(filepath.Join(s.path(v), SignatureFile)); err != nil {
		return b, Installed{}, err
	}
	if b.Program, err = os.ReadFile(s.Program(v)); err != nil {
		return b, Installed{}, err
	}
	m, k, err := b.Check(keys)
	if err != nil {
		return b, Installed{}, fmt.Errorf("version %s: %w", v, err)
	}
	return b, Installed{Manifest: m, KeyID: k.ID, Bad: s.IsBad(v)}, nil
}

// Verify checks an installed version without keeping it in memory longer
// than needed (the launcher, before every start).
func (s Store) Verify(v string, keys []Key) (Manifest, error) {
	_, in, err := s.Load(v, keys)
	return in.Manifest, err
}

// List returns the installed versions that verify, newest first. Versions
// signed by a key that is no longer trusted are left out.
func (s Store) List(keys []Key) []Installed {
	entries, _ := os.ReadDir(s.versions())
	var out []Installed
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "bad" || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		mj, err1 := os.ReadFile(filepath.Join(s.path(e.Name()), ManifestFile))
		sig, err2 := os.ReadFile(filepath.Join(s.path(e.Name()), SignatureFile))
		if err1 != nil || err2 != nil {
			continue
		}
		m, k, err := Verify(mj, sig, keys)
		if err != nil || m.Version != e.Name() {
			continue
		}
		out = append(out, Installed{Manifest: m, KeyID: k.ID, Bad: s.IsBad(m.Version)})
	}
	slices.SortFunc(out, func(a, b Installed) int { return Compare(b.Version, a.Version) })
	return out
}

// Remove deletes an installed version; the one running can't be removed.
func (s Store) Remove(v string) error {
	if !validVersion(v) {
		return errors.New("invalid version")
	}
	if v == s.Current() {
		return errors.New("this version is in use")
	}
	if _, err := os.Stat(s.path(v)); err != nil {
		return errors.New("no such version")
	}
	return os.RemoveAll(s.path(v))
}

// Keys returns the trusted keys: the built-in ones first, then those saved in
// the data directory (built-in keys saved by other IBS versions, and added
// ones). Revoked keys are left out.
func (s Store) Keys() []Key {
	var keys []Key
	seen := map[string]bool{}
	for _, k := range BuiltinKeys {
		if !Revoked(k.ID) {
			keys = append(keys, k)
			seen[k.ID] = true
		}
	}
	entries, _ := os.ReadDir(s.keysDir())
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".pub") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.keysDir(), e.Name()))
		if err != nil {
			continue
		}
		var f keyFile
		if json.Unmarshal(b, &f) != nil {
			continue
		}
		k, err := ParseKey(f.Key)
		if err != nil || seen[k.ID] || Revoked(k.ID) {
			continue
		}
		k.Name, k.Default = f.Name, f.Builtin
		keys = append(keys, k)
		seen[k.ID] = true
	}
	return keys
}

// AddKey trusts a key for releases.
func (s Store) AddKey(key, name string) (Key, error) {
	k, err := ParseKey(key)
	if err != nil {
		return k, err
	}
	if Revoked(k.ID) {
		return k, errors.New("this key has been revoked")
	}
	for _, t := range s.Keys() {
		if t.ID == k.ID && t.Default {
			return t, nil
		}
	}
	k.Name = strings.TrimSpace(name)
	return k, s.saveKey(k, false)
}

type keyFile struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Builtin bool   `json:"builtin,omitempty"`
}

func (s Store) saveKey(k Key, builtin bool) error {
	b, _ := json.MarshalIndent(keyFile{k.String(), k.Name, builtin}, "", "  ")
	return writeFile(filepath.Join(s.keysDir(), k.ID+".pub"), b, 0o644)
}

// SaveBuiltinKeys saves this build's built-in keys in the data directory, so
// launchers and IBS versions built before a key was added trust it too, and
// deletes saved keys that are revoked. IBS calls it on start.
func (s Store) SaveBuiltinKeys() error {
	for _, k := range BuiltinKeys {
		if err := s.saveKey(k, true); err != nil {
			return err
		}
	}
	for _, id := range RevokedKeys {
		os.Remove(filepath.Join(s.keysDir(), id+".pub"))
	}
	return nil
}

// RemoveKey stops trusting a key. The built-in key can't be removed, nor a key
// that installed versions are signed with: remove those versions first.
func (s Store) RemoveKey(id string) error {
	for _, k := range s.Keys() {
		if k.ID == id && k.Default {
			return errors.New("the built-in key can't be removed")
		}
	}
	if strings.ContainsAny(id, "/\\.") {
		return errors.New("invalid key")
	}
	if in := s.SignedWith(id); len(in) > 0 {
		return fmt.Errorf("versions signed with this key are installed (%s); remove them first", strings.Join(in, ", "))
	}
	err := os.Remove(filepath.Join(s.keysDir(), id+".pub"))
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("no such key")
	}
	return err
}

// SignedWith returns the installed versions signed with the key id.
func (s Store) SignedWith(id string) []string {
	var out []string
	for _, in := range s.List(s.Keys()) {
		if in.KeyID == id {
			out = append(out, in.Version)
		}
	}
	return out
}

// ParseKeyList parses keys separated by commas or spaces, as in IBS_TRUSTED_KEYS.
func ParseKeyList(list string) ([]Key, error) {
	var keys []Key
	for _, f := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' }) {
		k, err := ParseKey(f)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Hash returns the SHA-256 of data as hex, as manifests hold it.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Equal reports whether two bundles are the same release.
func (b Bundle) Equal(o Bundle) bool { return bytes.Equal(b.ManifestJSON, o.ManifestJSON) }
