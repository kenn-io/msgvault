package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

func TestStoreAttachment_InvalidContentHash_ReturnsError(t *testing.T) {
	require := requirepkg.New(t)
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "init schema")

	attachmentsDir := filepath.Join(tmp, "attachments")

	att := &mime.Attachment{
		Filename:    "a.txt",
		ContentType: "text/plain",
		Content:     []byte("hi"),
		ContentHash: "a", // malformed
		Size:        2,
	}

	err = storeAttachment(st, attachmentsDir, 1, att)
	require.Error(err)

	// Ensure nothing was written.
	_, statErr := os.Stat(attachmentsDir)
	assertpkg.Error(t, statErr,
		"attachments dir should not have been created for invalid content hash")
}

func TestStoreAttachment_ComputesContentHashWhenMissing(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "init schema")

	src, err := st.GetOrCreateSource("mbox", "me@example.com")
	require.NoError(err, "get/create source")
	convID, err := st.EnsureConversation(src.ID, "thread1", "Thread")
	require.NoError(err, "ensure conversation")
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "msg1",
		MessageType:     "email",
	})
	require.NoError(err, "upsert message")

	attachmentsDir := filepath.Join(tmp, "attachments")

	att := &mime.Attachment{
		Filename:    "a.txt",
		ContentType: "text/plain",
		Content:     []byte("hi"),
		ContentHash: "", // missing
		Size:        2,
	}

	require.NoError(storeAttachment(st, attachmentsDir, msgID, att), "storeAttachment")
	assert.NotEmpty(att.ContentHash, "expected ContentHash to be computed")

	// Ensure file + DB record exist.
	fullPath := filepath.Join(attachmentsDir, att.ContentHash[:2], att.ContentHash)
	_, err = os.Stat(fullPath)
	require.NoError(err, "attachment file missing")

	var count int
	err = st.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`, msgID).Scan(&count)
	require.NoError(err, "count attachments")
	assert.Equal(1, count)
}

func TestStoreAttachment_StatError_DoesNotUpsertRow(t *testing.T) {
	require := requirepkg.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink support")
	}

	tmp := t.TempDir()

	dbPath := filepath.Join(tmp, "msgvault.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "init schema")

	src, err := st.GetOrCreateSource("mbox", "me@example.com")
	require.NoError(err, "get/create source")
	convID, err := st.EnsureConversation(src.ID, "thread1", "Thread")
	require.NoError(err, "ensure conversation")
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "msg1",
		MessageType:     "email",
	})
	require.NoError(err, "upsert message")

	attachmentsDir := filepath.Join(tmp, "attachments")

	content := []byte("hi")
	sum := sha256.Sum256(content)
	contentHash := hex.EncodeToString(sum[:])
	fullPath := filepath.Join(attachmentsDir, contentHash[:2], contentHash)

	require.NoError(os.MkdirAll(filepath.Dir(fullPath), 0700), "mkdir")
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

	require.Error(storeAttachment(st, attachmentsDir, msgID, att))

	var count int
	err = st.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`, msgID).Scan(&count)
	require.NoError(err, "count attachments")
	assertpkg.Equal(t, 0, count)
}
