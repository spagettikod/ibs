// Package tlscert gets and renews the manager's HTTPS certificate from an ACME
// CA (Let's Encrypt by default) with the DNS-01 challenge, so no port has to be
// reachable from the internet. It uses lego and a selection of its DNS
// providers.
//
// The settings come from the web interface (kept in the data directory) or
// from environment variables, which then take precedence and can't be changed
// in the web interface.
package tlscert

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	legolog "github.com/go-acme/lego/v4/log"
	"github.com/go-acme/lego/v4/registration"
)

// StagingCA is Let's Encrypt's test CA. Its certificates are not trusted by
// browsers, but its rate limits are generous: use it to try out a setup.
const StagingCA = "https://acme-staging-v02.api.letsencrypt.org/directory"

// renewBefore is how long before expiry a certificate is renewed at most;
// short-lived certificates are renewed when a third of their lifetime is left.
const renewBefore = 30 * 24 * time.Hour

// Settings configure HTTPS. No domains means HTTPS is off.
type Settings struct {
	Domains  []string          `json:"domains"`            // names on the certificate; the first is its main name
	Email    string            `json:"email,omitempty"`    // ACME account contact
	Provider string            `json:"provider,omitempty"` // lego DNS provider code, e.g. "cloudflare"
	Env      map[string]string `json:"env,omitempty"`      // the provider's settings (its environment variables)
	CA       string            `json:"ca,omitempty"`       // ACME directory URL; "" = Let's Encrypt, "staging" = its test CA
	// Resolvers are DNS servers (host:port) to check the TXT records with.
	Resolvers []string `json:"resolvers,omitempty"`
	// PropagationWait, e.g. "2m", waits that long after creating the TXT
	// records instead of checking that they are visible. For networks where
	// the check cannot work, e.g. when outgoing DNS is blocked.
	PropagationWait string `json:"propagation_wait,omitempty"`
	// Require refuses plain HTTP once a certificate is in place.
	Require bool `json:"require_https"`
}

func (s Settings) Enabled() bool { return len(s.Domains) > 0 }

func (s Settings) caURL() string {
	switch s.CA {
	case "":
		return lego.LEDirectoryProduction
	case "staging":
		return StagingCA
	}
	return s.CA
}

// Status is what the web interface shows about HTTPS.
type Status struct {
	Settings
	FromEnv   bool      `json:"from_env"` // set with environment variables, read-only
	Active    bool      `json:"active"`   // a certificate is in place and served
	Issuer    string    `json:"issuer,omitempty"`
	NotAfter  time.Time `json:"not_after,omitzero"`
	Error     string    `json:"error,omitempty"`
	ErrorTime time.Time `json:"error_time,omitzero"`
	// SecretsSet names the provider settings that are secret and set; their
	// values are never sent to the browser.
	SecretsSet []string `json:"secrets_set,omitempty"`
}

type Manager struct {
	dir     string
	fromEnv bool
	log     *slog.Logger
	wake    chan struct{}

	mu       sync.RWMutex
	set      Settings
	provider challenge.Provider
	cert     *tls.Certificate
	leaf     *x509.Certificate
	err      error
	errTime  time.Time
	onChange func()
}

// New creates the manager, keeping its files in dir. env, when not nil, are
// settings from environment variables: they are used instead of the stored
// ones, and a mistake in them is an error. Call Run to get and renew the
// certificate.
func New(dir string, env *Settings) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &Manager{dir: dir, log: slog.With("component", "tls"), wake: make(chan struct{}, 1)}
	// lego's progress messages go to the same log, as debug lines.
	legolog.Logger = slog.NewLogLogger(m.log.Handler(), slog.LevelDebug)
	if env != nil {
		m.fromEnv = true
		env.Domains = cleanList(env.Domains)
		p, err := validate(*env)
		if err != nil {
			return nil, err
		}
		m.set, m.provider = *env, p
	} else if b, err := os.ReadFile(m.settingsPath()); err == nil {
		if err := json.Unmarshal(b, &m.set); err != nil {
			return nil, fmt.Errorf("read %s: %w", m.settingsPath(), err)
		}
		if m.set.Enabled() {
			if m.provider, err = validate(m.set); err != nil {
				m.err, m.errTime = err, time.Now()
				m.log.Error("HTTPS settings", "err", err)
			}
		}
	}
	m.loadCert()
	return m, nil
}

// loadCert loads the stored certificate if it is for the configured names.
// Callers hold m.mu or own m.
func (m *Manager) loadCert() {
	m.cert, m.leaf = nil, nil
	if !m.set.Enabled() {
		return
	}
	if c, err := tls.LoadX509KeyPair(m.path("cert.pem"), m.path("key.pem")); err == nil && sameNames(c.Leaf, m.set.Domains) {
		m.cert, m.leaf = &c, c.Leaf
		m.log.Info("certificate loaded", "domains", m.set.Domains, "expires", c.Leaf.NotAfter.Format(time.DateOnly))
	}
}

// validate checks settings and creates their DNS provider.
func validate(s Settings) (challenge.Provider, error) {
	if !s.Enabled() {
		return nil, nil
	}
	for _, d := range s.Domains {
		if !validName(d) {
			return nil, fmt.Errorf("%q is not a valid host name", d)
		}
	}
	if s.Provider == "" {
		return nil, errors.New("a DNS provider is required (IBS_TLS_DNS_PROVIDER)")
	}
	if s.PropagationWait != "" {
		if d, err := time.ParseDuration(s.PropagationWait); err != nil || d < 0 {
			return nil, fmt.Errorf("invalid wait %q (use e.g. 2m)", s.PropagationWait)
		}
	}
	return newProvider(s.Provider, s.Env)
}

func validName(d string) bool {
	d = strings.TrimPrefix(d, "*.")
	if d == "" || len(d) > 253 || !strings.Contains(d, ".") || net.ParseIP(d) != nil {
		return false
	}
	for _, label := range strings.Split(d, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				return false
			}
		}
	}
	return true
}

// Update changes the settings from the web interface. Secret provider
// settings left empty keep their stored value when the provider stays the
// same, since the browser never gets them.
func (m *Manager) Update(s Settings) error {
	if m.fromEnv {
		return errors.New("HTTPS is set with environment variables (IBS_TLS_*); change them there")
	}
	s.Domains = cleanList(s.Domains)
	s.Resolvers = cleanList(s.Resolvers)
	s.Email = strings.TrimSpace(s.Email)
	m.mu.RLock()
	old := m.set
	m.mu.RUnlock()
	env := map[string]string{}
	if info, ok := providerInfo(s.Provider); ok {
		for _, f := range append(slices.Clone(info.Credentials), info.Additional...) {
			v := strings.TrimSpace(s.Env[f.Name])
			if v == "" && f.Secret && s.Provider == old.Provider {
				v = old.Env[f.Name]
			}
			if v != "" {
				env[f.Name] = v
			}
		}
	}
	s.Env = env
	if !s.Enabled() {
		s = Settings{}
	}
	p, err := validate(s)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := writeFile(m.settingsPath(), b); err != nil {
		return err
	}
	m.mu.Lock()
	m.set, m.provider, m.err = s, p, nil
	if m.leaf != nil && (!s.Enabled() || !sameNames(m.leaf, s.Domains) || s.caURL() != old.caURL()) {
		m.cert, m.leaf = nil, nil
	}
	m.mu.Unlock()
	if s.Enabled() {
		m.log.Info("HTTPS settings changed", "domains", s.Domains, "dns_provider", s.Provider)
	} else {
		m.log.Info("HTTPS turned off")
	}
	m.kick()
	m.changed()
	return nil
}

// Check validates settings without saving them, e.g. before creating a site.
func Check(s Settings) error {
	_, err := validate(s)
	return err
}

// Disable turns off HTTPS settings made in the web interface (ibs https-off).
// It stores empty settings rather than none, so the site's backup doesn't
// bring the old ones back.
func Disable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, "settings.json"), []byte("{}\n"))
}

func cleanList(in []string) []string {
	var out []string
	for _, v := range in {
		for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
			out = append(out, strings.ToLower(f))
		}
	}
	return out
}

// Status reports the settings, without secret values, and the certificate.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := Status{Settings: m.set, FromEnv: m.fromEnv, Active: m.cert != nil}
	if st.Domains == nil {
		st.Domains = []string{}
	}
	st.Env = map[string]string{}
	info, _ := providerInfo(m.set.Provider)
	secret := map[string]bool{}
	for _, f := range append(slices.Clone(info.Credentials), info.Additional...) {
		secret[f.Name] = f.Secret
	}
	for k, v := range m.set.Env {
		if secret[k] {
			st.SecretsSet = append(st.SecretsSet, k)
		} else {
			st.Env[k] = v
		}
	}
	slices.Sort(st.SecretsSet)
	if m.leaf != nil {
		st.Issuer = m.leaf.Issuer.CommonName
		st.NotAfter = m.leaf.NotAfter
	}
	if m.err != nil {
		st.Error, st.ErrorTime = m.err.Error(), m.errTime
	}
	return st
}

// Active reports whether a certificate is in place.
func (m *Manager) Active() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cert != nil
}

// GetCertificate is for tls.Config.
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cert == nil {
		return nil, errors.New("no certificate yet")
	}
	return m.cert, nil
}

// Run gets a certificate when HTTPS is on and renews it 30 days before it
// expires (short-lived ones when a third of their lifetime is left). Failures
// are retried with growing pauses, up to six hours. Changed settings take
// effect right away.
func (m *Manager) Run(ctx context.Context) {
	retry := time.Minute
	for {
		wait := 12 * time.Hour
		if m.due() {
			if err := m.obtain(); err != nil {
				m.mu.Lock()
				m.err, m.errTime = err, time.Now()
				m.mu.Unlock()
				m.log.Error("could not get certificate", "err", err, "retry_in", retry)
				wait = retry
				retry = min(retry*2, 6*time.Hour)
			} else {
				retry = time.Minute
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
			retry = time.Minute
		case <-time.After(wait):
		}
	}
}

func (m *Manager) due() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.set.Enabled() && m.provider != nil && (m.leaf == nil || renewDue(m.leaf, time.Now()))
}

func renewDue(leaf *x509.Certificate, now time.Time) bool {
	return leaf.NotAfter.Sub(now) < min(renewBefore, leaf.NotAfter.Sub(leaf.NotBefore)/3)
}

func (m *Manager) obtain() error {
	m.mu.RLock()
	set, provider := m.set, m.provider
	m.mu.RUnlock()
	ca := set.caURL()
	m.log.Info("requesting certificate", "domains", set.Domains, "ca", ca, "dns_provider", set.Provider)
	user, err := m.account(ca, set.Email)
	if err != nil {
		return err
	}
	lc := lego.NewConfig(user)
	lc.CADirURL = ca
	lc.Certificate.KeyType = certcrypto.EC256
	lc.UserAgent = "ibs"
	client, err := lego.NewClient(lc)
	if err != nil {
		return fmt.Errorf("ACME CA %s: %w", ca, err)
	}
	wait, _ := time.ParseDuration(set.PropagationWait)
	if err := client.Challenge.SetDNS01Provider(provider,
		dns01.CondOption(len(set.Resolvers) > 0, dns01.AddRecursiveNameservers(set.Resolvers)),
		dns01.CondOption(wait > 0, dns01.PropagationWait(wait, true))); err != nil {
		return err
	}
	if user.reg == nil {
		if user.reg, err = client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true}); err != nil {
			return fmt.Errorf("register ACME account: %w", err)
		}
		if err := m.saveAccount(ca, user); err != nil {
			return err
		}
	}
	res, err := client.Certificate.Obtain(certificate.ObtainRequest{Domains: set.Domains, Bundle: true})
	if err != nil {
		return err
	}
	c, err := tls.X509KeyPair(res.Certificate, res.PrivateKey)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !slices.Equal(m.set.Domains, set.Domains) || m.set.caURL() != ca {
		return nil // the settings changed meanwhile; the loop runs again
	}
	if err := writeFile(m.path("cert.pem"), res.Certificate); err != nil {
		return err
	}
	if err := writeFile(m.path("key.pem"), res.PrivateKey); err != nil {
		return err
	}
	m.cert, m.leaf, m.err = &c, c.Leaf, nil
	m.log.Info("certificate issued", "domains", set.Domains, "issuer", c.Leaf.Issuer.CommonName,
		"expires", c.Leaf.NotAfter.Format(time.DateOnly))
	go m.changed()
	return nil
}

func (m *Manager) path(name string) string { return filepath.Join(m.dir, name) }
func (m *Manager) settingsPath() string    { return m.path("settings.json") }

// sameNames reports whether the certificate covers exactly these names.
func sameNames(leaf *x509.Certificate, domains []string) bool {
	have := slices.Clone(leaf.DNSNames)
	want := slices.Clone(domains)
	slices.Sort(have)
	slices.Sort(want)
	return slices.Equal(have, want)
}

// account is the ACME account. It is kept per CA, so switching between the
// staging and production CA does not mix them up.
type account struct {
	email string
	key   crypto.PrivateKey
	reg   *registration.Resource
}

func (a *account) GetEmail() string                        { return a.email }
func (a *account) GetRegistration() *registration.Resource { return a.reg }
func (a *account) GetPrivateKey() crypto.PrivateKey        { return a.key }

func (m *Manager) accountDir(ca string) string {
	h := sha256.Sum256([]byte(ca))
	return filepath.Join(m.dir, "accounts", hex.EncodeToString(h[:6]))
}

func (m *Manager) account(ca, email string) (*account, error) {
	dir := m.accountDir(ca)
	a := &account{email: email}
	if pemKey, err := os.ReadFile(filepath.Join(dir, "account.key")); err == nil {
		if a.key, err = certcrypto.ParsePEMPrivateKey(pemKey); err != nil {
			return nil, fmt.Errorf("account key: %w", err)
		}
		if b, err := os.ReadFile(filepath.Join(dir, "account.json")); err == nil {
			var saved struct {
				Email        string                 `json:"email"`
				Registration *registration.Resource `json:"registration"`
			}
			if json.Unmarshal(b, &saved) == nil && saved.Email == a.email {
				a.reg = saved.Registration
			}
		}
		return a, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(dir, "account.key"), certcrypto.PEMEncode(key)); err != nil {
		return nil, err
	}
	a.key = key
	return a, nil
}

func (m *Manager) saveAccount(ca string, a *account) error {
	b, err := json.MarshalIndent(map[string]any{"ca": ca, "email": a.email, "registration": a.reg}, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(m.accountDir(ca), "account.json"), b)
}

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
