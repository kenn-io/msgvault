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
	if err := os.MkdirAll(filepath.Dir(fullPath), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(fullPath, []byte("jello"), 0600); err != nil { // same size as "hello"
		t.Fatalf("write corrupt file: %v", err)
	}

	att := &mime.Attachment{
		Filename:    "a.txt",
		ContentType: "text/plain",
		Size:        len(content),
		ContentHash: hash,
		Content:     content,
	}
	_, err := StoreAttachmentFile(tmp, att)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "hash") {
		t.Fatalf("expected hash mismatch error, got %v", err)
	}
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
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("expected mismatch error, got %v", err)
	}

	if _, err := os.Stat(filepath.Join(tmp, badHash[:2], badHash)); !os.IsNotExist(err) {
		t.Fatalf("unexpected file at provided hash path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, hash[:2], hash)); !os.IsNotExist(err) {
		t.Fatalf("unexpected file at computed hash path: %v", err)
	}
}

func TestStoreAttachmentFile_ProvidedContentHashUppercase_AcceptedAndCanonicalized(t *testing.T) {
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
	if err != nil {
		t.Fatalf("StoreAttachmentFile: %v", err)
	}

	wantStoragePath := path.Join(hash[:2], hash)
	if gotStoragePath != wantStoragePath {
		t.Fatalf("storage path mismatch: got %q, want %q", gotStoragePath, wantStoragePath)
	}
	if att.ContentHash != hash {
		t.Fatalf("ContentHash not canonicalized: got %q, want %q", att.ContentHash, hash)
	}
	if _, err := os.Stat(filepath.Join(tmp, hash[:2], hash)); err != nil {
		t.Fatalf("attachment file missing: %v", err)
	}
}

func TestStoreAttachmentFile_ConcurrentWriters_SameHash_NoError(t *testing.T) {
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
	for i := 0; i < n; i++ {
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

	for i := 0; i < n; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("store: %v", err)
		}
	}

	wantStoragePath := path.Join(hash[:2], hash)
	for i := 0; i < n; i++ {
		if got := <-pathCh; got != wantStoragePath {
			t.Fatalf("storage path mismatch: got %q, want %q", got, wantStoragePath)
		}
	}

	fullPath := filepath.Join(tmp, hash[:2], hash)
	b, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	gotSum := sha256.Sum256(b)
	gotHash := hex.EncodeToString(gotSum[:])
	if gotHash != hash {
		t.Fatalf("stored file hash mismatch: got %q, want %q", gotHash, hash)
	}
}
