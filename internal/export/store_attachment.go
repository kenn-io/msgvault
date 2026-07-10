package export

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/mime"
)

// key: fullPath + size + expectedHash -> value: validatedAttachmentFile
var validatedAttachmentFiles sync.Map

type validatedAttachmentFile struct {
	modTime int64
	info    os.FileInfo
}

// syncDurableAttachmentFile is a narrow failure-injection seam. Production
// always fsyncs the validated final canonical descriptor.
var syncDurableAttachmentFile = func(f *os.File) error { return f.Sync() }

// lstatValidatedAttachmentFile is a narrow identity-swap test seam.
// Production returns platform-specific identity-stable path metadata.
var lstatValidatedAttachmentFile = snapshotAttachmentPathIdentity

var errAttachmentFileIdentityChanged = errors.New("attachment file identity changed")

type attachmentFileIdentityChangedError struct {
	path  string
	phase string
}

func (e *attachmentFileIdentityChangedError) Error() string {
	return fmt.Sprintf("attachment file %q changed identity %s", e.path, e.phase)
}

func (e *attachmentFileIdentityChangedError) Unwrap() error {
	return errAttachmentFileIdentityChanged
}

type attachmentFileValidationCloseError struct {
	err error
}

func (e *attachmentFileValidationCloseError) Error() string {
	return fmt.Sprintf("close attachment file after validation: %v", e.err)
}

func (e *attachmentFileValidationCloseError) Unwrap() error {
	return e.err
}

// resolveContentHash computes the SHA-256 of content and validates it against
// the provided hash (if any). Returns the canonical lowercase hash without
// mutating the attachment.
func resolveContentHash(content []byte, providedHash string) (string, error) {
	sum := sha256.Sum256(content)
	computed := hex.EncodeToString(sum[:])

	if providedHash == "" {
		return computed, nil
	}

	normalized := strings.ToLower(providedHash)
	if err := ValidateContentHash(normalized); err != nil {
		return "", fmt.Errorf("invalid attachment content hash %q: %w", normalized, err)
	}
	if normalized != computed {
		return "", fmt.Errorf("attachment content hash mismatch: provided %q, computed %q", normalized, computed)
	}
	return normalized, nil
}

// prepareStorageDir ensures the base attachments directory exists, resolves
// symlinks, and returns the resolved absolute path.
func prepareStorageDir(attachmentsDir string) (string, error) {
	baseDir, err := filepath.Abs(attachmentsDir)
	if err != nil {
		return "", fmt.Errorf("abs attachments dir %q: %w", attachmentsDir, err)
	}
	if err := fileutil.SecureMkdirAll(baseDir, 0700); err != nil {
		return "", fmt.Errorf("create attachments dir: %w", err)
	}
	if err := fileutil.SecureChmod(baseDir, 0700); err != nil {
		return "", fmt.Errorf("chmod attachments dir: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve attachments dir %q: %w", attachmentsDir, err)
	}
	st, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("lstat attachments dir: %w", err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("attachments dir %q is not a directory", resolved)
	}
	return resolved, nil
}

// checkSubdirSafe verifies the hash-prefix subdirectory is not a symlink,
// without creating it.
func checkSubdirSafe(baseDir, hashPrefix string) error {
	subdirPath := filepath.Join(baseDir, hashPrefix)
	if st, err := os.Lstat(subdirPath); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("attachment dir %q is a symlink", subdirPath)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("lstat attachment dir: %w", err)
	}
	return nil
}

// ensureSubdirSafe creates the hash-prefix subdirectory and checks it is
// not a symlink.
func ensureSubdirSafe(baseDir, hashPrefix string) error {
	if err := checkSubdirSafe(baseDir, hashPrefix); err != nil {
		return err
	}
	return fileutil.SecureMkdirAll(filepath.Join(baseDir, hashPrefix), 0700)
}

// writeAtomicFile writes data to a temp file alongside fullPath and renames
// it into place. On rename conflict (concurrent writer), validates the
// existing file instead.
func writeAtomicFile(fullPath string, data []byte, expectedSize int64, expectedHash string) error {
	return writeAtomicFileStream(fullPath, bytes.NewReader(data), expectedSize, expectedHash)
}

// writeAtomicFileStream is writeAtomicFile for a streaming source.
func writeAtomicFileStream(fullPath string, src io.Reader, expectedSize int64, expectedHash string) error {
	dir := filepath.Dir(fullPath)
	base := filepath.Base(fullPath)

	tmp, err := os.CreateTemp(dir, base+".tmp.")
	if err != nil {
		return fmt.Errorf("create temp attachment file: %w", err)
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if err := fileutil.SecureChmod(tmpPath, 0600); err != nil {
		return fmt.Errorf("chmod temp attachment file: %w", err)
	}

	if _, err := io.Copy(tmp, src); err != nil {
		return fmt.Errorf("write attachment file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close attachment file: %w", err)
	}

	if err := os.Rename(tmpPath, fullPath); err != nil {
		if _, statErr := os.Lstat(fullPath); statErr == nil {
			// Another writer may have installed the final file first (notably
			// on Windows; Unix rename typically overwrites). Validate the
			// existing file.
			removeTmp = false
			_ = os.Remove(tmpPath)
			return validateExistingAttachmentFile(fullPath, expectedSize, expectedHash)
		}
		return fmt.Errorf("rename attachment file into place: %w", err)
	}
	removeTmp = false
	return nil
}

// StoreAttachmentFile stores att.Content on disk under attachmentsDir using
// content-addressed storage (hash[:2]/hash). It validates existing files when
// de-duping. If attachmentsDir is a symlink, it is resolved before writing.
//
// Returns the storage path relative to attachmentsDir (e.g. "ab/<hash>"), or
// empty string if nothing was stored.
func StoreAttachmentFile(attachmentsDir string, att *mime.Attachment) (string, error) {
	if attachmentsDir == "" || len(att.Content) == 0 {
		return "", nil
	}

	contentHash, err := resolveContentHash(att.Content, att.ContentHash)
	if err != nil {
		return "", err
	}
	att.ContentHash = contentHash

	hashPrefix := contentHash[:2]
	storagePath := path.Join(hashPrefix, contentHash)

	baseDir, err := prepareStorageDir(attachmentsDir)
	if err != nil {
		return "", err
	}

	if err := ensureSubdirSafe(baseDir, hashPrefix); err != nil {
		return "", err
	}

	fullPath := filepath.Join(baseDir, hashPrefix, contentHash)
	expectedSize := int64(len(att.Content))

	if _, err := os.Lstat(fullPath); err == nil {
		if err := validateExistingAttachmentFile(fullPath, expectedSize, contentHash); err != nil {
			return "", err
		}
		return storagePath, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("lstat attachment file: %w", err)
	}

	if err := writeAtomicFile(fullPath, att.Content, expectedSize, contentHash); err != nil {
		return "", err
	}
	return storagePath, nil
}

// StoreAttachmentFileDurable stores attachment content, including an empty
// blob, through the content-addressed atomic write path. Unlike ordinary
// ingest, this entry point is reserved for maintenance that will discard an
// existing authoritative copy after the loose file is durable.
func StoreAttachmentFileDurable(attachmentsDir string, att *mime.Attachment) (string, error) {
	if attachmentsDir == "" {
		return "", nil
	}
	contentHash, err := resolveContentHash(att.Content, att.ContentHash)
	if err != nil {
		return "", err
	}
	att.ContentHash = contentHash
	hashPrefix := contentHash[:2]
	storagePath := path.Join(hashPrefix, contentHash)
	baseDir, err := prepareStorageDir(attachmentsDir)
	if err != nil {
		return "", err
	}
	if err := checkSubdirSafe(baseDir, hashPrefix); err != nil {
		return "", err
	}
	hashDir := filepath.Join(baseDir, hashPrefix)
	if err := ensureSubdirSafe(baseDir, hashPrefix); err != nil {
		return "", err
	}
	if err := pack.SyncDir(baseDir); err != nil {
		return "", fmt.Errorf("sync attachments base before durable attachment publish: %w", err)
	}
	fullPath := filepath.Join(baseDir, hashPrefix, contentHash)
	expectedSize := int64(len(att.Content))
	if _, err := os.Lstat(fullPath); err == nil {
		if err := validateExistingAttachmentFileDurable(fullPath, expectedSize, contentHash); err != nil {
			return "", err
		}
		if err := pack.SyncDir(hashDir); err != nil {
			return "", fmt.Errorf("sync durable attachment parent: %w", err)
		}
		return storagePath, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("lstat attachment file: %w", err)
	}
	if err := writeAtomicFile(fullPath, att.Content, expectedSize, contentHash); err != nil {
		return "", err
	}
	if err := validateExistingAttachmentFileDurable(fullPath, expectedSize, contentHash); err != nil {
		return "", err
	}
	if err := pack.SyncDir(hashDir); err != nil {
		return "", fmt.Errorf("sync durable attachment parent: %w", err)
	}
	return storagePath, nil
}

// hashSourceFile hashes f without staging any bytes, enforcing maxSize on
// the bytes actually read.
func hashSourceFile(f *os.File, srcPath string, maxSize int64) (string, int64, error) {
	src := io.Reader(f)
	if maxSize > 0 {
		src = io.LimitReader(f, maxSize+1)
	}
	h := sha256.New()
	size, err := io.Copy(h, src)
	if err != nil {
		return "", 0, fmt.Errorf("hash attachment source: %w", err)
	}
	if maxSize > 0 && size > maxSize {
		return "", 0, fmt.Errorf("attachment source %q exceeds %d bytes", srcPath, maxSize)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// stageAttachmentSource copies f into a temp file under baseDir, hashing in
// the same read so the staged bytes always match the returned hash and size.
// On success the caller owns the temp file at the returned path; on error
// the temp file is already removed. When the failure happens after the copy
// fully hashed the source (the temp-file close), the computed hash and size
// are returned alongside the error so the caller can still honor
// StoreAttachmentFromPath's failed-store metadata contract.
func stageAttachmentSource(baseDir string, f *os.File, srcPath string, maxSize int64) (string, string, int64, error) {
	tmp, err := os.CreateTemp(baseDir, "attachment.tmp.")
	if err != nil {
		return "", "", 0, fmt.Errorf("create temp attachment file: %w", err)
	}
	tmpPath := tmp.Name()
	staged := false
	defer func() {
		if !staged {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := fileutil.SecureChmod(tmpPath, 0600); err != nil {
		return "", "", 0, fmt.Errorf("chmod temp attachment file: %w", err)
	}

	src := io.Reader(f)
	if maxSize > 0 {
		src = io.LimitReader(f, maxSize+1)
	}
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), src)
	if err != nil {
		return "", "", 0, fmt.Errorf("stage attachment source: %w", err)
	}
	if maxSize > 0 && size > maxSize {
		return "", "", 0, fmt.Errorf("attachment source %q exceeds %d bytes", srcPath, maxSize)
	}
	if err := tmp.Close(); err != nil {
		return "", hex.EncodeToString(h.Sum(nil)), size,
			fmt.Errorf("close temp attachment file: %w", err)
	}
	staged = true
	return tmpPath, hex.EncodeToString(h.Sum(nil)), size, nil
}

// StoreAttachmentFromPath streams the regular file at srcPath into
// content-addressed storage under attachmentsDir (hash[:2]/hash), hashing
// without loading the file into memory. maxSize > 0 rejects larger sources.
//
// The source is hashed before any bytes are staged, so importing content
// that is already stored needs no temp-file writes and no free disk space.
// When the blob is new, the source is re-read and staged with the hash
// recomputed in the same read, so the stored bytes always match the returned
// hash and size even if the source file changes between the two reads;
// maxSize is enforced on the bytes actually read, not just the pre-read stat.
//
// Returns the storage path relative to attachmentsDir, the content hash, and
// the stored size. On failures after the source was hashed, contentHash and
// size are still returned (with an empty storage path) so callers can record
// metadata for content they could not store.
func StoreAttachmentFromPath(attachmentsDir, srcPath string, maxSize int64) (string, string, int64, error) {
	if attachmentsDir == "" || srcPath == "" {
		return "", "", 0, errors.New("attachments dir and source path are required")
	}
	linfo, err := os.Lstat(srcPath)
	if err != nil {
		return "", "", 0, fmt.Errorf("lstat attachment source: %w", err)
	}
	if !linfo.Mode().IsRegular() {
		return "", "", 0, fmt.Errorf("attachment source %q is not a regular file", srcPath)
	}
	if maxSize > 0 && linfo.Size() > maxSize {
		return "", "", 0, fmt.Errorf("attachment source %q is %d bytes (max %d)", srcPath, linfo.Size(), maxSize)
	}

	f, err := os.Open(srcPath)
	if err != nil {
		return "", "", 0, fmt.Errorf("open attachment source: %w", err)
	}
	defer func() { _ = f.Close() }()

	baseDir, err := prepareStorageDir(attachmentsDir)
	if err != nil {
		return "", "", 0, err
	}

	contentHash, size, err := hashSourceFile(f, srcPath, maxSize)
	if err != nil {
		return "", "", 0, err
	}
	hashPrefix := contentHash[:2]
	storagePath := path.Join(hashPrefix, contentHash)
	if err := checkSubdirSafe(baseDir, hashPrefix); err != nil {
		return "", contentHash, size, err
	}
	fullPath := filepath.Join(baseDir, hashPrefix, contentHash)
	if _, err := os.Lstat(fullPath); err == nil {
		if err := validateExistingAttachmentFile(fullPath, size, contentHash); err != nil {
			return "", contentHash, size, err
		}
		return storagePath, contentHash, size, nil
	} else if !os.IsNotExist(err) {
		return "", contentHash, size, fmt.Errorf("lstat attachment file: %w", err)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", contentHash, size, fmt.Errorf("rewind attachment source: %w", err)
	}
	tmpPath, stagedHash, stagedSize, err := stageAttachmentSource(baseDir, f, srcPath, maxSize)
	if err != nil {
		if stagedHash != "" {
			// The staging read hashed the full source before failing; being
			// the most recent read, it governs the returned metadata just as
			// it would have on success.
			return "", stagedHash, stagedSize, err
		}
		return "", contentHash, size, err
	}
	// The staged hash governs from here so the stored bytes match the
	// returned metadata even if the source changed between the two reads.
	contentHash, size = stagedHash, stagedSize
	hashPrefix = contentHash[:2]
	storagePath = path.Join(hashPrefix, contentHash)
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := ensureSubdirSafe(baseDir, hashPrefix); err != nil {
		return "", contentHash, size, err
	}
	fullPath = filepath.Join(baseDir, hashPrefix, contentHash)
	if _, err := os.Lstat(fullPath); err == nil {
		if err := validateExistingAttachmentFile(fullPath, size, contentHash); err != nil {
			return "", contentHash, size, err
		}
		return storagePath, contentHash, size, nil
	} else if !os.IsNotExist(err) {
		return "", contentHash, size, fmt.Errorf("lstat attachment file: %w", err)
	}

	// A concurrent writer that wins the race between the existence check and
	// this rename staged bytes for the same hash, so a POSIX rename replacing
	// its file installs identical content. On Windows, where rename does not
	// replace, validate the winner's file and let the deferred cleanup drop
	// our staged copy.
	if err := os.Rename(tmpPath, fullPath); err != nil {
		if _, statErr := os.Lstat(fullPath); statErr == nil {
			if err := validateExistingAttachmentFile(fullPath, size, contentHash); err != nil {
				return "", contentHash, size, err
			}
			return storagePath, contentHash, size, nil
		}
		return "", contentHash, size, fmt.Errorf("rename attachment file into place: %w", err)
	}
	removeTmp = false
	return storagePath, contentHash, size, nil
}

func validateExistingAttachmentFile(fullPath string, expectedSize int64, expectedHash string) error {
	const maxIdentityAttempts = 5
	var lastErr error
	for attempt := range maxIdentityAttempts {
		f, descriptorInfo, err := openValidatedAttachmentFile(fullPath, expectedSize, expectedHash, false)
		if err == nil {
			identityErr := validateAttachmentPathIdentity(fullPath, f, descriptorInfo)
			if closeErr := closeValidatedAttachmentFile(f); closeErr != nil {
				return errors.Join(identityErr, closeErr)
			}
			if identityErr == nil {
				return nil
			}
			lastErr = identityErr
		} else {
			lastErr = err
		}

		if !isRetryableAttachmentFileIdentityChange(lastErr) || attempt == maxIdentityAttempts-1 {
			return lastErr
		}
		runtime.Gosched()
		time.Sleep(time.Duration(attempt+1) * time.Millisecond)
	}
	return lastErr
}

func isRetryableAttachmentFileIdentityChange(err error) bool {
	var closeErr *attachmentFileValidationCloseError
	if errors.As(err, &closeErr) {
		return false
	}
	var identityErr *attachmentFileIdentityChangedError
	return errors.As(err, &identityErr)
}

func validateExistingAttachmentFileDurable(fullPath string, expectedSize int64, expectedHash string) error {
	f, descriptorInfo, err := openValidatedAttachmentFile(fullPath, expectedSize, expectedHash, true)
	if err != nil {
		return err
	}
	if err := syncDurableAttachmentFile(f); err != nil {
		return errors.Join(
			fmt.Errorf("sync durable attachment file: %w", err),
			closeDurableAttachmentFile(f),
		)
	}
	if err := validateAttachmentPathIdentity(fullPath, f, descriptorInfo); err != nil {
		return errors.Join(err, closeDurableAttachmentFile(f))
	}
	return closeDurableAttachmentFile(f)
}

func closeDurableAttachmentFile(f *os.File) error {
	if err := f.Close(); err != nil {
		return fmt.Errorf("close durable attachment file: %w", err)
	}
	return nil
}

func closeValidatedAttachmentFile(f *os.File) error {
	if err := f.Close(); err != nil {
		return &attachmentFileValidationCloseError{err: err}
	}
	return nil
}

func openValidatedAttachmentFile(fullPath string, expectedSize int64, expectedHash string, durable bool) (
	resultFile *os.File, resultInfo os.FileInfo, resultErr error,
) {
	preOpenInfo, err := lstatValidatedAttachmentFile(fullPath)
	if err != nil {
		return nil, nil, fmt.Errorf("lstat attachment file before validation: %w", err)
	}
	if err := validateAttachmentPathInfo(fullPath, preOpenInfo); err != nil {
		return nil, nil, err
	}

	var f *os.File
	const maxRetries = 5
	for attempt := range maxRetries {
		if durable {
			f, err = openNoFollowDurable(fullPath)
		} else {
			f, err = openNoFollow(fullPath)
		}
		if err == nil {
			break
		}
		if runtime.GOOS != "windows" || attempt == maxRetries-1 {
			return nil, nil, fmt.Errorf(
				"open attachment file for validation: %w", err,
			)
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	keepOpen := false
	defer func() {
		if keepOpen || f == nil {
			return
		}
		var closeErr error
		if durable {
			closeErr = closeDurableAttachmentFile(f)
		} else {
			closeErr = closeValidatedAttachmentFile(f)
		}
		if closeErr != nil {
			resultErr = errors.Join(resultErr, closeErr)
		}
	}()

	st, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat attachment file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("attachment file %q descriptor is not a regular file", fullPath)
	}
	if !os.SameFile(preOpenInfo, st) {
		return nil, nil, &attachmentFileIdentityChangedError{
			path: fullPath, phase: "before validation",
		}
	}
	if st.Size() != expectedSize {
		return nil, nil, fmt.Errorf("attachment file %q has size %d, want %d", fullPath, st.Size(), expectedSize)
	}

	key := fmt.Sprintf("%s\x00%d\x00%s", fullPath, expectedSize, expectedHash)
	modTime := st.ModTime().UnixNano()
	if cached, ok := validatedAttachmentFiles.Load(key); ok {
		if validated, ok := cached.(validatedAttachmentFile); ok &&
			validated.modTime == modTime && os.SameFile(validated.info, st) {
			keepOpen = true
			return f, st, nil
		}
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, nil, fmt.Errorf("hash attachment file: %w", err)
	}
	gotHash := hex.EncodeToString(h.Sum(nil))
	if gotHash != expectedHash {
		return nil, nil, fmt.Errorf("attachment file %q has hash %q, want %q", fullPath, gotHash, expectedHash)
	}
	validatedAttachmentFiles.Store(key, validatedAttachmentFile{
		modTime: modTime,
		info:    st,
	})
	keepOpen = true
	return f, st, nil
}

func validateAttachmentPathInfo(fullPath string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("attachment file %q is a symlink", fullPath)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("attachment file %q is not a regular file", fullPath)
	}
	if err := validateNoFollowFileInfo(info); err != nil {
		return fmt.Errorf("attachment file %q %w", fullPath, err)
	}
	return nil
}

func validateAttachmentPathIdentity(fullPath string, f *os.File, descriptorInfo os.FileInfo) error {
	postOpenInfo, err := lstatValidatedAttachmentFile(fullPath)
	if err != nil {
		return fmt.Errorf("lstat attachment file after validation: %w", err)
	}
	if err := validateAttachmentPathInfo(fullPath, postOpenInfo); err != nil {
		return err
	}
	currentInfo, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat attachment descriptor after validation: %w", err)
	}
	if !currentInfo.Mode().IsRegular() || !os.SameFile(descriptorInfo, currentInfo) ||
		!os.SameFile(postOpenInfo, currentInfo) {
		return &attachmentFileIdentityChangedError{
			path: fullPath, phase: "during validation",
		}
	}
	return nil
}
