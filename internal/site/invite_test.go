package site

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"ibs/internal/keys"
	"ibs/internal/relay"
)

func TestInviteFormats(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	rk1, _, _ := ed25519.GenerateKey(rand.Reader)
	rk2, _, _ := ed25519.GenerateKey(rand.Reader)
	inv := invite{
		PubKey:  base64.RawURLEncoding.EncodeToString(pub),
		Secret:  keys.RandomToken(12),
		Address: "192.168.1.20:7443",
		Relays: []string{
			relay.Token{Address: "ibs-relay.example.com:7444", Key: rk1, Secret: keys.RandomToken(24)}.EncodeWithAddress(),
			relay.Token{Address: "relay2.example.org:7444", Key: rk2, Secret: keys.RandomToken(24)}.EncodeWithAddress(),
		},
	}
	tok, err := encodeInvite(inv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "ibs2.") {
		t.Fatalf("new invites start with ibs2.: %s", tok[:8])
	}
	got, err := decodeInvite(tok)
	if err != nil || !reflect.DeepEqual(got, inv) {
		t.Fatalf("round trip:\n got %+v\nwant %+v (%v)", got, inv, err)
	}
	// Wrapped when pasted.
	if got, err := decodeInvite(tok[:40] + "\n  " + tok[40:]); err != nil || got.Secret != inv.Secret {
		t.Fatalf("wrapped invite: %v", err)
	}

	// A typical invite (one relay) is short; the old form was about twice as long.
	one := inv
	one.Address, one.Relays = "", inv.Relays[:1]
	short, _ := encodeInvite(one)
	one.Name = "roland-001"
	j, _ := json.Marshal(one)
	old := "ibs1." + base64.RawURLEncoding.EncodeToString(j)
	t.Logf("one relay: %d characters (old form %d)", len(short), len(old))
	if len(short) > 220 || len(short)*2 > len(old)+40 {
		t.Fatalf("invite is %d characters, the old form %d", len(short), len(old))
	}

	// Old invites are still read, until 1.5.0.
	got, err = decodeInvite(old)
	if err != nil || got.Name != "roland-001" || got.Secret != one.Secret || len(got.Relays) != 1 {
		t.Fatalf("ibs1 invite: %+v %v", got, err)
	}

	for _, bad := range []string{"", "ibs2.", "ibs2.!!!", "ibs3." + tok[5:], tok[:len(tok)-6], tok + "AAAA", "ibs1.e30x"} {
		if _, err := decodeInvite(bad); err == nil {
			t.Errorf("accepted a malformed invite %q", bad)
		}
	}
}
