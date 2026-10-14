package tlscert

//go:generate python3 ../../scripts/gen-dns-providers.py

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/go-acme/lego/v4/challenge"
)

// The DNS providers IBS includes are in providers_gen.go. lego has many more,
// but some pull in large cloud SDKs; rfc2136, acme-dns, exec and httpreq work
// with almost any DNS setup. The codes and settings are lego's:
// https://go-acme.github.io/lego/dns/

// Field is a setting of a DNS provider, passed to it as an environment variable.
type Field struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Secret      bool   `json:"secret"`
}

// Provider describes a DNS provider for the web interface.
type Provider struct {
	Code        string  `json:"code"`
	Name        string  `json:"name"`
	Docs        string  `json:"docs"`
	Credentials []Field `json:"credentials"`
	Additional  []Field `json:"additional"`
}

// Providers lists the supported DNS providers, sorted by code.
func Providers() []Provider { return providerList }

func providerCodes() string {
	codes := make([]string, len(providerList))
	for i, p := range providerList {
		codes[i] = p.Code
	}
	return strings.Join(codes, ", ")
}

func providerInfo(code string) (Provider, bool) {
	for _, p := range providerList {
		if p.Code == code {
			return p, true
		}
	}
	return Provider{}, false
}

// envMu serializes provider creation: lego providers read their settings from
// the environment, so settings from the web interface are set there briefly.
var envMu sync.Mutex

// newProvider creates a DNS provider. env holds settings from the web
// interface; they take precedence over the process environment.
func newProvider(code string, env map[string]string) (challenge.Provider, error) {
	if code == "acmedns" {
		code = "acme-dns"
	}
	f, ok := constructors[code]
	if !ok {
		return nil, fmt.Errorf("unsupported DNS provider %q; supported: %s", code, providerCodes())
	}
	envMu.Lock()
	defer envMu.Unlock()
	for k, v := range env {
		if v == "" {
			continue
		}
		old, had := os.LookupEnv(k)
		os.Setenv(k, v)
		if had {
			defer os.Setenv(k, old)
		} else {
			defer os.Unsetenv(k)
		}
	}
	p, err := f()
	if err != nil {
		return nil, fmt.Errorf("DNS provider %s: %w", code, err)
	}
	return p, nil
}
