package export

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

func TestStoreAttachmentFileDurableSurfacesFileSyncFailure(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	content := []byte("durable attachment")
	hash := sha256.Sum256(content)
	att := &mime.Attachment{Content: content, ContentHash: hex.EncodeToString(hash[:])}
	syncErr := errors.New("injected attachment file sync failure")
	originalSync := syncDurableAttachmentFile
	syncDurableAttachmentFile = func(*os.File) error { return syncErr }
	t.Cleanup(func() { syncDurableAttachmentFile = originalSync })
	normalAtt := &mime.Attachment{Content: content, ContentHash: hex.EncodeToString(hash[:])}
	_, err := StoreAttachmentFile(t.TempDir(), normalAtt)
	require.NoError(err, "ordinary ingest must not pay or depend on the durable fsync path")

	_, err = StoreAttachmentFileDurable(dir, att)
	require.ErrorIs(err, syncErr)

	var retrySyncs int
	syncDurableAttachmentFile = func(file *os.File) error {
		retrySyncs++
		return originalSync(file)
	}
	storagePath, err := StoreAttachmentFileDurable(dir, att)
	require.NoError(err, "retry must validate and durably reuse loose residue")
	require.Equal(1, retrySyncs, "existing canonical file must still be fsynced")
	require.FileExists(filepath.Join(dir, filepath.FromSlash(storagePath)))
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

func TestStoreAttachmentFileDurableRejectsIdentitySwapAfterOpen(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	content := []byte("same bytes do not imply same file identity")
	hash := sha256.Sum256(content)
	hashText := hex.EncodeToString(hash[:])
	att := &mime.Attachment{Content: content, ContentHash: hashText}
	storagePath, err := StoreAttachmentFileDurable(dir, att)
	require.NoError(err)
	resolvedDir, err := filepath.EvalSymlinks(dir)
	require.NoError(err)
	canonical := filepath.Join(resolvedDir, filepath.FromSlash(storagePath))
	displaced := canonical + ".displaced"
	originalLstat := lstatValidatedAttachmentFile
	var canonicalLstats int
	lstatValidatedAttachmentFile = func(path string) (os.FileInfo, error) {
		if filepath.Clean(path) == filepath.Clean(canonical) {
			canonicalLstats++
			if canonicalLstats == 2 {
				require.NoError(os.Rename(canonical, displaced))
				require.NoError(os.WriteFile(canonical, content, 0o600))
			}
		}
		return originalLstat(path)
	}
	t.Cleanup(func() { lstatValidatedAttachmentFile = originalLstat })

	_, err = StoreAttachmentFileDurable(dir, att)
	require.Error(err)
	assert.Contains(err.Error(), "identity")
	require.ErrorIs(err, errAttachmentFileIdentityChanged)
	assert.Equal(2, canonicalLstats)
	assert.FileExists(canonical)
	assert.FileExists(displaced)
}

func TestValidateExistingAttachmentFileRetriesIdentitySwapAfterOpen(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	content := []byte("ordinary validation retries a same-content replacement")
	hash := sha256.Sum256(content)
	hashText := hex.EncodeToString(hash[:])
	canonical := filepath.Join(dir, hashText)
	require.NoError(os.WriteFile(canonical, content, 0o600))
	displaced := canonical + ".displaced"
	originalLstat := lstatValidatedAttachmentFile
	var canonicalLstats int
	lstatValidatedAttachmentFile = func(path string) (os.FileInfo, error) {
		if filepath.Clean(path) == filepath.Clean(canonical) {
			canonicalLstats++
			if canonicalLstats == 2 {
				require.NoError(os.Rename(canonical, displaced))
				require.NoError(os.WriteFile(canonical, content, 0o600))
			}
		}
		return originalLstat(path)
	}
	t.Cleanup(func() { lstatValidatedAttachmentFile = originalLstat })

	err := validateExistingAttachmentFile(canonical, int64(len(content)), hashText)
	require.NoError(err)
	assert.Equal(4, canonicalLstats)
	assert.FileExists(canonical)
	assert.FileExists(displaced)
	got, err := os.ReadFile(canonical)
	require.NoError(err)
	assert.Equal(content, got)
}

func TestValidateExistingAttachmentFileRetriesIdentitySwapBeforeOpen(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	content := []byte("ordinary validation retries a pre-open replacement")
	hash := sha256.Sum256(content)
	hashText := hex.EncodeToString(hash[:])
	canonical := filepath.Join(dir, hashText)
	require.NoError(os.WriteFile(canonical, content, 0o600))
	displaced := canonical + ".displaced"
	originalLstat := lstatValidatedAttachmentFile
	var canonicalLstats int
	lstatValidatedAttachmentFile = func(path string) (os.FileInfo, error) {
		if filepath.Clean(path) != filepath.Clean(canonical) {
			return originalLstat(path)
		}
		canonicalLstats++
		if canonicalLstats != 1 {
			return originalLstat(path)
		}
		info, err := originalLstat(path)
		require.NoError(err)
		require.NoError(os.Rename(canonical, displaced))
		require.NoError(os.WriteFile(canonical, content, 0o600))
		replacementInfo, err := originalLstat(path)
		require.NoError(err)
		require.False(os.SameFile(info, replacementInfo),
			"the saved production helper must snapshot identity before replacement")
		return info, nil
	}
	t.Cleanup(func() { lstatValidatedAttachmentFile = originalLstat })

	err := validateExistingAttachmentFile(canonical, int64(len(content)), hashText)
	require.NoError(err)
	assert.Equal(3, canonicalLstats)
	assert.FileExists(canonical)
	assert.FileExists(displaced)
	got, err := os.ReadFile(canonical)
	require.NoError(err)
	assert.Equal(content, got)
}

func TestValidateExistingAttachmentFileFailsClosedWhenIdentitySnapshotUnavailable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	content := []byte("content")
	hash := sha256.Sum256(content)
	hashText := hex.EncodeToString(hash[:])
	filePath := filepath.Join(t.TempDir(), hashText)
	require.NoError(os.WriteFile(filePath, content, 0o600))
	originalLstat := lstatValidatedAttachmentFile
	snapshotErr := errors.New("identity snapshot unavailable")
	lstatValidatedAttachmentFile = func(string) (os.FileInfo, error) {
		return nil, snapshotErr
	}
	t.Cleanup(func() { lstatValidatedAttachmentFile = originalLstat })

	err := validateExistingAttachmentFile(filePath, int64(len(content)), hashText)
	require.Error(err)
	require.ErrorIs(err, snapshotErr)
	assert.Contains(err.Error(), "lstat attachment file before validation")
}

func TestValidateExistingAttachmentFileRehashesReplacementWithSameMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	content := []byte("expected replacement bytes")
	corrupt := []byte("corrupt! replacement bytes")
	require.Len(corrupt, len(content))
	hash := sha256.Sum256(content)
	hashText := hex.EncodeToString(hash[:])
	canonical := filepath.Join(dir, hashText)
	require.NoError(os.WriteFile(canonical, content, 0o600))
	originalInfo, err := os.Lstat(canonical)
	require.NoError(err)
	displaced := canonical + ".displaced"
	originalLstat := lstatValidatedAttachmentFile
	var canonicalLstats int
	lstatValidatedAttachmentFile = func(path string) (os.FileInfo, error) {
		if filepath.Clean(path) == filepath.Clean(canonical) {
			canonicalLstats++
			if canonicalLstats == 2 {
				require.NoError(os.Rename(canonical, displaced))
				require.NoError(os.WriteFile(canonical, corrupt, 0o600))
				require.NoError(os.Chtimes(canonical, originalInfo.ModTime(), originalInfo.ModTime()))
			}
		}
		return originalLstat(path)
	}
	t.Cleanup(func() { lstatValidatedAttachmentFile = originalLstat })

	err = validateExistingAttachmentFile(canonical, int64(len(content)), hashText)
	require.Error(err)
	assert.Contains(err.Error(), "has hash")
	require.NotErrorIs(err, errAttachmentFileIdentityChanged)
	assert.Equal(3, canonicalLstats, "the replacement should fail during the retry hash")
}

func TestValidateExistingAttachmentFileIdentityChurnExhaustsRetries(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	content := []byte("ordinary validation eventually rejects unbounded identity churn")
	hash := sha256.Sum256(content)
	hashText := hex.EncodeToString(hash[:])
	canonical := filepath.Join(dir, hashText)
	require.NoError(os.WriteFile(canonical, content, 0o600))
	originalLstat := lstatValidatedAttachmentFile
	var canonicalLstats int
	lstatValidatedAttachmentFile = func(path string) (os.FileInfo, error) {
		if filepath.Clean(path) == filepath.Clean(canonical) {
			canonicalLstats++
			if canonicalLstats%2 == 0 {
				displaced := fmt.Sprintf("%s.displaced-%d", canonical, canonicalLstats)
				require.NoError(os.Rename(canonical, displaced))
				require.NoError(os.WriteFile(canonical, content, 0o600))
			}
		}
		return originalLstat(path)
	}
	t.Cleanup(func() { lstatValidatedAttachmentFile = originalLstat })

	err := validateExistingAttachmentFile(canonical, int64(len(content)), hashText)
	require.Error(err)
	assert.Contains(err.Error(), "identity")
	require.ErrorIs(err, errAttachmentFileIdentityChanged)
	assert.Equal(10, canonicalLstats)
	got, readErr := os.ReadFile(canonical)
	require.NoError(readErr)
	assert.Equal(content, got)
}
