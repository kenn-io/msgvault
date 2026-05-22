package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

func TestStoreAttachment_InvalidContentHash_ReturnsError(t *testing.T) {
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	attachmentsDir := filepath.Join(tmp, "attachments")

	att := &mime.Attachment{
		Filename:    "a.txt",
		ContentType: "text/plain",
		Content:     []byte("hi"),
		ContentHash: "a", // malformed
		Size:        2,
	}

	err = storeAttachment(st, attachmentsDir, 1, att)
	if err == nil {
		t.Fatalf("expected error")
	}

	// Ensure nothing was written.
	if _, statErr := os.Stat(attachmentsDir); statErr == nil {
		t.Fatalf("attachments dir should not have been created for invalid content hash")
	}
}

func TestStoreAttachment_ComputesContentHashWhenMissing(t *testing.T) {
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	src, err := st.GetOrCreateSource("mbox", "me@example.com")
	if err != nil {
		t.Fatalf("get/create source: %v", err)
	}
	convID, err := st.EnsureConversation(src.ID, "thread1", "Thread")
	if err != nil {
		t.Fatalf("ensure conversation: %v", err)
	}
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "msg1",
		MessageType:     "email",
	})
	if err != nil {
		t.Fatalf("upsert message: %v", err)
	}

	attachmentsDir := filepath.Join(tmp, "attachments")

	att := &mime.Attachment{
		Filename:    "a.txt",
		ContentType: "text/plain",
		Content:     []byte("hi"),
		ContentHash: "", // missing
		Size:        2,
	}

	if err := storeAttachment(st, attachmentsDir, msgID, att); err != nil {
		t.Fatalf("storeAttachment: %v", err)
	}
	if att.ContentHash == "" {
		t.Fatalf("expected ContentHash to be computed")
	}

	// Ensure file + DB record exist.
	fullPath := filepath.Join(attachmentsDir, att.ContentHash[:2], att.ContentHash)
	if _, err := os.Stat(fullPath); err != nil {
		t.Fatalf("attachment file missing: %v", err)
	}

	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`, msgID).Scan(&count); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

func TestStoreAttachment_StatError_DoesNotUpsertRow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink support")
	}

	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	src, err := st.GetOrCreateSource("mbox", "me@example.com")
	if err != nil {
		t.Fatalf("get/create source: %v", err)
	}
	convID, err := st.EnsureConversation(src.ID, "thread1", "Thread")
	if err != nil {
		t.Fatalf("ensure conversation: %v", err)
	}
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "msg1",
		MessageType:     "email",
	})
	if err != nil {
		t.Fatalf("upsert message: %v", err)
	}

	attachmentsDir := filepath.Join(tmp, "attachments")

	content := []byte("hi")
	sum := sha256.Sum256(content)
	contentHash := hex.EncodeToString(sum[:])
	fullPath := filepath.Join(attachmentsDir, contentHash[:2], contentHash)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(fullPath, fullPath); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	att := &mime.Attachment{
		Filename:    "a.txt",
		ContentType: "text/plain",
		Content:     content,
		ContentHash: contentHash,
		Size:        len(content),
	}

	if err := storeAttachment(st, attachmentsDir, msgID, att); err == nil {
		t.Fatalf("expected error")
	}

	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`, msgID).Scan(&count); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}
