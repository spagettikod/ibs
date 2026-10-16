// Package blockcodec encrypts file blocks with a bucket key.
//
// Every block gets a random GCM nonce, stored at its start, so no two blocks
// are alike, even of identical data: each file owns its blocks (IBS doesn't
// deduplicate; the tool that writes backups into it does). Blocks written
// before 1.6.0 used a nonce derived from the data; they decrypt the same way.
// A block's ID is the SHA-256 of its ciphertext, which lets any copy be
// verified without the key.
package blockcodec

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

const BlockSize = 4 << 20

var ErrCorrupt = errors.New("block content does not match its id")

func ID(ciphertext []byte) string {
	h := sha256.Sum256(ciphertext)
	return hex.EncodeToString(h[:])
}

func Verify(id string, ciphertext []byte) bool {
	return ID(ciphertext) == id
}

func Encrypt(key, plain []byte) (id string, ciphertext []byte) {
	gcm := newGCM(key)
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	ct := gcm.Seal(append([]byte{}, nonce...), nonce, plain, nil)
	return ID(ct), ct
}

func Decrypt(key []byte, id string, ciphertext []byte) ([]byte, error) {
	if !Verify(id, ciphertext) {
		return nil, ErrCorrupt
	}
	gcm := newGCM(key)
	if len(ciphertext) < gcm.NonceSize() {
		return nil, ErrCorrupt
	}
	return gcm.Open(nil, ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():], nil)
}

func newGCM(key []byte) cipher.AEAD {
	b, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		panic(err)
	}
	return g
}

// ValidID reports whether s looks like a block id (64 lowercase hex chars).
func ValidID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
