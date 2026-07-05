package pack

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

const (
	entrySize        = 61 // id(32) + offset(8) + storedLen(8) + rawLen(8) + flags(1) + crc(4)
	plainTrailerSize = 40 // footer_len(4) + sha256(32) + magic(4)
	encTrailerSize   = 20 // footer_offset(8) + footer_stored_len(8) + magic(4)
)

// Entry describes one blob within a pack (docs/architecture/backup-format.md, Pack Files). Offset and
// StoredLen locate the stored (possibly compressed and encrypted) bytes;
// CRC32C covers exactly those stored bytes so integrity can be scanned
// without keys.
type Entry struct {
	ID        BlobID
	Offset    uint64
	StoredLen uint64
	RawLen    uint64
	Flags     BlobFlags
	CRC32C    uint32
}

// encodeFooterRegion serializes entries as count(u32 LE) || entry*count.
func encodeFooterRegion(entries []Entry) []byte {
	buf := make([]byte, 0, 4+len(entries)*entrySize)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(entries))) //nolint:gosec // len(entries) is always valid
	for _, e := range entries {
		buf = append(buf, e.ID[:]...)
		buf = binary.LittleEndian.AppendUint64(buf, e.Offset)
		buf = binary.LittleEndian.AppendUint64(buf, e.StoredLen)
		buf = binary.LittleEndian.AppendUint64(buf, e.RawLen)
		buf = append(buf, byte(e.Flags))
		buf = binary.LittleEndian.AppendUint32(buf, e.CRC32C)
	}
	return buf
}

// parseFooterRegion decodes and validates a footer region. footerStart is the
// file offset where the footer begins; every entry must lie fully inside
// [headerSize, footerStart).
func parseFooterRegion(region []byte, footerStart uint64) ([]Entry, error) {
	if len(region) < 4 {
		return nil, fmt.Errorf("%w: footer region shorter than count field", ErrCorrupt)
	}
	count := binary.LittleEndian.Uint32(region[:4])
	if uint64(len(region)) != 4+uint64(count)*entrySize {
		return nil, fmt.Errorf("%w: footer region is %d bytes, want %d for %d entries",
			ErrCorrupt, len(region), 4+uint64(count)*entrySize, count)
	}
	entries := make([]Entry, count)
	for i := range entries {
		off := 4 + i*entrySize
		e := &entries[i]
		copy(e.ID[:], region[off:off+32])
		e.Offset = binary.LittleEndian.Uint64(region[off+32:])
		e.StoredLen = binary.LittleEndian.Uint64(region[off+40:])
		e.RawLen = binary.LittleEndian.Uint64(region[off+48:])
		e.Flags = BlobFlags(region[off+56])
		e.CRC32C = binary.LittleEndian.Uint32(region[off+57:])
		if e.StoredLen > maxStoredLen {
			return nil, fmt.Errorf(
				"%w: entry %d stored length %d exceeds max %d",
				ErrCorrupt, i, e.StoredLen, uint64(maxStoredLen))
		}
		end := e.Offset + e.StoredLen
		if e.Offset < headerSize || end < e.Offset || end > footerStart {
			return nil, fmt.Errorf("%w: entry %d spans [%d,%d) outside data region [%d,%d)",
				ErrCorrupt, i, e.Offset, end, headerSize, footerStart)
		}
	}
	return entries, nil
}

// appendPlainTrailer appends the plain-pack trailer to a footer region:
// footer_len(u32 LE) || sha256(region || footer_len) || "KPVM".
func appendPlainTrailer(region []byte) []byte {
	out := make([]byte, 0, len(region)+plainTrailerSize)
	out = append(out, region...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(region))) //nolint:gosec // len(region) is always valid
	sum := sha256.Sum256(out)
	out = append(out, sum[:]...)
	return append(out, trailerMagic...)
}

// parsePlainTrailer parses the plain-pack trailer (docs/architecture/backup-format.md, Pack Files) out of
// its fixed-size tail and validates footer_len against maxFooterLen and
// fileSize. It returns the footer length and the trailer's stored checksum
// without requiring the footer region bytes themselves to be present in
// trailer, so a caller can learn how much of the file to read next before
// reading it.
func parsePlainTrailer(trailer []byte, fileSize uint64) (footerLen uint32,
	checksum [32]byte, err error) {
	if len(trailer) < plainTrailerSize {
		return 0, checksum, fmt.Errorf("%w: %d bytes is too small for a pack",
			ErrTruncated, len(trailer))
	}
	trailer = trailer[len(trailer)-plainTrailerSize:]
	if !bytes.Equal(trailer[36:40], []byte(trailerMagic)) {
		return 0, checksum, fmt.Errorf("%w: trailer", ErrBadMagic)
	}
	footerLen = binary.LittleEndian.Uint32(trailer[:4])
	if uint64(footerLen) > maxFooterLen ||
		fileSize < uint64(headerSize)+uint64(footerLen)+plainTrailerSize {
		return 0, checksum, fmt.Errorf("%w: footer length %d exceeds file",
			ErrTruncated, footerLen)
	}
	copy(checksum[:], trailer[4:36])
	return footerLen, checksum, nil
}

// extractPlainFooterRegionShifted is extractPlainFooterRegion for a tail
// window that starts at file offset shift rather than zero. tail must
// contain the footer region as well as the trailer.
func extractPlainFooterRegionShifted(tail []byte, shift uint64) ([]byte, error) {
	fileSize := shift + uint64(len(tail))
	footerLen, checksum, err := parsePlainTrailer(tail, fileSize)
	if err != nil {
		return nil, err
	}
	regionStart := fileSize - plainTrailerSize - uint64(footerLen)
	if regionStart < shift {
		return nil, fmt.Errorf("%w: footer region starts before tail window", ErrTruncated)
	}
	localStart := regionStart - shift
	checked := tail[localStart : uint64(len(tail))-plainTrailerSize+4]
	sum := sha256.Sum256(checked)
	if !bytes.Equal(sum[:], checksum[:]) {
		return nil, ErrChecksum
	}
	return tail[localStart : localStart+uint64(footerLen)], nil
}

// extractPlainFooterRegion validates the plain trailer at the end of file and
// returns the footer region bytes.
func extractPlainFooterRegion(file []byte) ([]byte, error) {
	return extractPlainFooterRegionShifted(file, 0)
}

// appendEncryptedTrailer appends the encrypted-pack trailer: the sealed footer
// followed by footer_offset(u64 LE) || footer_stored_len(u64 LE) || "KPVM"
// (docs/architecture/backup-format.md, Pack Files). The trailer is plaintext by necessity; tampering is
// caught because the footer's AEAD open fails.
func appendEncryptedTrailer(sealedFooter []byte,
	footerOffset uint64) []byte {
	out := make([]byte, 0, len(sealedFooter)+encTrailerSize)
	out = append(out, sealedFooter...)
	out = binary.LittleEndian.AppendUint64(out, footerOffset)
	out = binary.LittleEndian.AppendUint64(out, uint64(len(sealedFooter)))
	return append(out, trailerMagic...)
}

// parseEncryptedTrailer parses the encrypted-pack trailer (docs/architecture/backup-format.md, Pack Files)
// out of its fixed-size tail and validates footer_offset/footer_stored_len
// against fileSize. It returns the sealed footer's file offset and length
// without requiring the footer bytes themselves to be present in trailer, so
// a caller can learn how much of the file to read next before reading it.
func parseEncryptedTrailer(trailer []byte, fileSize uint64) (footerOffset,
	storedLen uint64, err error) {
	if len(trailer) < encTrailerSize {
		return 0, 0, ErrTruncated
	}
	trailer = trailer[len(trailer)-encTrailerSize:]
	if !bytes.Equal(trailer[16:20], []byte(trailerMagic)) {
		return 0, 0, fmt.Errorf("%w: trailer", ErrBadMagic)
	}
	footerOffset = binary.LittleEndian.Uint64(trailer[:8])
	storedLen = binary.LittleEndian.Uint64(trailer[8:16])
	end := footerOffset + storedLen
	if storedLen > maxFooterLen || footerOffset < headerSize ||
		end < footerOffset || end != fileSize-encTrailerSize {
		return 0, 0,
			fmt.Errorf("%w: footer span [%d,%d) inconsistent with "+
				"file size %d", ErrTruncated, footerOffset, end,
				fileSize)
	}
	return footerOffset, storedLen, nil
}

// extractEncryptedFooterShifted is extractEncryptedFooter for a tail window
// that starts at file offset shift rather than zero. tail must contain the
// sealed footer bytes as well as the trailer.
func extractEncryptedFooterShifted(tail []byte, shift uint64) ([]byte,
	uint64, error) {
	fileSize := shift + uint64(len(tail))
	footerOffset, storedLen, err := parseEncryptedTrailer(tail, fileSize)
	if err != nil {
		return nil, 0, err
	}
	if footerOffset < shift {
		return nil, 0, fmt.Errorf("%w: footer region starts before tail window",
			ErrTruncated)
	}
	end := footerOffset + storedLen
	return tail[footerOffset-shift : end-shift], footerOffset, nil
}

// extractEncryptedFooter parses the encrypted-pack trailer and returns the
// sealed footer bytes and the file offset where the footer begins.
func extractEncryptedFooter(file []byte) ([]byte, uint64, error) {
	return extractEncryptedFooterShifted(file, 0)
}
