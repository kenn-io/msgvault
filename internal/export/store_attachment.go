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

	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/mime"
)

// key: fullPath + size + expectedHash -> value: modTime (int64)
var validatedAttachmentFiles sync.Map

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

// ensureSubdirSafe creates the hash-prefix subdirectory and checks it is
// not a symlink.
func ensureSubdirSafe(baseDir, hashPrefix string) error {
	subdirPath := filepath.Join(baseDir, hashPrefix)
	if st, err := os.Lstat(subdirPath); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("attachment dir %q is a symlink", subdirPath)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("lstat attachment dir: %w", err)
	}
	return fileutil.SecureMkdirAll(subdirPath, 0700)
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

// StoreAttachmentFromPath streams the regular file at srcPath into
// content-addressed storage under attachmentsDir (hash[:2]/hash), hashing
// without loading the file into memory. maxSize > 0 rejects larger sources.
//
// Returns the storage path relative to attachmentsDir, the content hash, and
// the source size. On failures after the source was hashed, contentHash and
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
	size := linfo.Size()
	if maxSize > 0 && size > maxSize {
		return "", "", 0, fmt.Errorf("attachment source %q is %d bytes (max %d)", srcPath, size, maxSize)
	}

	f, err := os.Open(srcPath)
	if err != nil {
		return "", "", 0, fmt.Errorf("open attachment source: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", "", 0, fmt.Errorf("hash attachment source: %w", err)
	}
	contentHash := hex.EncodeToString(h.Sum(nil))

	hashPrefix := contentHash[:2]
	storagePath := path.Join(hashPrefix, contentHash)

	baseDir, err := prepareStorageDir(attachmentsDir)
	if err != nil {
		return "", contentHash, size, err
	}
	if err := ensureSubdirSafe(baseDir, hashPrefix); err != nil {
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
	if err := writeAtomicFileStream(fullPath, f, size, contentHash); err != nil {
		return "", contentHash, size, err
	}
	return storagePath, contentHash, size, nil
}

func validateExistingAttachmentFile(fullPath string, expectedSize int64, expectedHash string) error {
	var f *os.File
	var err error
	const maxRetries = 5
	for attempt := range maxRetries {
		f, err = openNoFollow(fullPath)
		if err == nil {
			break
		}
		if runtime.GOOS != "windows" || attempt == maxRetries-1 {
			return fmt.Errorf(
				"open attachment file for validation: %w", err,
			)
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat attachment file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("attachment file %q is not a regular file", fullPath)
	}
	if st.Size() != expectedSize {
		return fmt.Errorf("attachment file %q has size %d, want %d", fullPath, st.Size(), expectedSize)
	}

	key := fmt.Sprintf("%s\x00%d\x00%s", fullPath, expectedSize, expectedHash)
	modTime := st.ModTime().UnixNano()
	if cached, ok := validatedAttachmentFiles.Load(key); ok {
		if ts, ok := cached.(int64); ok && ts == modTime {
			return nil
		}
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash attachment file: %w", err)
	}
	gotHash := hex.EncodeToString(h.Sum(nil))
	if gotHash != expectedHash {
		return fmt.Errorf("attachment file %q has hash %q, want %q", fullPath, gotHash, expectedHash)
	}
	validatedAttachmentFiles.Store(key, modTime)
	return nil
}
