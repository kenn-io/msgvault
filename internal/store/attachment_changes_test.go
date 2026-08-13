package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAttachmentChangeJournalCapturesOnlyRelevantCommittedMutations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("attachment-journal")
	firstID := createJournalAttachment(t, f, messageID, "part:1", "a")
	assert.Zero(attachmentChangeCount(t, f), "journal stays empty with no registered feature")

	consumer, created, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), "document-index/v1")
	require.NoError(err)
	assert.True(created)
	assert.Zero(consumer.BaselineSequence)
	_, created, err = f.Store.RegisterAttachmentChangeConsumer(t.Context(), "document-index/v1")
	require.NoError(err)
	assert.False(created)
	require.ErrorContains(
		func() error {
			_, listErr := f.Store.ListAttachmentChanges(t.Context(), "document-index/v1", 10)
			return listErr
		}(),
		"requires full reconciliation",
	)
	require.NoError(f.Store.CompleteAttachmentChangeReconciliation(
		t.Context(), consumer.ConsumerKey, consumer.BaselineSequence,
	))
	require.NoError(f.Store.CompleteAttachmentChangeReconciliation(
		t.Context(), consumer.ConsumerKey, consumer.BaselineSequence,
	), "concurrent completion of the same baseline must be idempotent")

	transaction, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	_, err = transaction.Exec(f.Store.Rebind(
		`UPDATE attachments SET filename = ? WHERE id = ?`), "rolled-back.pdf", firstID)
	require.NoError(err)
	require.NoError(transaction.Rollback())
	assert.Zero(attachmentChangeCount(t, f), "rolled-back trigger writes must roll back")

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET is_read = NOT is_read WHERE id = ?`), messageID)
	require.NoError(err)
	assert.Zero(attachmentChangeCount(t, f), "read state is not attachment lifecycle")

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE attachments SET filename = ? WHERE id = ?`), "changed.pdf", firstID)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM attachments WHERE id = ?`), firstID)
	require.NoError(err)

	changes, err := f.Store.ListAttachmentChanges(t.Context(), consumer.ConsumerKey, 10)
	require.NoError(err)
	require.Len(changes, 3)
	assert.Equal("attachment_update", changes[0].EventKind)
	assert.Equal(firstID, requireInt64Pointer(t, changes[0].OldAttachmentID))
	assert.Equal(firstID, requireInt64Pointer(t, changes[0].NewAttachmentID))
	assert.Equal("message_live_exit", changes[1].EventKind)
	assert.Equal(messageID, requireInt64Pointer(t, changes[1].OldMessageID))
	assert.Nil(changes[1].NewMessageID)
	assert.Equal("attachment_delete", changes[2].EventKind)
	assert.Equal(firstID, requireInt64Pointer(t, changes[2].OldAttachmentID))
	assert.Nil(changes[2].NewAttachmentID)
}

func TestAttachmentChangeConsumersPruneOnlySharedConsumedPrefix(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("attachment-consumers")
	for _, key := range []string{"document-index/v1", "visual-index/v1"} {
		consumer, created, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), key)
		require.NoError(err)
		require.True(created)
		require.NoError(f.Store.CompleteAttachmentChangeReconciliation(
			t.Context(), key, consumer.BaselineSequence,
		))
	}
	attachmentID := createJournalAttachment(t, f, messageID, "part:1", "b")
	secondAttachmentID := createJournalAttachment(t, f, messageID, "part:2", "c")

	documentChanges, err := f.Store.ListAttachmentChanges(t.Context(), "document-index/v1", 10)
	require.NoError(err)
	require.Len(documentChanges, 2)
	visualChanges, err := f.Store.ListAttachmentChanges(t.Context(), "visual-index/v1", 10)
	require.NoError(err)
	require.Len(visualChanges, 2)
	assert.Equal(documentChanges[0].Sequence, visualChanges[0].Sequence)
	assert.Equal(attachmentID, requireInt64Pointer(t, documentChanges[0].NewAttachmentID))
	assert.Equal(secondAttachmentID, requireInt64Pointer(t, documentChanges[1].NewAttachmentID))

	require.NoError(f.Store.AdvanceAttachmentChangeConsumer(
		t.Context(), "document-index/v1", documentChanges[1].Sequence,
	))
	require.NoError(f.Store.AdvanceAttachmentChangeConsumer(
		t.Context(), "document-index/v1", documentChanges[0].Sequence,
	), "a slower concurrent caller may acknowledge an older sequence after a newer one")
	assert.Equal(2, attachmentChangeCount(t, f), "slower consumer retains its events")
	require.NoError(f.Store.AdvanceAttachmentChangeConsumer(
		t.Context(), "visual-index/v1", visualChanges[1].Sequence,
	))
	assert.Zero(attachmentChangeCount(t, f))

	require.NoError(f.Store.UnregisterAttachmentChangeConsumer(t.Context(), "document-index/v1"))
	require.NoError(f.Store.UnregisterAttachmentChangeConsumer(t.Context(), "visual-index/v1"))
	createJournalAttachment(t, f, messageID, "part:3", "d")
	assert.Zero(attachmentChangeCount(t, f), "capture stops after the final consumer unregisters")
}

func TestAttachmentChangeJournalCapturesCascadeDeletion(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("attachment-cascade")
	attachmentID := createJournalAttachment(t, f, messageID, "part:1", "d")
	consumer, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), "document-index/v1")
	require.NoError(err)
	require.NoError(f.Store.CompleteAttachmentChangeReconciliation(
		t.Context(), consumer.ConsumerKey, consumer.BaselineSequence,
	))

	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM messages WHERE id = ?`), messageID)
	require.NoError(err)
	changes, err := f.Store.ListAttachmentChanges(t.Context(), consumer.ConsumerKey, 10)
	require.NoError(err)
	require.Len(changes, 1)
	assert.Equal(t, "attachment_delete", changes[0].EventKind)
	assert.Equal(t, attachmentID, requireInt64Pointer(t, changes[0].OldAttachmentID))
}

func createJournalAttachment(
	t *testing.T,
	f *storetest.Fixture,
	messageID int64,
	partKey string,
	hashCharacter string,
) int64 {
	t.Helper()
	hash := strings.Repeat(hashCharacter, 64)
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: 64,
		StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: partKey,
	}))
	var id int64
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT id FROM attachments WHERE message_id = ? AND source_part_key = ?`), messageID, partKey).Scan(&id))
	return id
}

func attachmentChangeCount(t *testing.T, f *storetest.Fixture) int {
	t.Helper()
	var count int
	require.NoError(t, f.Store.DB().QueryRow(`SELECT COUNT(*) FROM attachment_change_log`).Scan(&count))
	return count
}

func requireInt64Pointer(t *testing.T, value *int64) int64 {
	t.Helper()
	require.NotNil(t, value)
	return *value
}
