package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestGetFileMetadataBatchUsesTransactionalAttachmentAuthority(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	localMessageID := f.CreateMessage("file-local")
	requirements.NoError(f.Store.UpsertAttachment(
		localMessageID, "report.pdf", "application/pdf", "aa/report", "localhash", 2048,
	))
	urlMessageID := f.CreateMessage("file-url")
	_, err := f.Store.DB().Exec(f.Store.Rebind(`
		INSERT INTO attachments
			(message_id, filename, mime_type, storage_path, content_hash, size, created_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		urlMessageID, "reference.html", "text/html", "https://example.com/reference", "stalehash", 31,
	)
	requirements.NoError(err)

	rows, err := f.Store.DB().Query(f.Store.Rebind(`
		SELECT id FROM attachments WHERE message_id IN (?, ?) ORDER BY id`), localMessageID, urlMessageID)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()
	var ids []int64
	for rows.Next() {
		var id int64
		requirements.NoError(rows.Scan(&id))
		ids = append(ids, id)
	}
	requirements.NoError(rows.Err())
	requirements.Len(ids, 2)

	files, err := f.Store.GetFileMetadataBatch(t.Context(), []int64{ids[1], ids[0], ids[1]})
	requirements.NoError(err)
	requirements.Len(files, 2)
	assertions.Equal(localMessageID, files[ids[0]].MessageID)
	assertions.Equal(f.ConvID, files[ids[0]].ConversationID)
	assertions.Equal(f.Source.ID, files[ids[0]].SourceID)
	assertions.Equal("file-local", files[ids[0]].SourceMessageID)
	assertions.Equal("email", files[ids[0]].MessageType)
	assertions.Equal("email_thread", files[ids[0]].ConversationType)
	assertions.Equal("localhash", files[ids[0]].ContentHash)
	assertions.Equal("aa/report", files[ids[0]].StoragePath)
	assertions.Empty(files[ids[0]].URL)
	assertions.Equal("https://example.com/reference", files[ids[1]].URL)
	assertions.Empty(files[ids[1]].ContentHash)
	assertions.Empty(files[ids[1]].StoragePath)
}

func TestGetFileMetadataReturnsNotFoundWithoutError(t *testing.T) {
	f := storetest.New(t)
	file, err := f.Store.GetFileMetadata(t.Context(), 999999)
	require.NoError(t, err)
	assert.Nil(t, file)
}

func TestGetFileMetadataHidesAttachmentsOnDedupHiddenMessages(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	survivorID := f.CreateMessage("dedup-file-keep")
	hiddenID := f.CreateMessage("dedup-file-drop")
	requirements.NoError(f.Store.UpsertAttachment(
		survivorID, "shared.pdf", "application/pdf", "aa/shared", "sharedhash", 2048,
	))
	requirements.NoError(f.Store.UpsertAttachment(
		hiddenID, "shared.pdf", "application/pdf", "aa/shared", "sharedhash", 2048,
	))
	survivorFileID := singleAttachmentID(t, f, survivorID)
	hiddenFileID := singleAttachmentID(t, f, hiddenID)
	_, err := f.Store.MergeDuplicates(survivorID, []int64{hiddenID}, "dedup-file-batch")
	requirements.NoError(err)

	hidden, err := f.Store.GetFileMetadata(t.Context(), hiddenFileID)
	requirements.NoError(err)
	assertions.Nil(hidden, "attachment on a dedup-hidden message must not resolve")

	files, err := f.Store.GetFileMetadataBatch(t.Context(), []int64{survivorFileID, hiddenFileID})
	requirements.NoError(err)
	requirements.Len(files, 1)
	assertions.Equal(survivorID, files[survivorFileID].MessageID)
	assertions.Equal("sharedhash", files[survivorFileID].ContentHash)
}

func singleAttachmentID(t *testing.T, f *storetest.Fixture, messageID int64) int64 {
	t.Helper()
	var id int64
	err := f.Store.DB().QueryRow(f.Store.Rebind(
		"SELECT id FROM attachments WHERE message_id = ?"), messageID).Scan(&id)
	require.NoError(t, err, "look up attachment for message %d", messageID)
	return id
}
