package relay

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func TestNewToken(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	oldTok, _ := ParseToken(s.Token())
	if got, _ := ReadToken(dir); got != s.Token() {
		t.Fatalf("ReadToken = %q, want %q", got, s.Token())
	}
	member, _, _ := ed25519.GenerateKey(rand.Reader)
	if !s.authorize(member, oldTok.Secret, "member") {
		t.Fatal("old token should register a site")
	}

	time.Sleep(10 * time.Millisecond) // a newer modification time
	newStr, err := NewToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	newTok, _ := ParseToken(newStr)
	if newTok.Secret == oldTok.Secret || !newTok.Key.Equal(oldTok.Key) {
		t.Fatal("new token should keep the key and change the secret")
	}

	s.mu.Lock()
	changed := s.reload()
	s.mu.Unlock()
	if !changed {
		t.Fatal("running relay did not pick up the new token")
	}
	stranger, _, _ := ed25519.GenerateKey(rand.Reader)
	if s.authorize(stranger, oldTok.Secret, "stranger") {
		t.Error("old token must not register new sites")
	}
	if !s.authorize(member, "", "member") {
		t.Error("registered site must keep working")
	}
	if !s.authorize(stranger, newTok.Secret, "stranger") {
		t.Error("new token should register a site")
	}
	if s.Token() != newStr {
		t.Error("relay should hand out the new token")
	}

	// A restart keeps the new token and both sites.
	s2, err := New(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if s2.Token() != newStr || len(s2.st.Sites) != 2 {
		t.Errorf("after restart: token %v, %d sites", s2.Token() == newStr, len(s2.st.Sites))
	}
}
