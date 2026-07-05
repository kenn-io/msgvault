package pack

import (
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
)

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// WriterOptions configures a pack Writer. Zero values select the defaults.
type WriterOptions struct {
	// TargetSize is the advisory size at which Full() reports true.
	TargetSize int64
	// ZstdLevel is the trial-compression level (default 3).
	ZstdLevel int
	// Crypter, when non-nil, produces an encrypted pack.
	Crypter *Crypter
}

// Writer builds one pack file in a staging directory and moves it to its
// final path on Seal, following the durability discipline (docs/architecture/backup-format.md, Crash Consistency):
// write -> fsync file -> rename -> fsync directory.
type Writer struct {
	id      string
	f       *os.File
	staging string
	opts    WriterOptions
	off     int64
	entries []Entry
	done    bool
	err     error
}

// NewWriter creates the staging file and writes the pack header.
func NewWriter(stagingDir string, opts WriterOptions) (*Writer, error) {
	if opts.TargetSize <= 0 {
		opts.TargetSize = DefaultTargetSize
	}
	id := NewPackID()
	staging := filepath.Join(stagingDir, id+".mvpack.staging")
	f, err := os.OpenFile(staging, os.O_CREATE|os.O_EXCL|os.O_WRONLY,
		0o600)
	if err != nil {
		return nil, fmt.Errorf("pack: creating staging file: %w", err)
	}
	var flags packFlags
	if opts.Crypter != nil {
		flags |= packEncrypted
	}
	header := append([]byte(headerMagic), FormatVersion, byte(flags))
	if _, err := f.Write(header); err != nil {
		_ = f.Close()
		_ = os.Remove(staging)
		return nil, fmt.Errorf("pack: writing header: %w", err)
	}
	return &Writer{id: id, f: f, staging: staging, opts: opts, off: headerSize},
		nil
}

// ID returns the pack's ULID.
func (w *Writer) ID() string { return w.id }

// StoredSize returns the bytes written so far (header plus stored frames).
func (w *Writer) StoredSize() int64 { return w.off }

// Full reports whether the pack has reached its advisory target size.
func (w *Writer) Full() bool { return w.off >= w.opts.TargetSize }

// Append stores one blob and returns its entry. Dedup is the caller's job.
// raw must be at most maxRawLen bytes: a compressed frame recording a larger
// raw length could never be read back, since decodeFrame rejects it to match
// the zstd decoder's memory limit, so Append rejects the input up front.
//
// Once a write to the staging file fails, the Writer is poisoned: the
// underlying file descriptor's position may have advanced past w.off (for
// example on a partial write), so every following Append or Seal would record
// or publish offsets that no longer match the file. Append and Seal both
// return the first such error immediately without writing again; Abort still
// works to discard the poisoned staging file.
func (w *Writer) Append(raw []byte) (Entry, error) {
	if w.done {
		return Entry{}, ErrSealed
	}
	if w.err != nil {
		return Entry{}, w.err
	}
	if uint64(len(raw)) > maxRawLen {
		return Entry{}, fmt.Errorf("pack: raw blob length %d exceeds max %d bytes",
			len(raw), uint64(maxRawLen))
	}
	id := ComputeBlobID(raw)
	stored, compressed := encodeFrame(raw, w.opts.ZstdLevel)
	return w.appendFrame(id, stored, uint64(len(raw)), compressed)
}

// AppendEncoded stores one blob whose frame the caller already produced with
// EncodeFrame, so trial compression can run concurrently outside the Writer.
// The caller contract is strict: id must equal ComputeBlobID of the original
// raw bytes, and (frame, compressed) must be EncodeFrame's output for those
// same bytes — a violated contract writes a frame readers will reject at
// decode time. The cheap invariants (raw length bound, an uncompressed
// frame's length matching rawLen) are still checked here.
func (w *Writer) AppendEncoded(id BlobID, frame []byte, rawLen uint64, compressed bool) (Entry, error) {
	if w.done {
		return Entry{}, ErrSealed
	}
	if w.err != nil {
		return Entry{}, w.err
	}
	if rawLen > maxRawLen {
		return Entry{}, fmt.Errorf("pack: raw blob length %d exceeds max %d bytes",
			rawLen, uint64(maxRawLen))
	}
	if !compressed && uint64(len(frame)) != rawLen {
		return Entry{}, fmt.Errorf("pack: uncompressed frame for blob %s is %d bytes but claims raw length %d",
			id, len(frame), rawLen)
	}
	return w.appendFrame(id, frame, rawLen, compressed)
}

// appendFrame seals (when encrypted), writes, and records one encoded frame.
func (w *Writer) appendFrame(id BlobID, stored []byte, rawLen uint64, compressed bool) (Entry, error) {
	var flags BlobFlags
	if compressed {
		flags |= BlobCompressed
	}
	if w.opts.Crypter != nil {
		sealed, err := w.opts.Crypter.SealBlob(id, stored)
		if err != nil {
			return Entry{}, fmt.Errorf("pack: sealing blob %s: %w", id, err)
		}
		stored = sealed
		flags |= BlobEncrypted
	}
	if _, err := w.f.Write(stored); err != nil {
		w.err = fmt.Errorf("pack: writing blob %s: %w", id, err)
		return Entry{}, w.err
	}
	e := Entry{
		ID:        id,
		Offset:    uint64(w.off), //nolint:gosec // w.off >= 0
		StoredLen: uint64(len(stored)),
		RawLen:    rawLen,
		Flags:     flags,
		CRC32C:    crc32.Checksum(stored, crc32cTable),
	}
	w.off += int64(len(stored))
	w.entries = append(w.entries, e)
	return e, nil
}

// Seal writes the footer and trailer, makes the file durable, and renames it
// to finalPath. The Writer is unusable afterward.
//
// Once the rename to finalPath succeeds, the pack is considered published and
// the Writer is marked done even if the following directory-entry sync fails.
// In that case Seal returns an error, but the pack file already exists at
// finalPath: the failure means only that the directory entry for the rename
// may not be durable yet, not that the pack is missing or incomplete. A
// following Abort call is then a no-op that returns nil; it must not remove
// the published pack.
func (w *Writer) Seal(finalPath string) ([]Entry, error) {
	if w.done {
		return nil, ErrSealed
	}
	if w.err != nil {
		return nil, w.err
	}
	region := encodeFooterRegion(w.entries)
	var tail []byte
	if w.opts.Crypter != nil {
		sealed, err := w.opts.Crypter.SealObject("pack-footer", w.id,
			region)
		if err != nil {
			return nil, fmt.Errorf("pack: sealing footer: %w", err)
		}
		tail = appendEncryptedTrailer(sealed, uint64(w.off)) //nolint:gosec // w.off >= 0
	} else {
		tail = appendPlainTrailer(region)
	}
	if _, err := w.f.Write(tail); err != nil {
		w.err = fmt.Errorf("pack: writing footer: %w", err)
		return nil, w.err
	}
	if err := w.f.Sync(); err != nil {
		return nil, fmt.Errorf("pack: syncing pack file: %w", err)
	}
	if err := w.f.Close(); err != nil {
		return nil, fmt.Errorf("pack: closing pack file: %w", err)
	}
	if err := mkdirAllSynced(filepath.Dir(finalPath)); err != nil {
		return nil, err
	}
	if err := os.Rename(w.staging, finalPath); err != nil {
		return nil, fmt.Errorf("pack: publishing pack: %w", err)
	}
	// The pack is published as of this point: mark the writer done before
	// attempting the directory sync so a failure below doesn't leave the
	// Writer looking unsealed. A caller that calls Abort next must find a
	// no-op, not a removal of the file we just published.
	w.done = true
	if err := SyncDir(filepath.Dir(finalPath)); err != nil {
		return nil, fmt.Errorf(
			"pack: pack published at %s but directory sync failed (entry may not be durable): %w",
			finalPath, err)
	}
	return w.entries, nil
}

// Abort discards the staging file. Safe to call after a failed Seal.
func (w *Writer) Abort() error {
	if w.done {
		return nil
	}
	w.done = true
	_ = w.f.Close()
	if err := os.Remove(w.staging); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("pack: removing staging file: %w", err)
	}
	return nil
}

// SyncDir fsyncs a directory so a rename into it is durable. It is a
// variable so tests can inject failures; on Windows it is a no-op (see
// syncdir_windows.go).
var SyncDir = syncDirPlatform

// mkdirAllSynced creates dir and any missing parent directories one level at
// a time, fsyncing each parent directory immediately after it gains a new
// child. Plain os.MkdirAll leaves every directory-entry write for
// intermediate components undurable except the one the caller fsyncs
// afterward (typically just the leaf); if a fresh packs root and a shard
// directory are both created in one Seal call, a crash right after Seal
// returns could still lose the whole subtree even though the leaf itself was
// synced. Recursing to the deepest existing ancestor first, then fsyncing
// each parent as its new child is created, keeps the entire new path
// durable.
func mkdirAllSynced(dir string) error {
	dir = filepath.Clean(dir)
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("pack: %s exists and is not a directory", dir)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("pack: stat %s: %w", dir, err)
	}
	parent := filepath.Dir(dir)
	if parent == dir {
		return fmt.Errorf("pack: no existing ancestor found while creating %s", dir)
	}
	if err := mkdirAllSynced(parent); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("pack: creating directory %s: %w", dir, err)
	}
	if err := SyncDir(parent); err != nil {
		return fmt.Errorf("pack: syncing directory %s: %w", parent, err)
	}
	return nil
}
