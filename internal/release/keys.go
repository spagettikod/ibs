package release

// DefaultKey is the maintainer's release key: built into every IBS build,
// always trusted and never removable. Its private half is the signing key,
// used with OpenSSL (~/.config/ibs/signing.key on the maintainer's machine;
// see DEVELOPMENT.md).
var DefaultKey = mustKey("ibspub1:HUX9q7eGaKKt0mBFsfMIrjChsjF3xH7lg7eOAwhIgm8", "Official release key")

// BuiltinKeys are the release keys built into this build: always trusted and
// never removable. IBS saves them in the data directory on start, so a
// launcher built earlier trusts them too. To rotate: add the new key here
// (keep the old one) and release; sign later releases with the new key.
var BuiltinKeys = []Key{DefaultKey}

// RevokedKeys are IDs of keys that must not be trusted any more, also when a
// data directory has them saved. After a key leaks: remove it from
// BuiltinKeys, add its ID here, and release IBS (signed with the new key) and
// a new launcher.
var RevokedKeys = []string{}

// Revoked reports whether a key ID is in RevokedKeys.
func Revoked(id string) bool {
	for _, r := range RevokedKeys {
		if r == id {
			return true
		}
	}
	return false
}

func mustKey(s, name string) Key {
	k, err := ParseKey(s)
	if err != nil {
		panic(err)
	}
	k.Name, k.Default = name, true
	return k
}
