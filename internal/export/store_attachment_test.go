package export

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
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
	requirepkg.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0700), "mkdir")
	requirepkg.NoError(t, os.WriteFile(fullPath, []byte("jello"), 0600), "write corrupt file") // same size as "hello"

	att := &mime.Attachment{
		Filename:    "a.txt",
		ContentType: "text/plain",
		Size:        len(content),
		ContentHash: hash,
		Content:     content,
	}
	_, err := StoreAttachmentFile(tmp, att)
	requirepkg.ErrorContains(t, err, "hash", "expected hash mismatch error")
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
	requirepkg.ErrorContains(t, err, "mismatch", "expected mismatch error")

	_, err = os.Stat(filepath.Join(tmp, badHash[:2], badHash))
	assertpkg.True(t, os.IsNotExist(err), "unexpected file at provided hash path: %v", err)
	_, err = os.Stat(filepath.Join(tmp, hash[:2], hash))
	assertpkg.True(t, os.IsNotExist(err), "unexpected file at computed hash path: %v", err)
}

func TestStoreAttachmentFile_ProvidedContentHashUppercase_AcceptedAndCanonicalized(t *testing.T) {
	require := requirepkg.New(t)
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
	require := requirepkg.New(t)
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
