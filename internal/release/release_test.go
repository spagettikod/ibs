package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"os"
	"strings"
	"testing"
)

func testKey(t *testing.T) (ed25519.PrivateKey, Key) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	return priv, Key{ID: KeyID(pub), Pub: pub}
}

func bundle(priv ed25519.PrivateKey, v string, program string) Bundle {
	return MakeBundle(priv, []byte(program), Manifest{Version: v, Platform: Platform, MinCompatible: "1.1.0", DBLevel: 3})
}

func TestSignAndCheck(t *testing.T) {
	priv, key := testKey(t)
	_, other := testKey(t)
	b := bundle(priv, "1.3.0", "program")
	if m, k, err := b.Check([]Key{other, key}); err != nil || m.Version != "1.3.0" || k.ID != key.ID {
		t.Fatalf("check: %v %v %v", m, k, err)
	}
	if _, _, err := b.Check([]Key{other}); err == nil {
		t.Fatal("accepted a release signed by an untrusted key")
	}
	tampered := b
	tampered.Program = []byte("evil")
	if _, _, err := tampered.Check([]Key{key}); err == nil {
		t.Fatal("accepted a program that doesn't match its manifest")
	}
	read, err := ReadTar(bytesReader(b.BundleBytes()))
	if err != nil || !read.Equal(b) || string(read.Program) != "program" {
		t.Fatalf("round trip through .ibsrelease: %v", err)
	}
	k2, err := ParseKey(key.String())
	if err != nil || k2.ID != key.ID {
		t.Fatalf("key round trip: %v", err)
	}
}

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.2.0", "1.10.0", -1}, {"1.3.0", "1.3.0", 0}, {"2.0.0", "1.9.9", 1},
		{"1.3.0-beta1", "1.3.0", -1}, {"1.3.0-beta2", "1.3.0-beta1", 1}, {"dev", "1.0.0", -1}, {"v1.2.0", "1.2.0", 0},
	} {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if !Compatible("1.3.0", "1.1.0", "1.2.0", "1.1.0") || Compatible("1.3.0", "1.3.0", "1.2.0", "1.1.0") ||
		Compatible("1.0.0", "1.0.0", "1.3.0", "1.1.0") || !Compatible("dev", "", "1.3.0", "1.3.0") {
		t.Fatal("Compatible gives wrong answers")
	}
}

func TestStore(t *testing.T) {
	priv, key := testKey(t)
	s := Store{Dir: t.TempDir()}
	keys := []Key{key}
	for _, v := range []string{"1.2.0", "1.3.0"} {
		if _, err := s.Install(bundle(priv, v, "program "+v), keys); err != nil {
			t.Fatal(err)
		}
	}
	if l := s.List(keys); len(l) != 2 || l[0].Version != "1.3.0" {
		t.Fatalf("list: %+v", l)
	}
	s.SetCurrent("1.2.0")
	s.SetGood("1.2.0")
	s.SetCurrent("1.3.0")
	if s.Current() != "1.3.0" || s.Previous() != "1.2.0" {
		t.Fatal("current/previous")
	}
	// The new version fails: back to the last good one, and it's marked bad.
	s.MarkBad("1.3.0")
	if s.Current() != "1.2.0" || !s.IsBad("1.3.0") {
		t.Fatalf("after a failed start: current %s", s.Current())
	}
	// A program changed on disk no longer verifies.
	os.WriteFile(s.Program("1.2.0"), []byte("changed"), 0o755)
	if _, err := s.Verify("1.2.0", keys); err == nil {
		t.Fatal("a changed program must not verify")
	}
	if err := s.Remove("1.2.0"); err == nil {
		t.Fatal("removed the version in use")
	}
	if err := s.Remove("1.3.0"); err != nil {
		t.Fatal(err)
	}
	// Keys: the built-in one can't be removed; added ones can, once no
	// installed version is signed with them.
	otherPriv, other := testKey(t)
	if _, err := s.AddKey(other.String(), "mine"); err != nil {
		t.Fatal(err)
	}
	if ks := s.Keys(); len(ks) != 2 || !ks[0].Default || ks[1].Name != "mine" {
		t.Fatalf("keys: %+v", ks)
	}
	if _, err := s.Install(bundle(otherPriv, "1.4.0", "program 1.4.0"), s.Keys()); err != nil {
		t.Fatal(err)
	}
	if s.RemoveKey(DefaultKey.ID) == nil {
		t.Fatal("removed the built-in key")
	}
	if err := s.RemoveKey(other.ID); err == nil || !strings.Contains(err.Error(), "1.4.0") {
		t.Fatalf("removed a key that 1.4.0 is signed with: %v", err)
	}
	if err := s.Remove("1.4.0"); err != nil {
		t.Fatal(err)
	}
	if s.RemoveKey(other.ID) != nil || len(s.Keys()) != 1 {
		t.Fatal("key removal")
	}
}

func bytesReader(b []byte) *bytesR { return &bytesR{b: b} }

type bytesR struct{ b []byte }

func (r *bytesR) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

func TestKeyRotation(t *testing.T) {
	_, newKey := testKey(t)
	newKey.Name, newKey.Default = "new", true
	s := Store{Dir: t.TempDir()}
	defer func(b []Key, r []string) { BuiltinKeys, RevokedKeys = b, r }(BuiltinKeys, RevokedKeys)

	// An IBS version that adds a built-in key saves it...
	BuiltinKeys = []Key{DefaultKey, newKey}
	if err := s.SaveBuiltinKeys(); err != nil {
		t.Fatal(err)
	}
	// ...so a launcher built before it trusts it too, as a built-in key.
	BuiltinKeys = []Key{DefaultKey}
	if ks := s.Keys(); len(ks) != 2 || ks[1].ID != newKey.ID || !ks[1].Default {
		t.Fatalf("keys: %+v", ks)
	}
	if s.RemoveKey(newKey.ID) == nil {
		t.Fatal("removed a built-in key")
	}

	// A launcher that revokes the old key leaves it out, also when it is saved.
	BuiltinKeys, RevokedKeys = []Key{newKey}, []string{DefaultKey.ID}
	s.saveKey(DefaultKey, true)
	if ks := s.Keys(); len(ks) != 1 || ks[0].ID != newKey.ID {
		t.Fatalf("keys after revoking: %+v", ks)
	}
	if _, err := s.AddKey(DefaultKey.String(), ""); err == nil {
		t.Fatal("added a revoked key")
	}
	if s.SaveBuiltinKeys(); len(s.Keys()) != 1 {
		t.Fatal("the revoked key's file should be gone")
	}
}

func TestPickGitHubRelease(t *testing.T) {
	asset := func(v string) string {
		return `{"name":"ibs-` + v + AssetSuffix + `","browser_download_url":"https://x/` + v + `"}`
	}
	list := `[
		{"tag_name":"v1.4.0-beta1","prerelease":true,"assets":[` + asset("1.4.0-beta1") + `]},
		{"tag_name":"v1.3.0","assets":[` + asset("1.3.0") + `]},
		{"tag_name":"v1.5.0","draft":true,"assets":[` + asset("1.5.0") + `]},
		{"tag_name":"v1.2.0","assets":[]}
	]`
	if r, err := PickGitHubRelease([]byte(list), false); err != nil || r.Version != "1.3.0" || r.AssetURL != "https://x/1.3.0" {
		t.Fatalf("stable: %+v %v", r, err)
	}
	if r, err := PickGitHubRelease([]byte(list), true); err != nil || r.Version != "1.4.0-beta1" {
		t.Fatalf("betas: %+v %v", r, err)
	}
	if _, err := PickGitHubRelease([]byte(`[{"tag_name":"v1.4.0-beta1","prerelease":true,"assets":[`+asset("1.4.0-beta1")+`]}]`), false); err == nil {
		t.Fatal("found a stable release among betas")
	}
	if r, err := PickGitHubRelease([]byte(`{"tag_name":"v1.3.0","assets":[`+asset("1.3.0")+`]}`), false); err != nil || r.Version != "1.3.0" {
		t.Fatalf("single release: %+v %v", r, err)
	}
}
