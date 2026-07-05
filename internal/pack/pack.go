// Package pack implements the shared blob/pack container format used by the
// msgvault backup repository and the packed live attachment store. See
// docs/architecture/backup-format.md (Pack Files). Packs are
// sealed-immutable: they are written once via Writer and
// never mutated afterward.
package pack

import "errors"

const (
	// FormatVersion is the pack container format version (docs/architecture/backup-format.md, Pack Files).
	FormatVersion = 1

	// DefaultTargetSize is the advisory pack size at which callers seal.
	DefaultTargetSize = 32 << 20

	// DefaultZstdLevel is the default zstd compression level (docs/architecture/backup-format.md, Pack Files).
	DefaultZstdLevel = 3

	headerMagic  = "MVPK"
	trailerMagic = "KPVM"
	headerSize   = 6
	maxFooterLen = 1 << 30

	// maxRawLen bounds the raw (decompressed) length a footer entry may claim.
	// It matches the zstd decoder's WithDecoderMaxMemory(1<<32) limit in
	// frame.go: a compressed frame whose entry claims a larger raw length can
	// never decode successfully, so decodeFrame rejects it before trusting the
	// value to size a preallocation. Append enforces the same bound on raw
	// input so every pack Append writes can produce is guaranteed readable.
	maxRawLen = 1 << 32

	// maxStoredLen bounds StoredLen, the number of bytes a footer entry claims
	// occupy the pack's data region: readStored preallocates a buffer of this
	// size before any integrity check runs, so an untrusted footer entry must
	// not be able to claim an arbitrarily large StoredLen (bounded only by
	// file size otherwise). The legitimate maximum is maxRawLen inflated by
	// the worst-case expansion a frame can add on top of the raw bytes:
	//   - zstd's documented worst case for incompressible input is
	//     input + (input >> 8) + a small fixed number of frame/block header
	//     bytes; maxZstdFrameOverhead is a round, conservative stand-in for
	//     that fixed term.
	//   - an encrypted pack additionally seals the frame, adding a fixed
	//     XChaCha20-Poly1305 overhead (24-byte nonce + 16-byte tag; see
	//     maxSealOverhead in crypter.go).
	// Both allowances can apply to the same entry (compress then seal), so
	// they're additive.
	maxStoredLen = maxRawLen + maxRawLen>>8 + maxZstdFrameOverhead + maxSealOverhead

	// maxZstdFrameOverhead is the conservative fixed-byte allowance in
	// maxStoredLen for zstd frame/block headers, on top of the input+input>>8
	// expansion term.
	maxZstdFrameOverhead = 64
)

// BlobFlags describes how a blob's stored bytes were produced.
type BlobFlags uint8

const (
	// BlobCompressed marks a zstd-compressed frame.
	BlobCompressed BlobFlags = 1 << 0
	// BlobEncrypted marks an AEAD-sealed frame.
	BlobEncrypted BlobFlags = 1 << 1
)

type packFlags uint8

const packEncrypted packFlags = 1 << 0

var (
	ErrBadMagic           = errors.New("pack: bad magic")
	ErrUnsupportedVersion = errors.New("pack: unsupported format version")
	ErrTruncated          = errors.New("pack: truncated file")
	ErrChecksum           = errors.New("pack: footer checksum mismatch")
	ErrCorrupt            = errors.New("pack: corrupt")
	ErrBlobMismatch       = errors.New("pack: blob content hash mismatch")
	ErrEncrypted          = errors.New("pack: encrypted pack requires a crypter")
	ErrDecrypt            = errors.New("pack: decryption failed")
	ErrSealed             = errors.New("pack: writer already sealed")
)
