// Package blockcodec encrypts file blocks with a bucket key.
//
// Encryption is convergent within a bucket: the GCM nonce is derived from the
// plaintext, so identical blocks in the same bucket produce identical
// ciphertext and are stored once. A block's ID is the SHA-256 of its
// ciphertext, which lets any replica verify a block without knowing the key.
package blockcodec

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
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
	nk := hmac.New(sha256.New, key)
	nk.Write([]byte("ibs/block-nonce/v1"))
	m := hmac.New(sha256.New, nk.Sum(nil))
	m.Write(plain)
	nonce := m.Sum(nil)[:gcm.NonceSize()]
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
