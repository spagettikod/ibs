// Package blockcodec encrypts file blocks with a bucket key.
//
// Every block gets a random GCM nonce, stored at its start, so no two blocks
// are alike, even of identical data: IBS doesn't deduplicate. A block's ID is
// the SHA-256 of its ciphertext, which lets any copy be verified without the
// key. Small files' encrypted blocks are placed one after another in shared
// blocks (site/sharedblocks.go); each keeps its own nonce and check value.
package blockcodec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// BlockSize is how large blocks are; a file's blocks after its first
// LargeAfter bytes are LargeBlockSize, once every site accepts those.
const (
	BlockSize      = 4 << 20
	LargeBlockSize = 16 << 20
	LargeAfter     = 1 << 30
)

var ErrCorrupt = errors.New("block content does not match its id")

func ID(ciphertext []byte) string {
	h := sha256.Sum256(ciphertext)
	return hex.EncodeToString(h[:])
}

func Verify(id string, ciphertext []byte) bool {
	return ID(ciphertext) == id
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
