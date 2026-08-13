package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

func TestStoreAttachment_InvalidContentHash_ReturnsError(t *testing.T) {
	require := require.New(t)
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
	assert.Error(t, statErr,
		"attachments dir should not have been created for invalid content hash")
}

func TestStoreAttachment_ComputesContentHashWhenMissing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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

func TestStoreAttachmentPreservesMIMEOccurrenceEvidence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())

	src, err := st.GetOrCreateSource("mbox", "me@example.com")
	require.NoError(err)
	convID, err := st.EnsureConversation(src.ID, "thread-role", "Thread")
	require.NoError(err)
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID: convID, SourceID: src.ID,
		SourceMessageID: "msg-role", MessageType: "email",
	})
	require.NoError(err)

	attachmentsDir := filepath.Join(tmp, "attachments")
	for _, att := range []*mime.Attachment{
		{
			Filename: "report.pdf", ContentType: "application/pdf",
			Content: []byte("standalone"), Disposition: "attachment", PartKey: "mime:1",
		},
		{
			Filename: "signature.png", ContentType: "image/png",
			Content: []byte("inline"), Disposition: "inline", IsInline: true,
			ContentID: "signature-1", PartKey: "mime:2",
		},
	} {
		require.NoError(storeAttachment(st, attachmentsDir, msgID, att))
	}

	rows, err := st.DB().Query(`
		SELECT attachment_role, role_source, source_part_key, COALESCE(content_id, '')
		FROM attachments WHERE message_id = ? ORDER BY source_part_key`, msgID)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	type evidence struct{ role, source, partKey, contentID string }
	var got []evidence
	for rows.Next() {
		var item evidence
		require.NoError(rows.Scan(&item.role, &item.source, &item.partKey, &item.contentID))
		got = append(got, item)
	}
	require.NoError(rows.Err())
	assert.Equal([]evidence{
		{role: "standalone", source: "mime_disposition", partKey: "mime:1"},
		{role: "inline", source: "mime_disposition", partKey: "mime:2", contentID: "signature-1"},
	}, got)
}

func TestStoreAttachment_StatError_DoesNotUpsertRow(t *testing.T) {
	require := require.New(t)
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
	assert.Equal(t, 0, count)
}
