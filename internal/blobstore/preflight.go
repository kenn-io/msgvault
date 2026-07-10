package blobstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
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
		return nil, fmt.Errorf("%w: pack container is %d bytes, limit %d",
			ErrBlobTooLarge, size, MaxMaintenancePackBytes)
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
		return nil, fmt.Errorf("%w: pack footer is %d bytes, limit %d",
			ErrBlobTooLarge, footerLen, MaxMaintenanceFooterBytes)
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
		return nil, fmt.Errorf("%w: pack footer has %d entries, limit %d",
			ErrBlobTooLarge, count, MaxMaintenancePackEntries)
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
		return nil, fmt.Errorf("%w: raw length is %d bytes, limit %d",
			ErrBlobTooLarge, entry.RawLen, maxBytes)
	}
	if entry.StoredLen > limit {
		return nil, fmt.Errorf("%w: stored length is %d bytes, limit %d",
			ErrBlobTooLarge, entry.StoredLen, maxBytes)
	}
	if entry.Flags & ^pack.BlobCompressed != 0 {
		return nil, fmt.Errorf("%w: unsupported blob flags %#x", pack.ErrCorrupt, entry.Flags)
	}
	maxInt := uint64(^uint(0) >> 1)
	if entry.RawLen > maxInt || entry.StoredLen > maxInt {
		return nil, fmt.Errorf("%w: blob lengths exceed addressable memory", ErrBlobTooLarge)
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
