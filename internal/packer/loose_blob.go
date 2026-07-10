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
	"strings"

	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/blobstore"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

var (
	// These variables keep production's fixed limits while letting focused
	// package tests exercise boundaries without allocating 64 MiB per case.
	maintenanceBlobBytes   = int64(blobstore.MaxMaintenanceBlobBytes)
	maintenancePackEntries = blobstore.MaxMaintenancePackEntries
	// openLooseFile is a narrow test seam; production always uses os.Open.
	openLooseFile = os.Open
	// linkLooseFile lets tests force unsupported atomic no-replace publication
	// and destination races. Production always uses os.Link.
	linkLooseFile = os.Link
	// sameLooseFile lets portable tests model case-insensitive path aliases.
	// Production always uses os.SameFile.
	sameLooseFile = os.SameFile
)

var errLooseHashMismatch = errors.New("loose attachment bytes do not match hash")

type canonicalizeStoreError struct{ err error }

func (e *canonicalizeStoreError) Error() string { return e.err.Error() }
func (e *canonicalizeStoreError) Unwrap() error { return e.err }

const looseCopyBufferBytes = 128 << 10

type normalizedBlobHash string

func normalizeBlobHash(raw string) (normalizedBlobHash, error) {
	normalized := strings.ToLower(raw)
	if err := export.ValidateContentHash(normalized); err != nil {
		return "", err
	}
	return normalizedBlobHash(normalized), nil
}

func (h normalizedBlobHash) String() string { return string(h) }

func canonicalLooseRel(hash normalizedBlobHash) string {
	value := hash.String()
	return value[:2] + "/" + value
}

func canonicalLoosePath(attachmentsDir string, hash normalizedBlobHash) string {
	return filepath.Join(attachmentsDir, filepath.FromSlash(canonicalLooseRel(hash)))
}

func isCanonicalLoosePath(attachmentsDir string, hash normalizedBlobHash, path string) bool {
	return filepath.Clean(path) == filepath.Clean(canonicalLoosePath(attachmentsDir, hash))
}

// readVerifiedLoose buffers exactly the descriptor's stat-reported size,
// bounded before allocation, and probes one more byte to detect growth.
func readVerifiedLoose(path string, hash normalizedBlobHash, limit int64) ([]byte, int64, error) {
	f, err := openLooseFile(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close() //nolint:errcheck // read error is authoritative
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := info.Size()
	if size < 0 {
		return nil, size, fmt.Errorf("negative loose blob size %d", size)
	}
	if size > limit {
		return nil, size, &blobstore.LimitError{
			Dimension: blobstore.LimitBlobRawBytes,
			Actual:    uint64(size),
			Limit:     uint64(limit), //nolint:gosec // positive fixed production/test limit
		}
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, size, fmt.Errorf("read exact loose blob: %w", err)
	}
	var probe [1]byte
	n, probeErr := f.Read(probe[:])
	if n != 0 || probeErr == nil {
		return nil, size + int64(n), &blobstore.LimitError{
			Dimension: blobstore.LimitBlobStatBytes,
			Actual:    uint64(size) + uint64(n), //nolint:gosec // nonnegative descriptor stat
			Limit:     uint64(size),
		}
	}
	if !errors.Is(probeErr, io.EOF) {
		return nil, size, fmt.Errorf("probe loose blob growth: %w", probeErr)
	}
	if maintenanceHashBytes(data) != hash.String() {
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
func verifyLooseStream(ctx context.Context, path string, hash normalizedBlobHash) error {
	lstatInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := validateCanonicalLooseInfo(path, lstatInfo); err != nil {
		return err
	}
	f, err := openLooseFile(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read error is authoritative
	before, err := f.Stat()
	if err != nil {
		return err
	}
	if !sameLooseFile(lstatInfo, before) {
		return fmt.Errorf("canonical loose blob changed identity before open: %s", path)
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
	if hex.EncodeToString(digest.Sum(nil)) != hash.String() {
		return errLooseHashMismatch
	}
	return nil
}

func validateCanonicalLooseObject(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return validateCanonicalLooseInfo(path, info)
}

func validateCanonicalLooseInfo(path string, info fs.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("canonical loose blob is not an independent regular file: %s", path)
	}
	return nil
}

// materializeCanonicalLoose makes a streaming-verified canonical copy without
// replacing any existing file. It does not update the database or remove the
// source; callers preserve that ordering around the transaction boundary.
func materializeCanonicalLoose(ctx context.Context, attachmentsDir string, hash normalizedBlobHash, source string) (string, error) {
	canonical := canonicalLoosePath(attachmentsDir, hash)
	if isCanonicalLoosePath(attachmentsDir, hash, source) {
		if err := verifyLooseStream(ctx, source, hash); err != nil {
			return "", err
		}
		if err := pack.SyncDir(filepath.Dir(canonical)); err != nil {
			return "", fmt.Errorf("sync existing canonical loose directory: %w", err)
		}
		return canonical, nil
	}
	if err := verifyLooseStream(ctx, canonical, hash); err == nil {
		if err := pack.SyncDir(filepath.Dir(canonical)); err != nil {
			return "", fmt.Errorf("sync existing canonical loose directory: %w", err)
		}
		return canonical, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("verify existing canonical loose blob: %w", err)
	}

	parent := filepath.Dir(canonical)
	if err := pack.MkdirAllSynced(parent); err != nil {
		return "", fmt.Errorf("create synced canonical loose directory: %w", err)
	}
	staging, err := os.CreateTemp(parent, "."+hash.String()+".*.staging")
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

	sourceFile, err := openLooseFile(source)
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
	if hex.EncodeToString(digest.Sum(nil)) != hash.String() {
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
				if syncErr := pack.SyncDir(filepath.Dir(canonical)); syncErr != nil {
					return "", fmt.Errorf("sync winning canonical loose directory: %w", syncErr)
				}
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
	err := linkLooseFile(staging, canonical)
	if err == nil {
		if err := os.Remove(staging); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if errors.Is(err, fs.ErrExist) {
		return err
	}
	return fmt.Errorf("atomic no-replace canonical loose publish: %w", err)
}

// canonicalizeLooseSource publishes verified canonical bytes, commits every
// case-equivalent DB path update, and only then attempts best-effort legacy
// deletion.
func canonicalizeLooseSource(ctx context.Context, st *store.Store, attachmentsDir string, originalHashes []string, hash normalizedBlobHash, source string) error {
	canonical, err := materializeCanonicalLoose(ctx, attachmentsDir, hash, source)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := st.CanonicalizeAttachmentBlobAliases(hash.String(), originalHashes); err != nil {
		return &canonicalizeStoreError{err: fmt.Errorf(
			"canonicalize attachment blob paths for %s: %w", hash.String(), err)}
	}
	if _, err := removeIndependentLoose(source, canonical); err != nil {
		slog.Warn("preserve canonicalized legacy loose blob after identity check",
			"hash", hash.String(), "original_hashes", originalHashes, "path", source, "error", err)
	}
	return nil
}

func removeIndependentLoose(source, canonical string) (bool, error) {
	if filepath.Clean(source) == filepath.Clean(canonical) {
		return false, nil
	}
	sourceInfo, err := os.Stat(source)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat legacy loose source before removal: %w", err)
	}
	canonicalInfo, err := os.Stat(canonical)
	if err != nil {
		return false, fmt.Errorf("stat canonical loose blob before legacy removal: %w", err)
	}
	if sameLooseFile(sourceInfo, canonicalInfo) {
		return false, nil
	}
	if err := removeLooseFile(source); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}
