package site

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestInviteCodeFormat(t *testing.T) {
	code := formatCode("relay.example.com:7444", "7", []string{"comet", "harbor", "violet"})
	if code != "relay.example.com 7-comet-harbor-violet" {
		t.Fatalf("code %q", code)
	}
	addr, n, w, err := parseCode("  Relay.Example.com   7-Comet-Harbor-Violet ")
	if err != nil || addr != "relay.example.com:7444" || n != "7" || w != "comet-harbor-violet" {
		t.Fatalf("parse: %q %q %q %v", addr, n, w, err)
	}
	if addr, _, _, err := parseCode("relay.example.com:9444 12-comet-harbor-violet"); err != nil || addr != "relay.example.com:9444" {
		t.Fatalf("other port: %q %v", addr, err)
	}
	for _, bad := range []string{"", "relay.example.com", "relay 7-comet-harbor", "relay 07-comet-harbor-violet",
		"relay x-comet-harbor-violet", "relay 7-comet-harbor-notaword", "a b c"} {
		if _, _, _, err := parseCode(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if err := func() error { _, _, _, err := parseCode("relay 7-comet-harbor-vilet"); return err }(); err == nil || !strings.Contains(err.Error(), "vilet") {
		t.Errorf("a misspelt word should be named: %v", err)
	}
}

// The joining site gets the invite only with the right words, and a code
// works once.
func TestInviteCodeExchange(t *testing.T) {
	s := newTestSite(t)
	put := func(number, words string) {
		s.DB.Exec(`INSERT INTO invite_codes(number, relay_id, words, invite, expires) VALUES(?, 'r', ?, 'ibs2.the-invite', ?)`,
			number, words, time.Now().Add(time.Hour).Unix())
	}
	try := func(number, words string) (string, error) {
		a, b := net.Pipe()
		go s.answerCode(a, number)
		defer b.Close()
		return fetchInvite(b, number, words)
	}

	put("7", "comet-harbor-violet")
	if inv, err := try("7", "comet-harbor-violet"); err != nil || inv != "ibs2.the-invite" {
		t.Fatalf("right words: %q %v", inv, err)
	}
	if _, err := try("7", "comet-harbor-violet"); err == nil {
		t.Fatal("a code worked twice")
	}

	put("8", "comet-harbor-violet")
	if _, err := try("8", "comet-harbor-valley"); err == nil || !strings.Contains(err.Error(), "don't match") {
		t.Fatalf("wrong words: %v", err)
	}
	if _, err := try("8", "comet-harbor-violet"); err == nil {
		t.Fatal("a code still worked after a wrong guess")
	}
}
