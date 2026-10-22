package blockcodec

import (
	"bytes"
	"crypto/rand"
	"testing"

	"filippo.io/age"
)

func TestAgeBlocks(t *testing.T) {
	id, _ := age.GenerateHybridIdentity()
	plain := make([]byte, 300_000) // several 64 KB chunks
	rand.Read(plain)
	bid, ct, err := EncryptAge(id.Recipient(), plain)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("overhead: %d bytes", len(ct)-len(plain))
	got, err := DecryptAge(id, bid, ct)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip: %v", err)
	}
	for _, r := range [][2]int64{{0, 10}, {65530, 20}, {200_000, 100_000}, {299_999, 1}} {
		got, err := DecryptAgeRange(id, bid, ct, r[0], r[1])
		if err != nil || !bytes.Equal(got, plain[r[0]:r[0]+r[1]]) {
			t.Fatalf("range %v: %v", r, err)
		}
	}
	ct[len(ct)-1] ^= 1
	if _, err := DecryptAge(id, bid, ct); err != ErrCorrupt {
		t.Fatalf("damaged block: %v", err)
	}
	other, _ := age.GenerateHybridIdentity()
	ct[len(ct)-1] ^= 1
	if _, err := DecryptAge(other, bid, ct); err == nil {
		t.Fatal("decrypted with another bucket's key")
	}
}
