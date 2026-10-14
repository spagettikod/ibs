package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ibs/internal/release"
)

// install adds a shell script as a signed release; it appends its version to
// ran.log when started.
func install(t *testing.T, s release.Store, priv ed25519.PrivateKey, v, body string) {
	t.Helper()
	script := "#!/bin/sh\necho " + v + " >> " + filepath.Join(s.Dir, "ran.log") + "\n" + body + "\n"
	b := release.MakeBundle(priv, []byte(script), release.Manifest{Version: v, Platform: release.Platform})
	if _, err := s.Install(b, s.Keys()); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (release.Store, ed25519.PrivateKey) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := release.Store{Dir: t.TempDir()}
	if _, err := s.AddKey(release.Key{Pub: pub}.String(), "test"); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"ibs-launcher"}
	return s, priv
}

func ran(s release.Store) string {
	b, _ := os.ReadFile(filepath.Join(s.Dir, "ran.log"))
	return strings.Join(strings.Fields(string(b)), " ")
}

func TestNewVersionThatCrashesGoesBack(t *testing.T) {
	s, priv := setup(t)
	install(t, s, priv, "1.2.0", "exit 0")
	install(t, s, priv, "1.3.0", "exit 3")
	s.SetCurrent("1.2.0")
	s.SetGood("1.2.0")
	s.SetCurrent("1.3.0")
	if code := run(s, time.Minute); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if got := ran(s); got != "1.3.0 1.2.0" {
		t.Fatalf("ran %q", got)
	}
	if s.Current() != "1.2.0" || !s.IsBad("1.3.0") {
		t.Fatalf("current %s, 1.3.0 bad: %v", s.Current(), s.IsBad("1.3.0"))
	}
}

func TestRestartRunsTheNewVersion(t *testing.T) {
	s, priv := setup(t)
	install(t, s, priv, "1.3.0", "sleep 0.3; exit 0")
	// 1.2.0 "installs" 1.3.0 and asks to be restarted, as IBS does.
	install(t, s, priv, "1.2.0", "echo 1.3.0 > "+filepath.Join(s.Dir, "versions", "current")+"; exit 75")
	s.SetCurrent("1.2.0")
	s.SetGood("1.2.0")
	if code := run(s, 100*time.Millisecond); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if got := ran(s); got != "1.2.0 1.3.0" {
		t.Fatalf("ran %q", got)
	}
	if s.Good() != "1.3.0" {
		t.Fatalf("1.3.0 ran past its first minutes, it should count as good (good=%s)", s.Good())
	}
}

func TestChangedProgramDoesntRun(t *testing.T) {
	s, priv := setup(t)
	install(t, s, priv, "1.2.0", "exit 0")
	install(t, s, priv, "1.3.0", "exit 0")
	s.SetCurrent("1.2.0")
	s.SetGood("1.2.0")
	s.SetCurrent("1.3.0")
	os.WriteFile(s.Program("1.3.0"), []byte("#!/bin/sh\necho tampered >> "+filepath.Join(s.Dir, "ran.log")+"\n"), 0o755)
	run(s, time.Minute)
	if got := ran(s); got != "1.2.0" {
		t.Fatalf("ran %q; a changed program must never run", got)
	}
}
