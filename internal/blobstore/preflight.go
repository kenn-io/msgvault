package blobstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"

	"github.com/klauspost/compress/zstd"
	"go.kenn.io/kit/pack"
)

const (
	// MaxMaintenanceBlobBytes bounds a single attachment buffered by
	// maintenance operations.
	MaxMaintenanceBlobBytes = 64 << 20
	// MaxMaintenancePackEntries bounds the footer table parsed into memory.
	MaxMaintenancePackEntries = 100_000
	// MaxMaintenanceFooterBytes bounds the footer region parsed into memory.
	MaxMaintenanceFooterBytes = 8 << 20
	// MaxMaintenancePackBytes bounds a maintenance pack container.
	MaxMaintenancePackBytes = 128 << 20
)

// ErrBlobTooLarge reports that a bounded attachment or pack exceeds its
// supplied or maintenance safety ceiling.
var ErrBlobTooLarge = errors.New("attachment blob exceeds bounded read limit")

// LimitDimension identifies which bounded-maintenance quantity exceeded its
// ceiling. Callers can report the actual constraint without parsing errors.
type LimitDimension string

const (
	LimitBlobRawBytes       LimitDimension = "blob_raw_bytes"
	LimitBlobStoredBytes    LimitDimension = "blob_stored_bytes"
	LimitBlobStatBytes      LimitDimension = "blob_stat_bytes"
	LimitPackContainerBytes LimitDimension = "pack_container_bytes"
	LimitPackFooterBytes    LimitDimension = "pack_footer_bytes"
	LimitPackEntryCount     LimitDimension = "pack_entry_count"
)

// LimitError preserves ErrBlobTooLarge while carrying a machine-readable
// dimension and the exact values that crossed the boundary.
type LimitError struct {
	Dimension LimitDimension
	Actual    uint64
	Limit     uint64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%s: %s is %d, limit %d", ErrBlobTooLarge, e.Dimension, e.Actual, e.Limit)
}

func (e *LimitError) Unwrap() error { return ErrBlobTooLarge }

func newLimitError(dimension LimitDimension, actual, limit uint64) error {
	return &LimitError{Dimension: dimension, Actual: actual, Limit: limit}
}

const (
	// These constants intentionally duplicate the stable plain v1 wire
	// format instead of following kit's mutable current-version constant.
	// Bounded maintenance reads must fail closed if kit moves to a new layout.
	plainPackVersion     = byte(1)
	plainPackHeaderSize  = 6
	plainPackTrailerSize = 40
	plainPackEntrySize   = 61
)

var boundedCRC32CTable = crc32.MakeTable(crc32.Castagnoli)

// boundedPackReader owns the exact descriptor whose header, trailer, footer,
// and checksum were validated. Keeping footer entries in a map makes each
// bounded lookup constant-time and lets parsing reject ambiguous duplicates.
type boundedPackReader struct {
	file    *os.File
	entries map[pack.BlobID]pack.Entry
}

// MaintenancePackReader exposes the bounded plain-v1 reader to maintenance
// packages without exposing its descriptor or mutable footer map. It keeps the
// exact descriptor used for preflight open until Close.
type MaintenancePackReader struct {
	reader *boundedPackReader
}

// OpenMaintenancePack preflights a pack's container, footer, entry count,
// checksum, and entry spans before returning an opaque retained-FD reader.
func OpenMaintenancePack(path string) (*MaintenancePackReader, error) {
	r, err := openBoundedPack(path)
	if err != nil {
		return nil, err
	}
	return &MaintenancePackReader{reader: r}, nil
}

// Entries returns a copy of the pack's authoritative footer entries.
func (r *MaintenancePackReader) Entries() []pack.Entry {
	entries := make([]pack.Entry, 0, len(r.reader.entries))
	for _, entry := range r.reader.entries {
		entries = append(entries, entry)
	}
	return entries
}

// ReadBlob returns one fully verified entry while enforcing maxBytes against
// both its stored and raw lengths.
func (r *MaintenancePackReader) ReadBlob(hash string, maxBytes int64) ([]byte, error) {
	id, err := pack.ParseBlobID(hash)
	if err != nil {
		return nil, fmt.Errorf("parse maintenance blob hash: %w", err)
	}
	entry, ok := r.reader.entries[id]
	if !ok {
		return nil, fmt.Errorf("%w: blob %s is absent from pack footer", fs.ErrNotExist, hash)
	}
	if maxBytes < 0 {
		return nil, fmt.Errorf("bounded attachment limit must be nonnegative, got %d", maxBytes)
	}
	return r.reader.readBlob(entry, maxBytes)
}

// Close releases the retained preflighted pack descriptor.
func (r *MaintenancePackReader) Close() error { return r.reader.Close() }

// openBoundedPack parses only the stable plain v1 format. This duplicates the
// small wire-format parser because pack.OpenReader reopens a pathname and its
// general parser permits footer/output allocations far above maintenance
// limits. Every later stored-byte ReadAt uses this same preflighted descriptor.
func openBoundedPack(path string) (*boundedPackReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open pack for bounded preflight: %w", err)
	}
	keepOpen := false
	defer func() {
		if !keepOpen {
			_ = f.Close()
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat pack for bounded preflight: %w", err)
	}
	size := info.Size()
	if size > MaxMaintenancePackBytes {
		return nil, newLimitError(LimitPackContainerBytes, uint64(size), MaxMaintenancePackBytes)
	}
	if size < plainPackHeaderSize+plainPackTrailerSize {
		return nil, fmt.Errorf("%w: %d bytes is too small for a plain pack", pack.ErrTruncated, size)
	}

	var header [plainPackHeaderSize]byte
	if err := readBoundedPackAt(f, header[:], 0, "header"); err != nil {
		return nil, err
	}
	if !bytes.Equal(header[:4], []byte("MVPK")) {
		return nil, fmt.Errorf("%w: header", pack.ErrBadMagic)
	}
	if header[4] != plainPackVersion {
		return nil, fmt.Errorf("%w: version %d", pack.ErrUnsupportedVersion, header[4])
	}
	if header[5] != 0 {
		return nil, fmt.Errorf("%w: bounded reads require a plain v1 pack, flags %#x",
			pack.ErrCorrupt, header[5])
	}

	var trailer [plainPackTrailerSize]byte
	if err := readBoundedPackAt(f, trailer[:], size-plainPackTrailerSize, "trailer"); err != nil {
		return nil, err
	}
	if !bytes.Equal(trailer[36:], []byte("KPVM")) {
		return nil, fmt.Errorf("%w: trailer", pack.ErrBadMagic)
	}
	footerLen := uint64(binary.LittleEndian.Uint32(trailer[:4]))
	if footerLen > MaxMaintenanceFooterBytes {
		return nil, newLimitError(LimitPackFooterBytes, footerLen, MaxMaintenanceFooterBytes)
	}
	fileSize := uint64(size)
	if footerLen < 4 || fileSize < plainPackHeaderSize+plainPackTrailerSize+footerLen {
		return nil, fmt.Errorf("%w: footer length %d is outside %d-byte pack",
			pack.ErrTruncated, footerLen, size)
	}
	footerStart := fileSize - plainPackTrailerSize - footerLen
	var countBytes [4]byte
	if err := readBoundedPackAt(f, countBytes[:], int64(footerStart), "footer count"); err != nil {
		return nil, err
	}
	count := uint64(binary.LittleEndian.Uint32(countBytes[:]))
	if count > MaxMaintenancePackEntries {
		return nil, newLimitError(LimitPackEntryCount, count, MaxMaintenancePackEntries)
	}
	wantFooterLen := uint64(4) + count*plainPackEntrySize
	if footerLen != wantFooterLen {
		return nil, fmt.Errorf("%w: footer length is %d bytes, want %d for %d entries",
			pack.ErrCorrupt, footerLen, wantFooterLen, count)
	}

	footer := make([]byte, int(footerLen))
	if err := readBoundedPackAt(f, footer, int64(footerStart), "footer"); err != nil {
		return nil, err
	}
	digest := sha256.New()
	_, _ = digest.Write(footer)
	_, _ = digest.Write(trailer[:4])
	if !bytes.Equal(digest.Sum(nil), trailer[4:36]) {
		return nil, pack.ErrChecksum
	}

	entries := make(map[pack.BlobID]pack.Entry, int(count))
	for i := range int(count) {
		off := 4 + i*plainPackEntrySize
		var entry pack.Entry
		copy(entry.ID[:], footer[off:off+32])
		entry.Offset = binary.LittleEndian.Uint64(footer[off+32:])
		entry.StoredLen = binary.LittleEndian.Uint64(footer[off+40:])
		entry.RawLen = binary.LittleEndian.Uint64(footer[off+48:])
		entry.Flags = pack.BlobFlags(footer[off+56])
		entry.CRC32C = binary.LittleEndian.Uint32(footer[off+57:])
		if entry.RawLen > pack.MaxRawLen {
			return nil, fmt.Errorf("%w: entry %d raw length %d exceeds max %d",
				pack.ErrCorrupt, i, entry.RawLen, uint64(pack.MaxRawLen))
		}
		if entry.Flags & ^pack.BlobCompressed != 0 {
			return nil, fmt.Errorf("%w: entry %d has unsupported blob flags %#x",
				pack.ErrCorrupt, i, entry.Flags)
		}
		end := entry.Offset + entry.StoredLen
		if entry.Offset < plainPackHeaderSize || end < entry.Offset || end > footerStart {
			return nil, fmt.Errorf("%w: entry %d spans [%d,%d) outside data region [%d,%d)",
				pack.ErrCorrupt, i, entry.Offset, end, plainPackHeaderSize, footerStart)
		}
		if _, duplicate := entries[entry.ID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate blob id %s in footer", pack.ErrCorrupt, entry.ID)
		}
		entries[entry.ID] = entry
	}

	keepOpen = true
	return &boundedPackReader{file: f, entries: entries}, nil
}

func readBoundedPackAt(f *os.File, dst []byte, offset int64, part string) error {
	if _, err := f.ReadAt(dst, offset); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: read %s: %w", pack.ErrTruncated, part, err)
		}
		return fmt.Errorf("read pack %s for bounded preflight: %w", part, err)
	}
	return nil
}

// readBlob verifies and decodes one footer entry without allowing output to
// grow beyond its authoritative RawLen. The caller has already matched the
// database row and holds the Store mutex, preventing cache eviction/close.
func (r *boundedPackReader) readBlob(entry pack.Entry, maxBytes int64) ([]byte, error) {
	limit := uint64(maxBytes) //nolint:gosec // ReadBounded rejects negative limits before dispatch
	if entry.RawLen > limit {
		return nil, newLimitError(LimitBlobRawBytes, entry.RawLen, limit)
	}
	if entry.StoredLen > limit {
		return nil, newLimitError(LimitBlobStoredBytes, entry.StoredLen, limit)
	}
	if entry.Flags & ^pack.BlobCompressed != 0 {
		return nil, fmt.Errorf("%w: unsupported blob flags %#x", pack.ErrCorrupt, entry.Flags)
	}
	maxInt := uint64(^uint(0) >> 1)
	if entry.RawLen > maxInt || entry.StoredLen > maxInt {
		if entry.RawLen > maxInt {
			return nil, newLimitError(LimitBlobRawBytes, entry.RawLen, maxInt)
		}
		return nil, newLimitError(LimitBlobStoredBytes, entry.StoredLen, maxInt)
	}

	stored := make([]byte, int(entry.StoredLen))
	if _, err := r.file.ReadAt(stored, int64(entry.Offset)); err != nil { //nolint:gosec // footer span is inside an int64-sized file
		return nil, fmt.Errorf("%w: reading stored bytes for %s: %w", pack.ErrCorrupt, entry.ID, err)
	}
	if crc32.Checksum(stored, boundedCRC32CTable) != entry.CRC32C {
		return nil, fmt.Errorf("%w: crc mismatch for blob %s", pack.ErrCorrupt, entry.ID)
	}

	raw := stored
	if entry.Flags&pack.BlobCompressed != 0 {
		if entry.RawLen == 0 {
			return nil, fmt.Errorf("%w: compressed blob %s has zero raw length", pack.ErrCorrupt, entry.ID)
		}
		decoder, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(entry.RawLen),
			zstd.WithDecoderMaxWindow(max(entry.RawLen, uint64(zstd.MinWindowSize))),
			zstd.WithDecodeAllCapLimit(true))
		if err != nil {
			return nil, fmt.Errorf("%w: initialize bounded zstd decoder: %w", pack.ErrCorrupt, err)
		}
		raw, err = decoder.DecodeAll(stored, make([]byte, 0, int(entry.RawLen)))
		decoder.Close()
		if err != nil {
			return nil, fmt.Errorf("%w: compressed blob %s exceeds declared raw length %d or is invalid: %w",
				pack.ErrCorrupt, entry.ID, entry.RawLen, err)
		}
	}
	if uint64(len(raw)) != entry.RawLen {
		return nil, fmt.Errorf("%w: frame decoded to %d bytes, expected %d",
			pack.ErrCorrupt, len(raw), entry.RawLen)
	}
	if sha256.Sum256(raw) != entry.ID {
		return nil, fmt.Errorf("%w: blob %s", pack.ErrBlobMismatch, entry.ID)
	}
	return raw, nil
}

func (r *boundedPackReader) Close() error { return r.file.Close() }
