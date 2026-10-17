package site

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
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
