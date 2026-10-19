package site

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"slices"
	"strings"
	"testing"

	"ibs/internal/db"
	"ibs/internal/release"
	"ibs/internal/version"
)

func TestUpdates(t *testing.T) {
	s := newTestSite(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	upload := func(v string, level int) error {
		b := release.MakeBundle(priv, []byte("program "+v), release.Manifest{Version: v, Platform: release.Platform, MinCompatible: "1.1.0", DBLevel: level})
		_, err := s.UploadRelease(bytes.NewReader(b.BundleBytes()))
		return err
	}
	if err := upload("9.0.0", db.Level); err == nil {
		t.Fatal("accepted a release signed by an untrusted key")
	}
	if _, err := s.AddReleaseKey(release.Key{Pub: pub}.String(), "mine"); err != nil {
		t.Fatal(err)
	}
	if err := upload("9.0.0", db.Level); err != nil {
		t.Fatal(err)
	}
	// An older version from before a change that breaks it can never run
	// here: it isn't kept.
	if err := upload("0.9.0", db.Level-1); err == nil || !strings.Contains(err.Error(), "database") {
		t.Fatalf("kept a version that can't run on this database: %v", err)
	}
	// An older version that knows every change that matters can be gone back to.
	if err := upload("0.9.5", db.Level); err != nil {
		t.Fatal(err)
	}
	u := s.Updates()
	if len(u.Versions) != 2 || u.Newer != "9.0.0" || u.Versions[0].KeyName != "mine" || len(u.Keys) != 2 {
		t.Fatalf("updates: %+v", u)
	}
	if u.Versions[0].Blocker != "" || u.Versions[1].Blocker != "" {
		t.Fatalf("9.0.0 and 0.9.5 should be allowed: %q, %q", u.Versions[0].Blocker, u.Versions[1].Blocker)
	}
	// Switching versions needs the launcher.
	if err := s.Install("9.0.0"); err == nil {
		t.Fatal("installed without the launcher")
	}
	t.Setenv("IBS_LAUNCHER", "1")
	if err := s.Install(version.Version); err == nil {
		t.Fatal("installed the running version again")
	}
	if err := s.RemoveVersion("0.9.5"); err != nil || len(s.Updates().Versions) != 1 {
		t.Fatalf("remove: %v", err)
	}
	if err := s.RemoveReleaseKey(release.DefaultKey.ID); err == nil {
		t.Fatal("removed the built-in key")
	}
}

// Remove old versions keeps the newest one the site can roll back to, and
// newer versions; one that failed to start doesn't count as a way back.
func TestRemoveOldVersions(t *testing.T) {
	s := newTestSite(t)
	was := version.Version
	version.Version = "1.0.0"
	t.Cleanup(func() { version.Version = was })
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := s.AddReleaseKey(release.Key{Pub: pub}.String(), "mine"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"0.9.1", "0.9.2", "0.9.3", "9.0.0"} {
		b := release.MakeBundle(priv, []byte("program "+v), release.Manifest{Version: v, Platform: release.Platform, MinCompatible: "0.9.0", DBLevel: db.Level,
			Notes: "### New\n\n- Something in " + v})
		if _, err := s.UploadRelease(bytes.NewReader(b.BundleBytes())); err != nil {
			t.Fatal(err)
		}
	}
	held := func() (out []string) {
		for _, v := range s.Updates().Versions {
			out = append(out, v.Version)
		}
		slices.Sort(out)
		return out
	}
	// Release notes come with the release; older ones from GitHub.
	db.SetSetting(s.DB, "github_notes", `{"0.9.1":"from GitHub"}`)
	for _, v := range s.Updates().Versions {
		if v.Notes != "### New\n\n- Something in "+v.Version {
			t.Fatalf("notes of %s = %q", v.Version, v.Notes)
		}
	}
	if err := s.store().MarkBad("0.9.3"); err != nil {
		t.Fatal(err)
	}
	p := s.Updates().Old
	if p.Keep != "0.9.2" || !slices.Equal(p.Remove, []string{"0.9.3", "0.9.1"}) {
		t.Fatalf("plan = %+v; want keep 0.9.2, remove 0.9.3 and 0.9.1", p)
	}
	if _, err := s.RemoveOldVersions(); err != nil {
		t.Fatal(err)
	}
	if got := held(); !slices.Equal(got, []string{"0.9.2", "9.0.0"}) {
		t.Fatalf("held after removing = %v; want 0.9.2 and 9.0.0", got)
	}
	if p := s.Updates().Old; len(p.Remove) != 0 || p.Keep != "0.9.2" {
		t.Fatalf("nothing more to remove, got %+v", p)
	}
}
