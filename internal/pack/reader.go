package pack

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
)

// Reader provides random access to a sealed pack file via pread.
type Reader struct {
	id      string
	f       *os.File
	crypter *Crypter
	enc     bool
	entries []Entry
}

// OpenReader opens a sealed pack. The pack ID is derived from the filename;
// for encrypted packs it participates in the footer's AAD, so a renamed pack
// fails to open. A crypter passed for a plain pack is ignored.
func OpenReader(path string, crypter *Crypter) (*Reader, error) {
	id := strings.TrimSuffix(filepath.Base(path), ".mvpack")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pack: opening %s: %w", path, err)
	}
	r, err := newReader(f, id, crypter)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("pack %s: %w", path, err)
	}
	return r, nil
}

func newReader(f *os.File, id string, crypter *Crypter) (*Reader, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}
	size := info.Size()
	header := make([]byte, headerSize)
	if size < headerSize {
		return nil, ErrTruncated
	}
	if _, err := f.ReadAt(header, 0); err != nil {
		return nil, fmt.Errorf("reading header: %w", err)
	}
	if !bytes.Equal(header[:4], []byte(headerMagic)) {
		return nil, fmt.Errorf("%w: header", ErrBadMagic)
	}
	if header[4] != FormatVersion {
		return nil, fmt.Errorf("%w: version %d", ErrUnsupportedVersion,
			header[4])
	}
	enc := packFlags(header[5])&packEncrypted != 0
	if enc && crypter == nil {
		return nil, ErrEncrypted
	}

	region, footerStart, err := readFooterRegion(f, size, enc, id, crypter)
	if err != nil {
		return nil, err
	}
	entries, err := parseFooterRegion(region, footerStart)
	if err != nil {
		return nil, err
	}
	return &Reader{id: id, f: f, crypter: crypter, enc: enc, entries: entries},
		nil
}

// readFooterRegion locates and reads the footer via two pread calls instead
// of loading a bounded-but-still-huge (up to ~1GiB) tail window: a fixed
// plainTrailerSize-byte read of the very end of the file is always enough to
// hold either trailer form (docs/architecture/backup-format.md, Pack Files) and yields the footer's exact
// offset and length, so the second read fetches only the footer region
// itself. This keeps OpenReader's memory use independent of both pack size
// and footer size for the common case, unlike reading a fixed fraction of
// the file. It returns the decoded (and, for encrypted packs, decrypted)
// footer region and the absolute file offset where that region begins.
func readFooterRegion(f *os.File, size int64, enc bool, id string,
	crypter *Crypter) ([]byte, uint64, error) {
	tailLen := min(size, int64(plainTrailerSize))
	fixedTail := make([]byte, tailLen)
	if _, err := f.ReadAt(fixedTail, size-tailLen); err != nil {
		return nil, 0, fmt.Errorf("reading trailer: %w", err)
	}

	if enc {
		footerOffset, storedLen, err := parseEncryptedTrailer(fixedTail, uint64(size)) //nolint:gosec // size >= 0
		if err != nil {
			return nil, 0, err
		}
		buf := make([]byte, storedLen+encTrailerSize)
		if _, err := f.ReadAt(buf, int64(footerOffset)); err != nil { //nolint:gosec // validated below maxFooterLen
			return nil, 0, fmt.Errorf("reading footer: %w", err)
		}
		sealed, off, err := extractEncryptedFooterShifted(buf, footerOffset)
		if err != nil {
			return nil, 0, err
		}
		region, err := crypter.OpenObject("pack-footer", id, sealed)
		if err != nil {
			return nil, 0, err
		}
		return region, off, nil
	}

	footerLen, _, err := parsePlainTrailer(fixedTail, uint64(size)) //nolint:gosec // size >= 0
	if err != nil {
		return nil, 0, err
	}
	regionStart := uint64(size) - plainTrailerSize - uint64(footerLen) //nolint:gosec // size >= 0
	buf := make([]byte, uint64(footerLen)+plainTrailerSize)
	if _, err := f.ReadAt(buf, int64(regionStart)); err != nil { //nolint:gosec // validated below maxFooterLen
		return nil, 0, fmt.Errorf("reading footer: %w", err)
	}
	region, err := extractPlainFooterRegionShifted(buf, regionStart)
	if err != nil {
		return nil, 0, err
	}
	return region, regionStart, nil
}

// ID returns the pack ULID derived from the filename.
func (r *Reader) ID() string { return r.id }

// Entries returns the footer entries in pack order. Callers must not mutate.
func (r *Reader) Entries() []Entry { return r.entries }

func (r *Reader) readStored(e Entry) ([]byte, error) {
	buf := make([]byte, e.StoredLen)
	//nolint:gosec // e.Offset < footerStart <= file size (int64), per parseFooterRegion
	if _, err := r.f.ReadAt(buf, int64(e.Offset)); err != nil {
		return nil,
			fmt.Errorf("%w: reading stored bytes for %s: %w", ErrCorrupt,
				e.ID, err)
	}
	if crc32.Checksum(buf, crc32cTable) != e.CRC32C {
		return nil, fmt.Errorf("%w: crc mismatch for blob %s", ErrCorrupt,
			e.ID)
	}
	return buf, nil
}

// VerifyStored checks the stored bytes' CRC without decrypting or decoding.
func (r *Reader) VerifyStored(e Entry) error {
	_, err := r.readStored(e)
	return err
}

// ReadBlob returns the raw content of e, verifying CRC, AEAD, and blob hash.
func (r *Reader) ReadBlob(e Entry) ([]byte, error) {
	stored, err := r.readStored(e)
	if err != nil {
		return nil, err
	}
	if e.Flags&BlobEncrypted != 0 {
		if !r.enc {
			return nil, fmt.Errorf("%w: blob %s flagged encrypted in a plain pack",
				ErrCorrupt, e.ID)
		}
		if r.crypter == nil {
			return nil, ErrEncrypted
		}
		if stored, err = r.crypter.OpenBlob(e.ID, stored); err != nil {
			return nil, err
		}
	}
	raw, err := decodeFrame(stored, e.Flags&BlobCompressed != 0, e.RawLen)
	if err != nil {
		return nil, err
	}
	if ComputeBlobID(raw) != e.ID {
		return nil, fmt.Errorf("%w: blob %s", ErrBlobMismatch, e.ID)
	}
	return raw, nil
}

// Close releases the underlying file.
func (r *Reader) Close() error { return r.f.Close() }
