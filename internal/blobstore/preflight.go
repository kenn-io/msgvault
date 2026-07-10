package blobstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

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
	plainPackHeaderSize  = 6
	plainPackTrailerSize = 40
	plainPackEntrySize   = 61
)

// preflightPlainPack validates the allocation-relevant parts of the stable
// plain v1 pack format while reading only its fixed header, fixed trailer,
// and four-byte footer count. It must run before pack.OpenReader, whose v1
// parser allocates storage for the entire footer table.
func preflightPlainPack(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open pack for bounded preflight: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat pack for bounded preflight: %w", err)
	}
	size := info.Size()
	if size > MaxMaintenancePackBytes {
		return fmt.Errorf("%w: pack container is %d bytes, limit %d",
			ErrBlobTooLarge, size, MaxMaintenancePackBytes)
	}
	if size < plainPackHeaderSize+plainPackTrailerSize {
		return fmt.Errorf("%w: %d bytes is too small for a plain pack", pack.ErrTruncated, size)
	}

	var header [plainPackHeaderSize]byte
	if _, err := f.ReadAt(header[:], 0); err != nil {
		return fmt.Errorf("read pack header for bounded preflight: %w", err)
	}
	if !bytes.Equal(header[:4], []byte("MVPK")) {
		return fmt.Errorf("%w: header", pack.ErrBadMagic)
	}
	if header[4] != pack.FormatVersion {
		return fmt.Errorf("%w: version %d", pack.ErrUnsupportedVersion, header[4])
	}
	if header[5] != 0 {
		return fmt.Errorf("bounded reads require a plain v1 pack, flags %#x", header[5])
	}

	var trailer [plainPackTrailerSize]byte
	if _, err := f.ReadAt(trailer[:], size-plainPackTrailerSize); err != nil {
		return fmt.Errorf("read pack trailer for bounded preflight: %w", err)
	}
	if !bytes.Equal(trailer[36:], []byte("KPVM")) {
		return fmt.Errorf("%w: trailer", pack.ErrBadMagic)
	}
	footerLen := uint64(binary.LittleEndian.Uint32(trailer[:4]))
	if footerLen > MaxMaintenanceFooterBytes {
		return fmt.Errorf("%w: pack footer is %d bytes, limit %d",
			ErrBlobTooLarge, footerLen, MaxMaintenanceFooterBytes)
	}
	fileSize := uint64(size)
	if footerLen < 4 || fileSize < plainPackHeaderSize+plainPackTrailerSize+footerLen {
		return fmt.Errorf("%w: footer length %d is outside %d-byte pack",
			pack.ErrTruncated, footerLen, size)
	}
	footerStart := fileSize - plainPackTrailerSize - footerLen
	var countBytes [4]byte
	if _, err := f.ReadAt(countBytes[:], int64(footerStart)); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: read footer count: %w", pack.ErrTruncated, err)
		}
		return fmt.Errorf("read pack footer count for bounded preflight: %w", err)
	}
	count := uint64(binary.LittleEndian.Uint32(countBytes[:]))
	if count > MaxMaintenancePackEntries {
		return fmt.Errorf("%w: pack footer has %d entries, limit %d",
			ErrBlobTooLarge, count, MaxMaintenancePackEntries)
	}
	wantFooterLen := uint64(4) + count*plainPackEntrySize
	if footerLen != wantFooterLen {
		return fmt.Errorf("%w: footer length is %d bytes, want %d for %d entries",
			pack.ErrCorrupt, footerLen, wantFooterLen, count)
	}
	return nil
}
