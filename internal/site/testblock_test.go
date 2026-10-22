package site

import (
	"filippo.io/age"

	"ibs/internal/blockcodec"
)

var testKey, _ = age.GenerateHybridIdentity()

// testBlock makes an encrypted block of plain, as stored.
func testBlock(plain []byte) (string, []byte) {
	id, ct, err := blockcodec.EncryptAge(testKey.Recipient(), plain)
	if err != nil {
		panic(err)
	}
	return id, ct
}
