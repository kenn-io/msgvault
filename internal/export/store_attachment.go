package export

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/mime"
)

var attachmentLooseStores sync.Map

type attachmentLooseStoreKey struct {
	baseDir string
	staging packstore.StagingMode
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

	var expectedHash packstore.Hash
	var err error
	if att.ContentHash != "" {
		normalized := strings.ToLower(att.ContentHash)
		if err := ValidateContentHash(normalized); err != nil {
			return "", fmt.Errorf("invalid attachment content hash %q: %w", normalized, err)
		}
		expectedHash, err = packstore.ParseHash(normalized)
		if err != nil {
			return "", fmt.Errorf("parse attachment content hash: %w", err)
		}
	}
	baseDir, err := prepareStorageDir(attachmentsDir)
	if err != nil {
		return "", err
	}
	loose, err := attachmentLooseStore(baseDir, packstore.StagingSameDirectory)
	if err != nil {
		return "", err
	}

	result, writeErr := loose.WriteBytes(context.Background(), att.Content, packstore.WriteOptions{
		Durability:   packstore.AtomicPublication,
		Dedup:        packstore.VerifyFullHash,
		ExpectedHash: expectedHash,
		ExpectedSize: int64(len(att.Content)),
		SizeKnown:    true,
	})
	if result.Hash != "" && (expectedHash == "" || result.Hash == expectedHash) {
		att.ContentHash = result.Hash.String()
	}
	if writeErr != nil {
		return "", fmt.Errorf("store loose attachment: %w", writeErr)
	}
	contentHash := result.Hash.String()
	return path.Join(contentHash[:2], contentHash), nil
}

func attachmentLooseStore(baseDir string, staging packstore.StagingMode) (*packstore.LooseStore, error) {
	key := attachmentLooseStoreKey{baseDir: baseDir, staging: staging}
	if existing, ok := attachmentLooseStores.Load(key); ok {
		store, ok := existing.(*packstore.LooseStore)
		if !ok {
			return nil, errors.New("attachment loose store cache contains an invalid value")
		}
		return store, nil
	}
	options := packstore.LayoutOptions{Staging: staging}
	if staging == packstore.StagingStoreDirectory {
		options.StagingDir = "."
	}
	layout, err := packstore.NewLayout(baseDir, options)
	if err != nil {
		return nil, fmt.Errorf("create attachment layout: %w", err)
	}
	loose, err := packstore.NewLooseStore(layout)
	if err != nil {
		return nil, fmt.Errorf("create loose attachment store: %w", err)
	}
	actual, _ := attachmentLooseStores.LoadOrStore(key, loose)
	store, ok := actual.(*packstore.LooseStore)
	if !ok {
		return nil, errors.New("attachment loose store cache contains an invalid value")
	}
	return store, nil
}

// StoreAttachmentFileDurable stores attachment content, including an empty
// blob, through the content-addressed atomic write path. Unlike ordinary
// ingest, this entry point is reserved for maintenance that will discard an
// existing authoritative copy after the loose file is durable.
func StoreAttachmentFileDurable(attachmentsDir string, att *mime.Attachment) (string, error) {
	if attachmentsDir == "" {
		return "", nil
	}
	var expectedHash packstore.Hash
	var err error
	if att.ContentHash != "" {
		normalized := strings.ToLower(att.ContentHash)
		if err := ValidateContentHash(normalized); err != nil {
			return "", fmt.Errorf("invalid attachment content hash %q: %w", normalized, err)
		}
		expectedHash, err = packstore.ParseHash(normalized)
		if err != nil {
			return "", fmt.Errorf("parse attachment content hash: %w", err)
		}
	}
	baseDir, err := prepareStorageDir(attachmentsDir)
	if err != nil {
		return "", err
	}
	loose, err := attachmentLooseStore(baseDir, packstore.StagingSameDirectory)
	if err != nil {
		return "", err
	}
	result, writeErr := loose.WriteBytes(context.Background(), att.Content, packstore.WriteOptions{
		Durability:   packstore.DurablePublication,
		Dedup:        packstore.VerifyFullHash,
		ExpectedHash: expectedHash,
		ExpectedSize: int64(len(att.Content)),
		SizeKnown:    true,
	})
	if result.Hash != "" && (expectedHash == "" || result.Hash == expectedHash) {
		att.ContentHash = result.Hash.String()
	}
	if writeErr != nil {
		return "", fmt.Errorf("store durable loose attachment: %w", writeErr)
	}
	contentHash := result.Hash.String()
	return path.Join(contentHash[:2], contentHash), nil
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
	hash, err := packstore.ParseHash(contentHash)
	if err != nil {
		return "", contentHash, size, fmt.Errorf("parse attachment content hash: %w", err)
	}
	loose, err := attachmentLooseStore(baseDir, packstore.StagingStoreDirectory)
	if err != nil {
		return "", contentHash, size, err
	}
	if _, exists, err := loose.Verify(hash, size, packstore.VerifyFullHash, packstore.AtomicPublication); err != nil {
		return "", contentHash, size, fmt.Errorf("verify loose attachment: %w", err)
	} else if exists {
		return path.Join(contentHash[:2], contentHash), contentHash, size, nil
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", contentHash, size, fmt.Errorf("rewind attachment source: %w", err)
	}
	result, writeErr := loose.Write(context.Background(), f, packstore.WriteOptions{
		Durability: packstore.AtomicPublication,
		Dedup:      packstore.VerifyFullHash,
		MaxBytes:   maxSize,
	})
	if result.Hash != "" {
		contentHash = result.Hash.String()
		size = result.Size
	}
	if writeErr != nil {
		return "", contentHash, size, fmt.Errorf("store attachment source %q: %w", srcPath, writeErr)
	}
	return path.Join(contentHash[:2], contentHash), contentHash, size, nil
}
