package keys

import (
	"bytes"
	"testing"

	"filippo.io/age"
)

// The site's age identity comes from the seed: the same seed, the same key,
// which age itself parses and round-trips.
func TestAgeIdentityFromSeed(t *testing.T) {
	seed := NewSeed()
	a, err := FromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := FromSeed(seed)
	if a.Age.String() != b.Age.String() {
		t.Fatal("the same seed gave different age identities")
	}
	if p, err := age.ParseHybridIdentity(a.Age.String()); err != nil || p.String() != a.Age.String() {
		t.Fatalf("age doesn't round-trip the derived key: %v", err)
	}
	other, _ := FromSeed(NewSeed())
	if other.Age.String() == a.Age.String() {
		t.Fatal("different seeds, same identity")
	}
}

func TestSealAge(t *testing.T) {
	id, _ := FromSeed(NewSeed())
	sealed, err := SealAge(id.Age.Recipient(), "bucket:photos", []byte("secret"))
	if err != nil || !IsAge(sealed) {
		t.Fatalf("seal: %v", err)
	}
	if got, err := OpenAge(id.Age, "bucket:photos", sealed); err != nil || !bytes.Equal(got, []byte("secret")) {
		t.Fatalf("open: %q, %v", got, err)
	}
	if _, err := OpenAge(id.Age, "bucket:other", sealed); err == nil {
		t.Fatal("opened with the wrong label")
	}
	other, _ := FromSeed(NewSeed())
	if _, err := OpenAge(other.Age, "bucket:photos", sealed); err == nil {
		t.Fatal("opened with another site's key")
	}
	if IsAge(Seal(id.Master, []byte("x"), nil)) {
		t.Fatal("the old format looks like age")
	}
}
