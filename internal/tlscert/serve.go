package tlscert

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Listener serves plain HTTP and, once a certificate is in place, HTTPS on
// the same port: a connection that starts with a TLS handshake gets TLS.
// So turning HTTPS on in the web interface needs no restart, and the plain
// address keeps working until HTTPS is required.
func (m *Manager) Listener(ln net.Listener) net.Listener {
	l := &dualListener{Listener: ln, conns: make(chan net.Conn), done: make(chan struct{}),
		tls: &tls.Config{GetCertificate: m.GetCertificate, MinVersion: tls.VersionTLS12}, m: m}
	go l.loop()
	return l
}

type dualListener struct {
	net.Listener
	m     *Manager
	tls   *tls.Config
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
	err   error // why the listener stopped, set before done is closed
}

func (l *dualListener) loop() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			l.once.Do(func() { l.err = err; close(l.done) })
			l.Listener.Close()
			return
		}
		go l.sniff(c)
	}
}

// sniff looks at the first byte of a connection: 0x16 starts a TLS handshake.
func (l *dualListener) sniff(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	b, err := br.Peek(1)
	c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return
	}
	var conn net.Conn = &peekedConn{Conn: c, r: br}
	if b[0] == 0x16 {
		conn = tls.Server(conn, l.tls)
	}
	select {
	case l.conns <- conn:
	case <-l.done:
		c.Close()
	}
}

func (l *dualListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		if l.err != nil {
			return nil, l.err
		}
		return nil, net.ErrClosed
	}
}

func (l *dualListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Guard refuses plain HTTP when HTTPS is required and a certificate is in
// place. Web pages are redirected to the HTTPS address; API calls get 421
// with that address, so the web interface can move there; S3 requests get an
// S3 error.
func (m *Manager) Guard(h http.Handler, s3 bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil || !m.required() {
			h.ServeHTTP(w, r)
			return
		}
		url := "https://" + m.httpsHost(r) + r.URL.RequestURI()
		switch {
		case s3:
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+
				`<Error><Code>InsecureRequest</Code><Message>HTTPS is required: use %s</Message></Error>`, xmlEscape(url))
		case strings.HasPrefix(r.URL.Path, "/api/"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMisdirectedRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "HTTPS is required", "https_url": url})
		default:
			http.Redirect(w, r, url, http.StatusPermanentRedirect)
		}
	})
}

func (m *Manager) required() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.set.Require && m.cert != nil
}

// httpsHost is the host:port to use over HTTPS: the request's host when the
// certificate covers it, otherwise the certificate's first plain name.
func (m *Manager) httpsHost(r *http.Request) string {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, ""
	}
	m.mu.RLock()
	names := m.set.Domains
	m.mu.RUnlock()
	if !covers(names, host) {
		for _, n := range names {
			if !strings.HasPrefix(n, "*.") {
				host = n
				break
			}
		}
	}
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

func covers(names []string, host string) bool {
	host = strings.ToLower(host)
	for _, n := range names {
		if n == host {
			return true
		}
		if base, ok := strings.CutPrefix(n, "*."); ok {
			if sub, ok := strings.CutSuffix(host, "."+base); ok && sub != "" && !strings.Contains(sub, ".") {
				return true
			}
		}
	}
	return false
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
