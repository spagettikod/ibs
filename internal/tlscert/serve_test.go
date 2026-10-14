package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testCert(t *testing.T, names ...string) *tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func TestDualListenerAndGuard(t *testing.T) {
	m := &Manager{wake: make(chan struct{}, 1)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: m.Guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, map[bool]string{true: "https", false: "http"}[r.TLS != nil])
	}), false)}
	go srv.Serve(m.Listener(ln))
	defer srv.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	plain := "http://" + ln.Addr().String()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(c *http.Client, url string) (int, string, string) {
		t.Helper()
		resp, err := c.Get(url)
		if err != nil {
			return 0, err.Error(), ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header.Get("Location")
	}

	if _, body, _ := get(noRedirect, plain+"/"); body != "http" {
		t.Fatalf("plain HTTP without certificate: %q", body)
	}

	cert := testCert(t, "ibs.example.com")
	m.mu.Lock()
	m.set = Settings{Domains: []string{"ibs.example.com"}}
	m.cert, m.leaf = cert, cert.Leaf
	m.mu.Unlock()
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	secure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "ibs.example.com"}}}
	if _, body, _ := get(secure, "https://"+ln.Addr().String()+"/"); body != "https" {
		t.Fatalf("HTTPS on the same port: %q", body)
	}
	if _, body, _ := get(noRedirect, plain+"/"); body != "http" {
		t.Fatalf("plain HTTP must keep working until required: %q", body)
	}

	m.mu.Lock()
	m.set.Require = true
	m.mu.Unlock()
	code, _, loc := get(noRedirect, plain+"/buckets?x=1")
	if code != http.StatusPermanentRedirect || loc != "https://ibs.example.com:"+itoa(port)+"/buckets?x=1" {
		t.Fatalf("page redirect: %d %q", code, loc)
	}
	code, body, _ := get(noRedirect, plain+"/api/state")
	if code != http.StatusMisdirectedRequest || !strings.Contains(body, `"https_url":"https://ibs.example.com:`) {
		t.Fatalf("API over plain HTTP: %d %s", code, body)
	}
	if _, body, _ := get(secure, "https://"+ln.Addr().String()+"/"); body != "https" {
		t.Fatalf("HTTPS when required: %q", body)
	}
}

func itoa(i int) string { return big.NewInt(int64(i)).String() }
