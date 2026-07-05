package pack

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// BlobID identifies a blob by the SHA-256 of its raw (uncompressed,
// unencrypted) content.
type BlobID [32]byte

// ComputeBlobID returns the BlobID for raw content.
func ComputeBlobID(raw []byte) BlobID {
	return sha256.Sum256(raw)
}

// String returns the lowercase hex form of the ID.
func (id BlobID) String() string {
	return hex.EncodeToString(id[:])
}

// ParseBlobID parses a 64-char lowercase hex blob ID.
func ParseBlobID(s string) (BlobID, error) {
	var id BlobID
	if len(s) != 64 {
		return id, fmt.Errorf("pack: blob id must be 64 hex chars, got %d", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("pack: parsing blob id: %w", err)
	}
	copy(id[:], b)
	return id, nil
}
