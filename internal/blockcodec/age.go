package blockcodec

import (
	"bytes"
	"io"

	"filippo.io/age"
)

// Blocks since IBS 1.10 are age files encrypted to their bucket's age
// identity (post-quantum hybrid): each with its own random file key,
// ChaCha20-Poly1305 in 64 KB chunks. The ID is still the SHA-256 of the
// stored bytes, so any copy can be checked without a key. Blocks from before
// are AES-GCM (Encrypt, Decrypt); keys.IsAge tells them apart.

// EncryptAge encrypts plain as a block for recipient r.
func EncryptAge(r age.Recipient, plain []byte) (id string, ct []byte, err error) {
	var b bytes.Buffer
	w, err := age.Encrypt(&b, r)
	if err != nil {
		return "", nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return "", nil, err
	}
	if err := w.Close(); err != nil {
		return "", nil, err
	}
	return ID(b.Bytes()), b.Bytes(), nil
}

// DecryptAge decrypts a whole age block, checked against its ID.
func DecryptAge(identity age.Identity, id string, ct []byte) ([]byte, error) {
	if !Verify(id, ct) {
		return nil, ErrCorrupt
	}
	r, err := age.Decrypt(bytes.NewReader(ct), identity)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// DecryptAgeRange decrypts n bytes at off of an age block: only the 64 KB
// chunks they're in (age checks each one).
func DecryptAgeRange(identity age.Identity, id string, ct []byte, off, n int64) ([]byte, error) {
	if !Verify(id, ct) {
		return nil, ErrCorrupt
	}
	ra, size, err := age.DecryptReaderAt(bytes.NewReader(ct), int64(len(ct)), identity)
	if err != nil {
		return nil, err
	}
	if off < 0 || n < 0 || off+n > size {
		return nil, io.ErrUnexpectedEOF
	}
	b := make([]byte, n)
	if _, err := ra.ReadAt(b, off); err != nil && !(err == io.EOF && off+n == size) {
		return nil, err
	}
	return b, nil
}
