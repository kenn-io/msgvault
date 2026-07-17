package export

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/msgvault/internal/mime"
)

func TestStoreAttachmentFile_ExistingFileHashMismatch_ReturnsError(t *testing.T) {
	tmp := t.TempDir()

	content := []byte("hello")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	// Create a corrupt file at the expected content-addressed path: correct size,
	// wrong contents.
	fullPath := filepath.Join(tmp, hash[:2], hash)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0700), "mkdir")
	require.NoError(t, os.WriteFile(fullPath, []byte("jello"), 0600), "write corrupt file") // same size as "hello"

	att := &mime.Attachment{
		Filename:    "a.txt",
		ContentType: "text/plain",
		Size:        len(content),
		ContentHash: hash,
		Content:     content,
	}
	_, err := StoreAttachmentFile(tmp, att)
	require.ErrorContains(t, err, "hash", "expected hash mismatch error")
}

func TestStoreAttachmentFile_ProvidedContentHashMismatch_ReturnsError(t *testing.T) {
	tmp := t.TempDir()

	content := []byte("hello")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	badSum := sha256.Sum256([]byte("jello"))
	badHash := hex.EncodeToString(badSum[:])

	att := &mime.Attachment{
		Filename:    "a.txt",
		ContentType: "text/plain",
		Size:        len(content),
		ContentHash: badHash,
		Content:     content,
	}
	_, err := StoreAttachmentFile(tmp, att)
	require.ErrorContains(t, err, "mismatch", "expected mismatch error")

	_, err = os.Stat(filepath.Join(tmp, badHash[:2], badHash))
	assert.True(t, os.IsNotExist(err), "unexpected file at provided hash path: %v", err)
	_, err = os.Stat(filepath.Join(tmp, hash[:2], hash))
	assert.True(t, os.IsNotExist(err), "unexpected file at computed hash path: %v", err)
}

func TestStoreAttachmentFile_ProvidedContentHashUppercase_AcceptedAndCanonicalized(t *testing.T) {
	require := require.New(t)
	tmp := t.TempDir()

	content := []byte("hello")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	upper := strings.ToUpper(hash)

	att := &mime.Attachment{
		Filename:    "a.txt",
		ContentType: "text/plain",
		Size:        len(content),
		ContentHash: upper,
		Content:     content,
	}
	gotStoragePath, err := StoreAttachmentFile(tmp, att)
	require.NoError(err, "StoreAttachmentFile")

	wantStoragePath := path.Join(hash[:2], hash)
	require.Equal(wantStoragePath, gotStoragePath, "storage path mismatch")
	require.Equal(hash, att.ContentHash, "ContentHash not canonicalized")
	_, err = os.Stat(filepath.Join(tmp, hash[:2], hash))
	require.NoError(err, "attachment file missing")
}

func TestStoreAttachmentFile_ConcurrentWriters_SameHash_NoError(t *testing.T) {
	require := require.New(t)
	tmp := t.TempDir()

	content := bytes.Repeat([]byte("a"), 1<<20) // 1 MiB
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	const n = 8
	start := make(chan struct{})
	errCh := make(chan error, n)
	pathCh := make(chan string, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			<-start

			att := &mime.Attachment{
				Filename:    "a.txt",
				ContentType: "text/plain",
				Size:        len(content),
				ContentHash: hash,
				Content:     content,
			}
			p, err := StoreAttachmentFile(tmp, att)
			errCh <- err
			if err == nil {
				pathCh <- p
			}
		}()
	}

	close(start)
	wg.Wait()

	for range n {
		require.NoError(<-errCh, "store")
	}

	wantStoragePath := path.Join(hash[:2], hash)
	for range n {
		require.Equal(wantStoragePath, <-pathCh, "storage path mismatch")
	}

	fullPath := filepath.Join(tmp, hash[:2], hash)
	b, err := os.ReadFile(fullPath)
	require.NoError(err, "read stored file")
	gotSum := sha256.Sum256(b)
	gotHash := hex.EncodeToString(gotSum[:])
	require.Equal(hash, gotHash, "stored file hash mismatch")
}

func TestStoreAttachmentFileDurableStoresEmptyContent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	emptySum := sha256.Sum256(nil)
	emptyHash := hex.EncodeToString(emptySum[:])
	att := &mime.Attachment{ContentHash: emptyHash}

	storagePath, err := StoreAttachmentFileDurable(dir, att)
	require.NoError(err)
	assert.Equal(path.Join(emptyHash[:2], emptyHash), storagePath)
	assert.Equal(emptyHash, att.ContentHash)
	info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(storagePath)))
	require.NoError(err)
	assert.True(info.Mode().IsRegular())
	assert.Zero(info.Size())

	skipped, err := StoreAttachmentFile(t.TempDir(), &mime.Attachment{ContentHash: emptyHash})
	require.NoError(err)
	assert.Empty(skipped, "ordinary ingest must keep its empty-content skip semantics")
}

func TestStoreAttachmentFileDurableSurfacesParentSyncFailure(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	content := []byte("directory durable attachment")
	hash := sha256.Sum256(content)
	hashText := hex.EncodeToString(hash[:])
	att := &mime.Attachment{Content: content, ContentHash: hashText}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	require.NoError(err)
	hashDir := filepath.Join(resolvedDir, hashText[:2])
	require.NoError(os.MkdirAll(hashDir, 0o700))
	syncErr := errors.New("injected attachment parent sync failure")
	originalSyncDir := pack.SyncDir
	pack.SyncDir = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(hashDir) {
			return syncErr
		}
		return originalSyncDir(path)
	}
	t.Cleanup(func() { pack.SyncDir = originalSyncDir })

	_, err = StoreAttachmentFileDurable(dir, att)
	require.ErrorIs(err, syncErr)

	pack.SyncDir = originalSyncDir
	storagePath, err := StoreAttachmentFileDurable(dir, att)
	require.NoError(err, "retry must validate and durably reuse loose residue")
	require.FileExists(filepath.Join(dir, filepath.FromSlash(storagePath)))
}

func TestStoreAttachmentFileDurableRejectsCanonicalSymlink(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	content := []byte("symlinks are not durable authority")
	hash := sha256.Sum256(content)
	hashText := hex.EncodeToString(hash[:])
	hashDir := filepath.Join(dir, hashText[:2])
	require.NoError(os.MkdirAll(hashDir, 0o700))
	target := filepath.Join(dir, "outside")
	require.NoError(os.WriteFile(target, content, 0o600))
	canonical := filepath.Join(hashDir, hashText)
	if err := os.Symlink(target, canonical); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err := StoreAttachmentFileDurable(dir, &mime.Attachment{
		Content: content, ContentHash: hashText,
	})
	require.Error(err)
	info, err := os.Lstat(canonical)
	require.NoError(err)
	assert.NotZero(info.Mode() & os.ModeSymlink)
	got, err := os.ReadFile(target)
	require.NoError(err)
	assert.Equal(content, got)
}

func TestStoreAttachmentFileDurableSyncsNewHashDirectoryAndFinalParent(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	resolvedDir, err := filepath.EvalSymlinks(dir)
	require.NoError(err)
	content := []byte("directory sync ordering")
	hash := sha256.Sum256(content)
	hashText := hex.EncodeToString(hash[:])
	hashDir := filepath.Join(resolvedDir, hashText[:2])
	originalSyncDir := pack.SyncDir
	var synced []string
	pack.SyncDir = func(path string) error {
		synced = append(synced, filepath.Clean(path))
		return originalSyncDir(path)
	}
	t.Cleanup(func() { pack.SyncDir = originalSyncDir })

	_, err = StoreAttachmentFileDurable(dir, &mime.Attachment{
		Content: content, ContentHash: hashText,
	})
	require.NoError(err)
	require.Equal([]string{filepath.Clean(resolvedDir), filepath.Clean(hashDir)}, synced,
		"new hash dir entry must be synced before the final file entry")
}
