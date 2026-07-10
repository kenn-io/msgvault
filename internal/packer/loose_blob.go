package packer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/store"
)

var (
	// These variables keep production's fixed limits while letting focused
	// package tests exercise boundaries without allocating 64 MiB per case.
	maintenanceBlobBytes   = int64(blobstore.MaxMaintenanceBlobBytes)
	maintenancePackEntries = blobstore.MaxMaintenancePackEntries
)

var errLooseHashMismatch = errors.New("loose attachment bytes do not match hash")

type canonicalizeStoreError struct{ err error }

func (e *canonicalizeStoreError) Error() string { return e.err.Error() }
func (e *canonicalizeStoreError) Unwrap() error { return e.err }

const looseCopyBufferBytes = 128 << 10

func canonicalLooseRel(hash string) string { return hash[:2] + "/" + hash }

func canonicalLoosePath(attachmentsDir, hash string) string {
	return filepath.Join(attachmentsDir, filepath.FromSlash(canonicalLooseRel(hash)))
}

func isCanonicalLoosePath(attachmentsDir, hash, path string) bool {
	return filepath.Clean(path) == filepath.Clean(canonicalLoosePath(attachmentsDir, hash))
}

// readVerifiedLoose buffers exactly the descriptor's stat-reported size,
// bounded before allocation, and probes one more byte to detect growth.
func readVerifiedLoose(path, hash string, limit int64) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close() //nolint:errcheck // read error is authoritative
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := info.Size()
	if size < 0 || size > limit {
		return nil, size, fmt.Errorf("%w: loose blob is %d bytes, limit %d",
			blobstore.ErrBlobTooLarge, size, limit)
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, size, fmt.Errorf("read exact loose blob: %w", err)
	}
	var probe [1]byte
	n, probeErr := f.Read(probe[:])
	if n != 0 || probeErr == nil {
		return nil, size + int64(n), fmt.Errorf("%w: loose blob grew beyond stat size %d",
			blobstore.ErrBlobTooLarge, size)
	}
	if !errors.Is(probeErr, io.EOF) {
		return nil, size, fmt.Errorf("probe loose blob growth: %w", probeErr)
	}
	if maintenanceHashBytes(data) != hash {
		return nil, size, errLooseHashMismatch
	}
	return data, size, nil
}

func maintenanceHashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// verifyLooseStream hashes a file through a fixed-size buffer. The before and
// after descriptor stats ensure a concurrently changed source is never
// accepted as a canonical recovery copy.
func verifyLooseStream(ctx context.Context, path, hash string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read error is authoritative
	before, err := f.Stat()
	if err != nil {
		return err
	}
	digest := sha256.New()
	buf := make([]byte, looseCopyBufferBytes)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			_, _ = digest.Write(buf[:n])
			total += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	after, err := f.Stat()
	if err != nil {
		return err
	}
	if total != before.Size() || after.Size() != before.Size() ||
		!after.ModTime().Equal(before.ModTime()) {
		return errors.New("loose attachment changed while hashing")
	}
	if hex.EncodeToString(digest.Sum(nil)) != hash {
		return errLooseHashMismatch
	}
	return nil
}

// materializeCanonicalLoose makes a streaming-verified canonical copy without
// replacing any existing file. It does not update the database or remove the
// source; callers preserve that ordering around the transaction boundary.
func materializeCanonicalLoose(ctx context.Context, attachmentsDir, hash, source string) (string, error) {
	canonical := canonicalLoosePath(attachmentsDir, hash)
	if isCanonicalLoosePath(attachmentsDir, hash, source) {
		err := verifyLooseStream(ctx, source, hash)
		return canonical, err
	}
	if err := verifyLooseStream(ctx, canonical, hash); err == nil {
		return canonical, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("verify existing canonical loose blob: %w", err)
	}

	parent := filepath.Dir(canonical)
	if err := pack.MkdirAllSynced(parent); err != nil {
		return "", fmt.Errorf("create synced canonical loose directory: %w", err)
	}
	staging, err := os.CreateTemp(parent, "."+hash+".*.staging")
	if err != nil {
		return "", fmt.Errorf("create canonical loose staging file: %w", err)
	}
	stagingPath := staging.Name()
	published := false
	defer func() {
		_ = staging.Close()
		if !published {
			_ = os.Remove(stagingPath)
		}
	}()

	sourceFile, err := os.Open(source)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	writer := io.MultiWriter(staging, digest)
	buf := make([]byte, looseCopyBufferBytes)
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			_ = sourceFile.Close()
			return "", err
		}
		n, readErr := sourceFile.Read(buf)
		if n > 0 {
			written, writeErr := writer.Write(buf[:n])
			copied += int64(written)
			if writeErr != nil {
				_ = sourceFile.Close()
				return "", writeErr
			}
			if written != n {
				_ = sourceFile.Close()
				return "", io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = sourceFile.Close()
			return "", readErr
		}
	}
	beforeClose, err := sourceFile.Stat()
	if err != nil {
		_ = sourceFile.Close()
		return "", err
	}
	if err := sourceFile.Close(); err != nil {
		return "", err
	}
	if copied != beforeClose.Size() {
		return "", errors.New("loose attachment changed while copying")
	}
	if hex.EncodeToString(digest.Sum(nil)) != hash {
		return "", errLooseHashMismatch
	}
	if err := staging.Sync(); err != nil {
		return "", fmt.Errorf("sync canonical loose staging file: %w", err)
	}
	if err := staging.Close(); err != nil {
		return "", fmt.Errorf("close canonical loose staging file: %w", err)
	}
	if err := publishLooseNoClobber(stagingPath, canonical); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if verifyErr := verifyLooseStream(ctx, canonical, hash); verifyErr == nil {
				return canonical, nil
			}
		}
		return "", err
	}
	published = true
	if err := pack.SyncDir(parent); err != nil {
		return "", fmt.Errorf("sync canonical loose directory: %w", err)
	}
	return canonical, nil
}

func publishLooseNoClobber(staging, canonical string) error {
	if err := os.Link(staging, canonical); err == nil {
		if err := os.Remove(staging); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	} else if errors.Is(err, fs.ErrExist) {
		return err
	}
	if _, err := os.Lstat(canonical); err == nil {
		return fs.ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(staging, canonical)
}

// canonicalizeLooseSource publishes verified canonical bytes, commits the DB
// path update, and only then attempts best-effort legacy deletion.
func canonicalizeLooseSource(ctx context.Context, st *store.Store, attachmentsDir, hash, source string) error {
	canonical, err := materializeCanonicalLoose(ctx, attachmentsDir, hash, source)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := st.CanonicalizeAttachmentBlobPaths(hash); err != nil {
		return &canonicalizeStoreError{err: fmt.Errorf(
			"canonicalize attachment blob paths for %s: %w", hash, err)}
	}
	if filepath.Clean(source) != filepath.Clean(canonical) {
		if err := removeLooseFile(source); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("remove canonicalized legacy loose blob", "hash", hash, "path", source, "error", err)
		}
	}
	return nil
}
