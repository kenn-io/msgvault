package pack

import (
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"math"

	"golang.org/x/crypto/chacha20poly1305"
)

// Crypter seals and opens pack content with XChaCha20-Poly1305 under a single
// repo key (docs/architecture/backup-format.md, Pack Files). The AAD binds every ciphertext to its identity:
// blob ID for blob frames, role plus object ID for metadata objects.
type Crypter struct {
	aead cipher.AEAD
}

// maxSealOverhead is the fixed number of bytes seal adds on top of the
// plaintext: a 24-byte XChaCha20 nonce prefix plus the 16-byte Poly1305 tag.
// Used by maxStoredLen (pack.go) to bound a footer entry's claimed StoredLen.
const maxSealOverhead = chacha20poly1305.NonceSizeX + chacha20poly1305.Overhead

// NewCrypter builds a Crypter for a 256-bit repo key.
func NewCrypter(key [32]byte) (*Crypter, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, fmt.Errorf("pack: initializing AEAD: %w", err)
	}
	return &Crypter{aead: aead}, nil
}

func (c *Crypter) seal(aad, plain []byte) ([]byte, error) {
	// Guard the capacity arithmetic below against int overflow for
	// pathologically large plaintexts (callers are bounded far under this by
	// maxRawLen, but the allocation math must not be able to wrap).
	if len(plain) > math.MaxInt-maxSealOverhead {
		return nil, fmt.Errorf("pack: plaintext too large to seal (%d bytes)", len(plain))
	}
	capacity := chacha20poly1305.NonceSizeX + len(plain) + c.aead.Overhead()
	nonce := make([]byte, chacha20poly1305.NonceSizeX, capacity)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("pack: generating nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plain, aad), nil
}

func (c *Crypter) open(aad, sealed []byte) ([]byte, error) {
	if len(sealed) < chacha20poly1305.NonceSizeX+c.aead.Overhead() {
		return nil, fmt.Errorf("%w: sealed data too short", ErrDecrypt)
	}
	nonce, ct := sealed[:chacha20poly1305.NonceSizeX], sealed[chacha20poly1305.NonceSizeX:]
	plain, err := c.aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecrypt, err)
	}
	if plain == nil {
		plain = []byte{}
	}
	return plain, nil
}

// SealBlob encrypts a stored frame, binding it to its blob ID.
func (c *Crypter) SealBlob(id BlobID, frame []byte) ([]byte, error) {
	return c.seal(id[:], frame)
}

// OpenBlob decrypts a sealed frame, verifying the blob ID binding.
func (c *Crypter) OpenBlob(id BlobID, sealed []byte) ([]byte, error) {
	return c.open(id[:], sealed)
}

func objectAAD(role, objectID string) []byte {
	aad := make([]byte, 0, len(role)+1+len(objectID))
	aad = append(aad, role...)
	aad = append(aad, 0)
	return append(aad, objectID...)
}

// SealObject encrypts a metadata object, binding it to a role and object ID
// (for example role "pack-footer" with the pack ULID).
func (c *Crypter) SealObject(role, objectID string, plain []byte) ([]byte, error) {
	return c.seal(objectAAD(role, objectID), plain)
}

// OpenObject decrypts a metadata object, verifying its role and ID binding.
func (c *Crypter) OpenObject(role, objectID string, sealed []byte) ([]byte, error) {
	return c.open(objectAAD(role, objectID), sealed)
}
