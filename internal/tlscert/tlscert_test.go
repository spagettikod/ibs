package tlscert

import (
	"crypto/x509"
	"testing"
	"time"
)

func TestRenewDue(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	for _, c := range []struct {
		name      string
		lifetime  time.Duration
		remaining time.Duration
		due       bool
	}{
		{"90-day cert, 60 days left", 90 * day, 60 * day, false},
		{"90-day cert, 29 days left", 90 * day, 29 * day, true},
		{"6-day cert, 5 days left", 6 * day, 5 * day, false},
		{"6-day cert, 1 day left", 6 * day, day, true},
		{"expired", 90 * day, -day, true},
	} {
		leaf := &x509.Certificate{NotBefore: now.Add(c.remaining - c.lifetime), NotAfter: now.Add(c.remaining)}
		if got := renewDue(leaf, now); got != c.due {
			t.Errorf("%s: due = %v, want %v", c.name, got, c.due)
		}
	}
}

func TestUpdateKeepsSecrets(t *testing.T) {
	m, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := Settings{Domains: []string{"IBS.example.com"}, Provider: "cloudflare", CA: "staging",
		Env: map[string]string{"CF_DNS_API_TOKEN": "secret-1", "CLOUDFLARE_TTL": "300"}}
	if err := m.Update(s); err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if st.Domains[0] != "ibs.example.com" || st.Env["CF_DNS_API_TOKEN"] != "" || st.Env["CLOUDFLARE_TTL"] != "300" ||
		len(st.SecretsSet) != 1 || st.SecretsSet[0] != "CF_DNS_API_TOKEN" {
		t.Fatalf("status leaks or loses settings: %+v", st)
	}
	// The browser sends the secret back empty: the stored value stays.
	s.Env = map[string]string{"CF_DNS_API_TOKEN": ""}
	if err := m.Update(s); err != nil {
		t.Fatal(err)
	}
	if m.set.Env["CF_DNS_API_TOKEN"] != "secret-1" {
		t.Fatal("secret was dropped")
	}
	// Another provider doesn't inherit it, and its missing credentials are an error.
	if err := m.Update(Settings{Domains: []string{"ibs.example.com"}, Provider: "hetzner"}); err == nil {
		t.Fatal("expected missing credentials error")
	}
	// Settings survive a restart; turning HTTPS off clears them.
	m2, _ := New(m.dir, nil)
	if m2.set.Env["CF_DNS_API_TOKEN"] != "secret-1" {
		t.Fatal("settings not stored")
	}
	if err := m2.Update(Settings{}); err != nil || m2.Status().Enabled() {
		t.Fatalf("turning off: %v", err)
	}
	if _, err := New(t.TempDir(), &Settings{Domains: []string{"x.example.com"}, Provider: "nope"}); err == nil {
		t.Fatal("bad environment settings must be an error")
	}
	if err := m2.Update(Settings{Domains: []string{"x.example.com"}, Provider: "cloudflare"}); err == nil {
		t.Fatal("missing credentials must be an error")
	}
}

func TestNames(t *testing.T) {
	for name, ok := range map[string]bool{"ibs.example.com": true, "*.ibs.example.com": true, "localhost": false,
		"192.168.1.2": false, "-a.example.com": false, "a_b.example.com": false, "": false} {
		if validName(name) != ok {
			t.Errorf("validName(%q) = %v", name, !ok)
		}
	}
	names := []string{"*.ibs.example.com", "ibs.example.com"}
	for host, ok := range map[string]bool{"ibs.example.com": true, "s3.ibs.example.com": true,
		"a.b.ibs.example.com": false, "example.com": false, "192.168.1.2": false} {
		if covers(names, host) != ok {
			t.Errorf("covers(%q) = %v", host, !ok)
		}
	}
}

func TestBackupRestore(t *testing.T) {
	m, _ := New(t.TempDir(), nil)
	var backups int
	m.OnChange(func() { backups++ })
	if err := m.Update(Settings{Domains: []string{"ibs.example.com"}, Provider: "cloudflare",
		Env: map[string]string{"CF_DNS_API_TOKEN": "secret-1"}}); err != nil {
		t.Fatal(err)
	}
	if backups != 1 {
		t.Fatalf("OnChange calls: %d", backups)
	}
	blob, err := m.Export()
	if err != nil {
		t.Fatal(err)
	}

	// A new manager (a restored site) gets everything back.
	m2, _ := New(t.TempDir(), nil)
	if ok, err := m2.Import(blob); !ok || err != nil {
		t.Fatalf("import: %v %v", ok, err)
	}
	if st := m2.Status(); st.Domains[0] != "ibs.example.com" || m2.set.Env["CF_DNS_API_TOKEN"] != "secret-1" || m2.provider == nil {
		t.Fatalf("restored: %+v", st)
	}
	// A manager with settings of its own keeps them.
	if ok, _ := m.Import(blob); ok {
		t.Fatal("import over existing settings")
	}
	// After https-off the backup doesn't bring HTTPS back.
	m3dir := t.TempDir()
	if err := Disable(m3dir); err != nil {
		t.Fatal(err)
	}
	m3, _ := New(m3dir, nil)
	if ok, _ := m3.Import(blob); ok || m3.Status().Enabled() {
		t.Fatal("https-off was undone by the backup")
	}
	// Paths can't escape the directory.
	m4, _ := New(t.TempDir(), nil)
	if _, err := m4.Import([]byte(`{"../evil":"x"}`)); err == nil {
		t.Fatal("path outside the directory accepted")
	}
}
