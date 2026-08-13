package mistral

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const (
	spoolFilenamePrefix    = ".mistral-ocr-"
	spoolReservationSuffix = ".mistral-ocr-reservations.lock"
	spoolLockRetryInterval = 50 * time.Millisecond
)

// ErrSpoolCapacity marks a retryable quota or free-space reservation failure.
var ErrSpoolCapacity = errors.New("mistral OCR spool capacity unavailable")

type SpoolOptions struct {
	Directory      string
	MediaType      string
	ExpectedSize   int64
	ExpectedSHA256 string
	MaxBytes       int64
	MaxSpoolBytes  int64
	MinFreeBytes   int64
}

// ScavengeSpoolDirectory removes only stale regular files created by this
// package. Unexpected file types fail closed; unrelated regular files remain
// untouched and still count against the aggregate quota.
func ScavengeSpoolDirectory(directory string, staleBefore time.Time) (int, error) {
	if directory == "" || staleBefore.IsZero() {
		return 0, errors.New("mistral OCR spool scavenging requires a directory and cutoff")
	}
	release, err := acquireSpoolReservationLock(context.Background(), directory)
	if err != nil {
		return 0, err
	}
	defer release()
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, fmt.Errorf("read Mistral OCR spool directory: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return removed, fmt.Errorf("inspect Mistral OCR spool entry: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return removed, errors.New("mistral OCR spool directory contains an unsafe entry")
		}
		if !strings.HasPrefix(entry.Name(), spoolFilenamePrefix) || !info.ModTime().Before(staleBefore) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return removed, fmt.Errorf("remove stale Mistral OCR spool %s: %w", filepath.Base(path), err)
		}
		removed++
	}
	return removed, nil
}

// SpoolVerifiedSource copies one authoritative CAS stream into a private
// request spool, consumes and closes the source, validates bytes/hash/type,
// and returns a cleanup function that removes only the created file.
func SpoolVerifiedSource(
	ctx context.Context,
	source io.ReadCloser,
	options SpoolOptions,
) (document Document, cleanup func() error, err error) {
	if source == nil {
		return Document{}, nil, errors.New("mistral OCR spool requires a source")
	}
	if options.Directory == "" || options.ExpectedSize < 0 || options.MaxBytes <= 0 ||
		options.ExpectedSize > options.MaxBytes || options.MaxSpoolBytes < options.MaxBytes || options.MinFreeBytes <= 0 {
		_ = source.Close()
		return Document{}, nil, errors.New("mistral OCR spool has invalid bounds")
	}
	if len(options.ExpectedSHA256) != sha256.Size*2 || options.ExpectedSHA256 != strings.ToLower(options.ExpectedSHA256) {
		_ = source.Close()
		return Document{}, nil, errors.New("mistral OCR spool requires a lowercase SHA-256")
	}
	if _, decodeErr := hex.DecodeString(options.ExpectedSHA256); decodeErr != nil {
		_ = source.Close()
		return Document{}, nil, errors.New("mistral OCR spool requires a lowercase SHA-256")
	}
	info, statErr := os.Lstat(options.Directory)
	if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		_ = source.Close()
		return Document{}, nil, errors.New("mistral OCR spool directory must already exist")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		_ = source.Close()
		return Document{}, nil, errors.New("mistral OCR spool directory permissions must be private")
	}
	release, lockErr := acquireSpoolReservationLock(ctx, options.Directory)
	if lockErr != nil {
		_ = source.Close()
		return Document{}, nil, lockErr
	}
	defer release()
	if capacityErr := checkSpoolCapacity(options); capacityErr != nil {
		_ = source.Close()
		return Document{}, nil, fmt.Errorf("%w: %w", ErrSpoolCapacity, capacityErr)
	}

	file, createErr := os.CreateTemp(options.Directory, spoolFilenamePrefix+"*")
	if createErr != nil {
		_ = source.Close()
		return Document{}, nil, fmt.Errorf("create Mistral OCR spool: %w", createErr)
	}
	path := file.Name()
	success := false
	defer func() {
		if !success {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	if chmodErr := file.Chmod(0o600); chmodErr != nil {
		_ = source.Close()
		return Document{}, nil, fmt.Errorf("secure Mistral OCR spool: %w", chmodErr)
	}

	hash := sha256.New()
	limited := io.LimitReader(&contextReader{ctx: ctx, reader: source}, options.MaxBytes+1)
	written, copyErr := io.Copy(io.MultiWriter(file, hash), limited)
	closeSourceErr := source.Close()
	if copyErr != nil {
		return Document{}, nil, fmt.Errorf("copy Mistral OCR spool: %w", copyErr)
	}
	if closeSourceErr != nil {
		return Document{}, nil, fmt.Errorf("close verified attachment source: %w", closeSourceErr)
	}
	if written > options.MaxBytes {
		return Document{}, nil, errors.New("mistral OCR spool exceeds byte limit")
	}
	if written != options.ExpectedSize {
		return Document{}, nil, errors.New("mistral OCR source size mismatch")
	}
	actualHash := hex.EncodeToString(hash.Sum(nil))
	if actualHash != options.ExpectedSHA256 {
		return Document{}, nil, errors.New("mistral OCR source hash mismatch")
	}
	if syncErr := file.Sync(); syncErr != nil {
		return Document{}, nil, fmt.Errorf("sync Mistral OCR spool: %w", syncErr)
	}
	if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
		return Document{}, nil, fmt.Errorf("rewind Mistral OCR spool: %w", seekErr)
	}
	format, detectErr := DetectFormat(file, written, options.MediaType)
	if detectErr != nil {
		return Document{}, nil, detectErr
	}
	if closeErr := file.Close(); closeErr != nil {
		return Document{}, nil, fmt.Errorf("close Mistral OCR spool: %w", closeErr)
	}

	success = true
	document = Document{Path: path, MediaType: format.MediaType, Size: written, SHA256: actualHash}
	cleanup = func() error {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("remove Mistral OCR spool %s: %w", filepath.Base(path), err)
		}
		return nil
	}
	return document, cleanup, nil
}

func checkSpoolCapacity(options SpoolOptions) error {
	entries, err := os.ReadDir(options.Directory)
	if err != nil {
		return fmt.Errorf("read Mistral OCR spool usage: %w", err)
	}
	var used int64
	for _, entry := range entries {
		info, statErr := os.Lstat(filepath.Join(options.Directory, entry.Name()))
		if statErr != nil {
			return fmt.Errorf("inspect Mistral OCR spool usage: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 {
			return errors.New("mistral OCR spool directory contains an unsafe entry")
		}
		if used > options.MaxSpoolBytes-info.Size() {
			return errors.New("mistral OCR spool quota is exhausted")
		}
		used += info.Size()
	}
	if used > options.MaxSpoolBytes-options.ExpectedSize {
		return errors.New("mistral OCR spool quota is exhausted")
	}
	available, err := availableDiskBytes(options.Directory)
	if err != nil {
		return fmt.Errorf("inspect Mistral OCR spool free space: %w", err)
	}
	if available < options.ExpectedSize || available-options.ExpectedSize < options.MinFreeBytes {
		return errors.New("mistral OCR spool free-space reserve would be crossed")
	}
	return nil
}

func acquireSpoolReservationLock(ctx context.Context, directory string) (func(), error) {
	lockPath := filepath.Join(filepath.Dir(directory), "."+filepath.Base(directory)+spoolReservationSuffix)
	lock := flock.New(lockPath, flock.SetPermissions(0o600))
	locked, err := lock.TryLockContext(ctx, spoolLockRetryInterval)
	if err != nil {
		return nil, fmt.Errorf("acquire Mistral OCR spool reservation lock: %w", err)
	}
	if !locked {
		return nil, errors.New("mistral OCR spool reservation lock was not acquired")
	}
	return func() { _ = lock.Unlock() }, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
