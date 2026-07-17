package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

var sampleRawMessage = []byte("From: test@example.com\r\nSubject: Test\r\n\r\nBody")

func TestStore_Open(t *testing.T) {
	st := testutil.NewTestStore(t)

	// Store should be usable
	assert.NotNil(t, st.DB(), "DB() returned nil")
}

func TestStore_GetStats_Empty(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	stats, err := st.GetStats()
	require.NoError(t, err, "GetStats()")

	assert.Equal(int64(0), stats.MessageCount, "MessageCount")
	assert.Equal(int64(0), stats.ThreadCount, "ThreadCount")
	assert.Equal(int64(0), stats.SourceCount, "SourceCount")
}

func TestStore_Source_CreateAndGet(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	// Create source
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(err, "GetOrCreateSource()")

	assert.NotZero(source.ID, "source ID should be non-zero")
	assert.Equal("gmail", source.SourceType, "SourceType")
	assert.Equal("test@example.com", source.Identifier, "Identifier")

	// Get same source again (should return existing)
	source2, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(err, "GetOrCreateSource() second call")

	assert.Equal(source.ID, source2.ID, "second call ID")
}

func TestStore_Source_UpdateSyncCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	err := f.Store.UpdateSourceSyncCursor(f.Source.ID, "12345")
	require.NoError(err, "UpdateSourceSyncCursor()")

	// Verify cursor was updated
	updated, err := f.Store.GetSourceByIdentifier("test@example.com")
	require.NoError(err, "GetSourceByIdentifier()")

	assert.True(updated.SyncCursor.Valid, "SyncCursor should be valid")
	assert.Equal("12345", updated.SyncCursor.String, "SyncCursor")
}

func TestStore_Source_UpdateDisplayName(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	err := f.Store.UpdateSourceDisplayName(f.Source.ID, "Work Account")
	require.NoError(err, "UpdateSourceDisplayName()")

	// Verify display name was updated
	updated, err := f.Store.GetSourceByIdentifier("test@example.com")
	require.NoError(err, "GetSourceByIdentifier()")

	assert.True(updated.DisplayName.Valid, "DisplayName should be valid")
	assert.Equal("Work Account", updated.DisplayName.String, "DisplayName")
}

func TestStore_ListSources(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	sources, err := f.Store.ListSources("")
	require.NoError(err, "ListSources()")

	require.Len(sources, 1)
	assert.Equal("test@example.com", sources[0].Identifier, "Identifier")
	assert.Equal(f.Source.ID, sources[0].ID, "ID")
}

func TestStore_SourceImportItems(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	src, err := f.Store.GetOrCreateSource("synctech_sms", "+15550000001")
	require.NoError(err, "GetOrCreateSource")

	item := store.SourceImportItem{
		SourceID:   src.ID,
		Provider:   "drive",
		ProviderID: "drive-file-1",
		Name:       "sms-2024.xml",
		Checksum:   "abc123",
		Size:       42,
		Status:     "imported",
	}
	require.NoError(f.Store.UpsertSourceImportItem(item), "UpsertSourceImportItem")

	got, err := f.Store.GetSourceImportItem(src.ID, "drive", "drive-file-1")
	require.NoError(err, "GetSourceImportItem")
	require.NotNil(got, "GetSourceImportItem returned nil")
	assert.Equal("sms-2024.xml", got.Name, "Name")
	assert.Equal("abc123", got.Checksum, "Checksum")
	assert.Equal("imported", got.Status, "Status")

	item.Size = 99
	item.Status = "failed"
	item.ErrorMessage = sql.NullString{String: "boom", Valid: true}
	require.NoError(f.Store.UpsertSourceImportItem(item), "UpsertSourceImportItem update")
	got, err = f.Store.GetSourceImportItem(src.ID, "drive", "drive-file-1")
	require.NoError(err, "GetSourceImportItem after update")
	assert.Equal(int64(99), got.Size, "updated Size")
	assert.Equal("failed", got.Status, "updated Status")
	assert.Equal("boom", got.ErrorMessage.String, "ErrorMessage")

	checksums, err := f.Store.ListImportedSourceItemChecksums(src.ID, "drive")
	require.NoError(err, "ListImportedSourceItemChecksums")
	assert.Empty(checksums["drive-file-1"], "failed item should not be returned as imported")
	item.Status = "imported"
	require.NoError(f.Store.UpsertSourceImportItem(item), "UpsertSourceImportItem imported")
	checksums, err = f.Store.ListImportedSourceItemChecksums(src.ID, "drive")
	require.NoError(err, "ListImportedSourceItemChecksums imported")
	assert.Equal("abc123", checksums["drive-file-1"], "imported checksum")

	_, err = f.Store.DB().Exec(
		f.Store.Rebind(`UPDATE source_import_items SET checksum = NULL WHERE source_id = ? AND provider = ? AND provider_id = ?`),
		src.ID, "drive", "drive-file-1",
	)
	require.NoError(err, "set checksum NULL")
	got, err = f.Store.GetSourceImportItem(src.ID, "drive", "drive-file-1")
	require.NoError(err, "GetSourceImportItem with NULL checksum")
	assert.Empty(got.Checksum, "NULL checksum should surface as empty string")
	checksums, err = f.Store.ListImportedSourceItemChecksums(src.ID, "drive")
	require.NoError(err, "ListImportedSourceItemChecksums with NULL checksum")
	assert.Empty(checksums["drive-file-1"], "NULL imported checksum should surface as empty string")
}

func TestStore_Conversation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	// Create conversation
	convID, err := f.Store.EnsureConversation(f.Source.ID, "thread-123", "Test Thread")
	require.NoError(err, "EnsureConversation()")

	assert.NotZero(convID, "conversation ID should be non-zero")

	// Get same conversation (should return existing)
	convID2, err := f.Store.EnsureConversation(f.Source.ID, "thread-123", "Test Thread")
	require.NoError(err, "EnsureConversation() second call")

	assert.Equal(convID, convID2, "second call ID")
}

func TestStore_EnsureParticipantByIdentifierAllowsShortCode(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	id, err := f.Store.EnsureParticipantByIdentifier("synctech_sms", "12345", "Bank alerts")
	require.NoError(err, "EnsureParticipantByIdentifier")
	require.NotZero(id, "participant id is zero")
	id2, err := f.Store.EnsureParticipantByIdentifier("synctech_sms", "12345", "Bank alerts")
	require.NoError(err, "EnsureParticipantByIdentifier second")
	assert.Equal(t, id, id2, "id2")
}

func TestStore_UpsertMessage(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name string
		msg  func(sourceID, convID int64) *store.Message
	}{
		{
			name: "AllFields",
			msg: func(sourceID, convID int64) *store.Message {
				return storetest.NewMessage(sourceID, convID).
					WithSourceMessageID("msg-all-fields").
					WithSubject("Full Subject").
					WithSnippet("Preview snippet").
					WithSize(2048).
					WithSentAt(now).
					WithReceivedAt(now.Add(time.Second)).
					WithInternalDate(now).
					WithAttachmentCount(2).
					WithIsFromMe(true).
					Build()
			},
		},
		{
			name: "MinimalFields",
			msg: func(sourceID, convID int64) *store.Message {
				return storetest.NewMessage(sourceID, convID).
					WithSourceMessageID("msg-minimal").
					WithSize(0).
					Build()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := storetest.New(t)
			msg := tt.msg(f.Source.ID, f.ConvID)

			// Insert
			msgID, err := f.Store.UpsertMessage(msg)
			require.NoError(err, "UpsertMessage insert")
			assert.NotZero(msgID, "message ID should be non-zero")

			// Update: mutate fields and upsert again
			msg.Subject = sql.NullString{String: "Updated Subject", Valid: true}
			msg.Snippet = sql.NullString{String: "Updated snippet", Valid: true}
			msg.HasAttachments = !msg.HasAttachments
			msgID2, err := f.Store.UpsertMessage(msg)
			require.NoError(err, "UpsertMessage update")
			assert.Equal(msgID, msgID2, "update ID")

			// Verify updated fields are persisted
			got := f.GetMessageFields(msgID)
			assert.Equal("Updated Subject", got.Subject, "subject")
			assert.Equal("Updated snippet", got.Snippet, "snippet")
			assert.Equal(msg.HasAttachments, got.HasAttachments, "has_attachments")

			// Verify stats show exactly one message
			stats, err := f.Store.GetStats()
			require.NoError(err, "GetStats")
			assert.Equal(int64(1), stats.MessageCount, "MessageCount")
		})
	}
}

func TestStore_MessageExistsBatch(t *testing.T) {
	assert := assert.New(t)
	f := storetest.New(t)

	// Insert some messages
	ids := []string{"msg-1", "msg-2", "msg-3"}
	for _, id := range ids {
		f.CreateMessage(id)
	}

	// Check which exist
	checkIDs := []string{"msg-1", "msg-2", "msg-4", "msg-5"}
	existing, err := f.Store.MessageExistsBatch(f.Source.ID, checkIDs)
	require.NoError(t, err, "MessageExistsBatch()")

	assert.Len(existing, 2)
	assert.Contains(existing, "msg-1")
	assert.Contains(existing, "msg-2")
	assert.NotContains(existing, "msg-4")
}

func TestStore_MessageMetadataBatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	withMetadataID := f.CreateMessage("with-metadata")
	withoutMetadataID := f.CreateMessage("without-metadata")
	require.NoError(f.Store.SetMessageMetadata(withMetadataID, sql.NullString{
		String: `{"transcript_state":"pending"}`,
		Valid:  true,
	}))

	got, err := f.Store.MessageMetadataBatch(f.Source.ID, []string{
		"with-metadata", "without-metadata", "missing",
	})
	require.NoError(err)
	require.Len(got, 2)
	assert.Equal(withMetadataID, got["with-metadata"].ID)
	assert.JSONEq(`{"transcript_state":"pending"}`, got["with-metadata"].Metadata.String)
	assert.True(got["with-metadata"].Metadata.Valid)
	assert.Equal(withoutMetadataID, got["without-metadata"].ID)
	assert.False(got["without-metadata"].Metadata.Valid)
	assert.NotContains(got, "missing")
}

func TestStore_MessageRaw(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-1")

	// Store raw data
	err := f.Store.UpsertMessageRaw(msgID, sampleRawMessage)
	require.NoError(t, err, "UpsertMessageRaw()")

	// Retrieve raw data
	retrieved, err := f.Store.GetMessageRaw(msgID)
	require.NoError(t, err, "GetMessageRaw()")

	assert.Equal(t, string(sampleRawMessage), string(retrieved), "retrieved data")
}

func TestStore_Participant(t *testing.T) {
	f := storetest.New(t)

	// Create participant
	pid := f.EnsureParticipant("alice@example.com", "Alice Smith", "example.com")
	assert.NotZero(t, pid, "participant ID should be non-zero")

	// Get same participant (should return existing)
	pid2 := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	assert.Equal(t, pid, pid2, "second call ID")
}

func TestStore_EnsureParticipantsBatch(t *testing.T) {
	assert := assert.New(t)
	f := storetest.New(t)

	addresses := []mime.Address{
		{Email: "alice@example.com", Name: "Alice", Domain: "example.com"},
		{Email: "bob@example.org", Name: "Bob", Domain: "example.org"},
		{Email: "", Name: "No Email", Domain: ""}, // Should be skipped
	}

	result, err := f.Store.EnsureParticipantsBatch(addresses)
	require.NoError(t, err, "EnsureParticipantsBatch()")

	assert.Len(result, 2)
	assert.Contains(result, "alice@example.com")
	assert.Contains(result, "bob@example.org")
}

func TestStore_Label(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	// Create label
	lid, err := f.Store.EnsureLabel(f.Source.ID, "INBOX", "Inbox", "system")
	require.NoError(err, "EnsureLabel()")

	assert.NotZero(lid, "label ID should be non-zero")

	// Get same label
	lid2, err := f.Store.EnsureLabel(f.Source.ID, "INBOX", "Inbox", "system")
	require.NoError(err, "EnsureLabel() second call")

	assert.Equal(lid, lid2, "second call ID")
}

func TestStore_EnsureLabelUpdatesTypeWhenNameUnchanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	lid, err := f.Store.EnsureLabel(f.Source.ID, "Sent Messages", "Sent Messages", "user")
	require.NoError(err, "create stale sent label")

	lid2, err := f.Store.EnsureLabel(f.Source.ID, "Sent Messages", "Sent Messages", "system")
	require.NoError(err, "refresh sent label type")

	assert.Equal(lid, lid2, "label ID")

	var labelType string
	err = f.Store.DB().QueryRow(
		f.Store.Rebind(`SELECT label_type FROM labels WHERE id = ?`), lid,
	).Scan(&labelType)
	require.NoError(err, "select label type")
	assert.Equal("system", labelType, "label type")
}

func TestStore_EnsureLabelUpdatesNullTypeWhenNameUnchanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	lid, err := f.Store.EnsureLabel(f.Source.ID, "Sent Messages", "Sent Messages", "user")
	require.NoError(err, "create stale sent label")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE labels SET label_type = NULL WHERE id = ?`), lid)
	require.NoError(err, "clear label type")

	lid2, err := f.Store.EnsureLabel(f.Source.ID, "Sent Messages", "Sent Messages", "system")
	require.NoError(err, "refresh sent label type")

	assert.Equal(lid, lid2, "label ID")

	var labelType string
	err = f.Store.DB().QueryRow(
		f.Store.Rebind(`SELECT label_type FROM labels WHERE id = ?`), lid,
	).Scan(&labelType)
	require.NoError(err, "select label type")
	assert.Equal("system", labelType, "label type")
}

func TestStore_EnsureLabel_NameConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	// Create label L1 with name "Work"
	lid1, err := f.Store.EnsureLabel(f.Source.ID, "L1", "Work", "user")
	require.NoError(err, "create L1")

	// Insert a different label L2 with the same name — should upsert,
	// not crash with UNIQUE constraint violation (#232).
	lid2, err := f.Store.EnsureLabel(f.Source.ID, "L2", "Work", "user")
	require.NoError(err, "create L2 with same name")

	assert.Equal(lid1, lid2, "L2 ID (should reuse existing row)")

	// L1's source_label_id was overwritten — looking up L2 should work
	lid2Again, err := f.Store.EnsureLabel(f.Source.ID, "L2", "Work", "user")
	require.NoError(err, "re-lookup L2")
	assert.Equal(lid1, lid2Again, "re-lookup L2 ID")
}

func TestStore_EnsureLabel_Rename(t *testing.T) {
	f := storetest.New(t)

	// Create label L1 with name "OldName"
	lid, err := f.Store.EnsureLabel(f.Source.ID, "L1", "OldName", "user")
	require.NoError(t, err, "create L1")

	// Same source_label_id, different name (label was renamed in Gmail)
	lid2, err := f.Store.EnsureLabel(f.Source.ID, "L1", "NewName", "user")
	require.NoError(t, err, "rename L1")

	assert.Equal(t, lid, lid2, "renamed label ID")
}

func TestStore_EnsureLabel_RenameAndReuse(t *testing.T) {
	f := storetest.New(t)

	// Scenario: L1 named "Foo" is renamed to "Bar", then L2 takes "Foo".
	_, err := f.Store.EnsureLabel(f.Source.ID, "L1", "Foo", "user")
	require.NoError(t, err, "create L1=Foo")

	// L1 renamed to "Bar" — should update name in place
	_, err = f.Store.EnsureLabel(f.Source.ID, "L1", "Bar", "user")
	require.NoError(t, err, "rename L1=Bar")

	// New label L2 takes the old name "Foo" — should succeed
	_, err = f.Store.EnsureLabel(f.Source.ID, "L2", "Foo", "user")
	require.NoError(t, err, "create L2=Foo")
}

func TestStore_EnsureLabel_RenameSwap(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	// L1="Foo" exists, and L1 is renamed to the name of an existing
	// label "Bar" (which has source_label_id "L2"). The rename merges
	// L2 into L1 so L1 can take the name.
	lid1, err := f.Store.EnsureLabel(f.Source.ID, "L1", "Foo", "user")
	require.NoError(err, "create L1=Foo")

	lid2, err := f.Store.EnsureLabel(f.Source.ID, "L2", "Bar", "user")
	require.NoError(err, "create L2=Bar")

	// Tag a message with L2 so we can verify associations survive merge
	msgID := f.CreateMessage("swap-msg")
	err = f.Store.ReplaceMessageLabels(msgID, []int64{lid2})
	require.NoError(err, "tag message with L2")

	// L1 renamed to "Bar" — merges L2 into L1
	lid1After, err := f.Store.EnsureLabel(f.Source.ID, "L1", "Bar", "user")
	require.NoError(err, "rename L1=Bar (was L2's name)")

	assert.Equal(t, lid1, lid1After, "L1 ID")

	// Message should now be associated with L1 (not the deleted L2)
	f.AssertLabelCount(msgID, 1)
	f.AssertMessageHasLabel(msgID, lid1)
}

func TestStore_EnsureLabelsBatch(t *testing.T) {
	f := storetest.New(t)

	labels := map[string]store.LabelInfo{
		"INBOX":       {Name: "Inbox", Type: "system"},
		"SENT":        {Name: "Sent", Type: "system"},
		"Label_12345": {Name: "My Label", Type: "user"},
	}

	result, err := f.Store.EnsureLabelsBatch(f.Source.ID, labels)
	require.NoError(t, err, "EnsureLabelsBatch()")

	assert.Len(t, result, 3)
	for sourceLabelID := range labels {
		assert.Contains(t, result, sourceLabelID, "%s should be in result", sourceLabelID)
	}
}

func TestStore_EnsureLabelsBatch_CrossRename(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	// Initial state: L1="Foo", L2="Bar"
	initial := map[string]store.LabelInfo{
		"L1": {Name: "Foo", Type: "user"},
		"L2": {Name: "Bar", Type: "user"},
	}
	ids, err := f.Store.EnsureLabelsBatch(f.Source.ID, initial)
	require.NoError(err, "initial batch")

	l1ID, l2ID := ids["L1"], ids["L2"]

	// Tag messages with each label
	msg1 := f.CreateMessage("cross-1")
	msg2 := f.CreateMessage("cross-2")
	err = f.Store.ReplaceMessageLabels(msg1, []int64{l1ID})
	require.NoError(err, "tag msg1 with L1")
	err = f.Store.ReplaceMessageLabels(msg2, []int64{l2ID})
	require.NoError(err, "tag msg2 with L2")

	// Cross-rename: L1→"Bar", L2→"Foo"
	swapped := map[string]store.LabelInfo{
		"L1": {Name: "Bar", Type: "user"},
		"L2": {Name: "Foo", Type: "user"},
	}
	ids2, err := f.Store.EnsureLabelsBatch(f.Source.ID, swapped)
	require.NoError(err, "cross-rename batch")

	// IDs should be preserved
	assert.Equal(l1ID, ids2["L1"], "L1 id")
	assert.Equal(l2ID, ids2["L2"], "L2 id")

	// Each message should still be linked to its original label ID
	f.AssertLabelCount(msg1, 1)
	f.AssertMessageHasLabel(msg1, l1ID)
	f.AssertLabelCount(msg2, 1)
	f.AssertMessageHasLabel(msg2, l2ID)
}

func TestStore_MessageLabels(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-1")

	labels := f.EnsureLabels(map[string]string{
		"INBOX":   "Inbox",
		"STARRED": "Starred",
		"SENT":    "Sent",
	}, "system")

	// Set labels
	err := f.Store.ReplaceMessageLabels(msgID, []int64{labels["INBOX"], labels["STARRED"]})
	require.NoError(t, err, "ReplaceMessageLabels()")

	f.AssertLabelCount(msgID, 2)

	// Replace with different labels
	err = f.Store.ReplaceMessageLabels(msgID, []int64{labels["SENT"]})
	require.NoError(t, err, "ReplaceMessageLabels() replace")

	f.AssertLabelCount(msgID, 1)

	// Verify it's the right label
	labelID := f.GetSingleLabelID(msgID)
	assert.Equal(t, labels["SENT"], labelID, "label_id (SENT)")
}

func TestStore_MessageRecipients(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-1")

	pid1 := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	pid2 := f.EnsureParticipant("bob@example.org", "Bob", "example.org")

	// Set recipients
	err := f.Store.ReplaceMessageRecipients(msgID, "to", []int64{pid1, pid2}, []string{"Alice", "Bob"})
	require.NoError(t, err, "ReplaceMessageRecipients()")

	f.AssertRecipientCount(msgID, "to", 2)

	// Replace recipients
	err = f.Store.ReplaceMessageRecipients(msgID, "to", []int64{pid1}, []string{"Alice"})
	require.NoError(t, err, "ReplaceMessageRecipients() replace")

	f.AssertRecipientCount(msgID, "to", 1)

	// Verify it's the right recipient
	participantID := f.GetSingleRecipientID(msgID, "to")
	assert.Equal(t, pid1, participantID, "participant_id (alice)")
}

func TestStore_MarkMessageDeleted(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-1")

	f.AssertMessageNotDeleted(msgID)

	err := f.Store.MarkMessageDeleted(f.Source.ID, "msg-1")
	require.NoError(t, err, "MarkMessageDeleted()")

	f.AssertMessageDeleted(msgID)
}

func TestStore_Attachment(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	msgID := storetest.NewMessage(f.Source.ID, f.ConvID).
		WithSourceMessageID("msg-1").
		WithAttachmentCount(1).
		Create(t, f.Store)

	err := f.Store.UpsertAttachment(msgID, "document.pdf", "application/pdf", "/path/to/file", "abc123hash", 1024)
	require.NoError(err, "UpsertAttachment()")

	// Upsert same attachment (should not error, dedupe by content_hash)
	err = f.Store.UpsertAttachment(msgID, "document.pdf", "application/pdf", "/path/to/file", "abc123hash", 1024)
	require.NoError(err, "UpsertAttachment() duplicate")

	stats, err := f.Store.GetStats()
	require.NoError(err, "GetStats")
	assert.Equal(t, int64(1), stats.AttachmentCount, "AttachmentCount")
}

func TestStore_SyncRun(t *testing.T) {
	f := storetest.New(t)

	syncID := f.StartSync()
	f.AssertActiveSync(syncID, "running")
}

func TestStore_SyncCheckpoint(t *testing.T) {
	f := storetest.New(t)

	syncID := f.StartSync()

	cp := &store.Checkpoint{
		PageToken:         "next-page-token",
		MessagesProcessed: 100,
		MessagesAdded:     50,
		MessagesUpdated:   10,
		ErrorsCount:       2,
	}

	err := f.Store.UpdateSyncCheckpoint(syncID, cp)
	require.NoError(t, err, "UpdateSyncCheckpoint()")

	// Verify checkpoint was saved
	f.AssertActiveSync(syncID, "running")
	active, err := f.Store.GetActiveSync(f.Source.ID)
	require.NoError(t, err, "GetActiveSync")
	assert.Equal(t, int64(100), active.MessagesProcessed, "sync MessagesProcessed")
}

func TestStore_SyncComplete(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	syncID := f.StartSync()

	err := f.Store.CompleteSync(syncID, "history-12345")
	require.NoError(err, "CompleteSync()")

	f.AssertNoActiveSync()

	// Should have a successful sync
	lastSync, err := f.Store.GetLastSuccessfulSync(f.Source.ID)
	require.NoError(err, "GetLastSuccessfulSync()")
	require.NotNil(lastSync, "expected last successful sync, got nil")
	assert.Equal(t, "completed", lastSync.Status, "status")
}

func TestStore_SyncFail(t *testing.T) {
	f := storetest.New(t)

	syncID := f.StartSync()

	err := f.Store.FailSync(syncID, "network error")
	require.NoError(t, err, "FailSync()")

	f.AssertNoActiveSync()

	// Verify sync status is "failed" and error message is stored
	status, errorMsg := f.GetSyncRun(syncID)
	assert.Equal(t, "failed", status, "sync status")
	assert.Equal(t, "network error", errorMsg, "error_message")
}

func TestStore_GetMessage_DeletedMessageVisibleByID(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("deleted-msg-1")
	_, err := f.Store.DB().Exec(
		f.Store.Rebind("UPDATE messages SET deleted_from_source_at = ? WHERE id = ?"),
		time.Date(2026, 3, 18, 14, 30, 0, 0, time.UTC),
		msgID,
	)
	require.NoError(t, err, "mark deleted")

	msg, err := f.Store.GetMessage(msgID)
	require.NoError(t, err, "GetMessage()")
	require.NotNil(t, msg, "GetMessage() = nil, want deleted message")
}

func TestStore_MarkMessageDeletedByGmailID(t *testing.T) {
	f := storetest.New(t)

	f.CreateMessage("gmail-msg-123")

	// Mark as deleted (trash)
	err := f.Store.MarkMessageDeletedByGmailID(false, "gmail-msg-123")
	require.NoError(t, err, "MarkMessageDeletedByGmailID(trash)")

	// Mark as permanently deleted
	err = f.Store.MarkMessageDeletedByGmailID(true, "gmail-msg-123")
	require.NoError(t, err, "MarkMessageDeletedByGmailID(permanent)")

	// Non-existent message should not error (no rows affected is OK)
	err = f.Store.MarkMessageDeletedByGmailID(true, "nonexistent-id")
	require.NoError(t, err, "MarkMessageDeletedByGmailID(nonexistent)")
}

func TestStore_MarkMessagesDeletedByGmailIDBatch(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	// Create 600+ messages to exercise multi-chunk behavior (chunkSize=500)
	const count = 600
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("batch-del-%d", i)
		f.CreateMessage(ids[i])
	}

	// Mark all as deleted in one batch call
	err := f.Store.MarkMessagesDeletedByGmailIDBatch(ids)
	require.NoError(err, "MarkMessagesDeletedByGmailIDBatch")

	// Verify all are marked deleted
	var deletedCount int
	err = f.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NOT NULL`,
	).Scan(&deletedCount)
	require.NoError(err, "count deleted")
	assert.Equal(t, count, deletedCount, "deleted count")

	// Empty batch should be a no-op
	err = f.Store.MarkMessagesDeletedByGmailIDBatch(nil)
	require.NoError(err, "MarkMessagesDeletedByGmailIDBatch(nil)")
}

func TestStore_GetMessageRaw_NotFound(t *testing.T) {
	f := storetest.New(t)

	// Try to get raw for non-existent message
	_, err := f.Store.GetMessageRaw(99999)
	assert.Error(t, err, "GetMessageRaw() should error for non-existent message")
}

func TestStore_UpsertMessageRaw_Update(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-raw-update")

	// Insert raw data
	rawData1 := []byte("Original raw content")
	err := f.Store.UpsertMessageRaw(msgID, rawData1)
	require.NoError(err, "UpsertMessageRaw()")

	// Update with new raw data
	rawData2 := []byte("Updated raw content that is different")
	err = f.Store.UpsertMessageRaw(msgID, rawData2)
	require.NoError(err, "UpsertMessageRaw() update")

	// Verify updated data
	retrieved, err := f.Store.GetMessageRaw(msgID)
	require.NoError(err, "GetMessageRaw")
	assert.Equal(t, string(rawData2), string(retrieved), "retrieved")
}

func TestStore_UpsertMessageBody(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	msgID := f.CreateMessage("msg-body-test")

	// Insert body
	err := f.Store.UpsertMessageBody(msgID,
		sql.NullString{String: "hello text", Valid: true},
		sql.NullString{String: "<p>hello html</p>", Valid: true},
	)
	require.NoError(err, "UpsertMessageBody()")

	// Verify via helper
	bodyText, bodyHTML := f.GetMessageBody(msgID)
	assert.Equal("hello text", bodyText.String, "body_text")
	assert.Equal("<p>hello html</p>", bodyHTML.String, "body_html")

	// Update body (upsert)
	err = f.Store.UpsertMessageBody(msgID,
		sql.NullString{String: "updated text", Valid: true},
		sql.NullString{},
	)
	require.NoError(err, "UpsertMessageBody() update")
	bodyText, bodyHTML = f.GetMessageBody(msgID)
	assert.Equal("updated text", bodyText.String, "after update: body_text")
	// UpsertMessageBody overwrites both columns; invalid NullString NULLs the column
	assert.False(bodyHTML.Valid, "after update: body_html should be NULL, got %q", bodyHTML.String)
}

func TestStore_UpsertMessageBodyInvalidatesChangedFTS(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS is unavailable")
	}
	messageID := f.CreateMessage("msg-body-fts-invalidation")
	body := sql.NullString{String: "old indexed body", Valid: true}
	require.NoError(f.Store.UpsertMessageBody(messageID, body, sql.NullString{}),
		"store initial body")
	require.NoError(f.Store.UpsertFTS(
		messageID, "subject", body.String, "", "", "",
	), "index initial body")

	// An idempotent body write must not create needless backfill work.
	require.NoError(f.Store.UpsertMessageBody(messageID, body, sql.NullString{}),
		"store unchanged body")
	if f.Store.IsPostgreSQL() {
		var searchIsNull bool
		require.NoError(f.Store.DB().QueryRow(
			"SELECT search_fts IS NULL FROM messages WHERE id = $1", messageID,
		).Scan(&searchIsNull), "check unchanged PostgreSQL index")
		assert.False(searchIsNull, "unchanged body must retain its FTS document")
	} else {
		var count int
		require.NoError(f.Store.DB().QueryRow(
			"SELECT COUNT(*) FROM messages_fts WHERE rowid = ?", messageID,
		).Scan(&count), "check unchanged SQLite index")
		assert.Equal(1, count, "unchanged body must retain its FTS document")
	}

	require.NoError(f.Store.UpsertMessageBody(messageID,
		sql.NullString{},
		sql.NullString{String: "<p>old indexed body</p>", Valid: true}),
		"replace indexed text with equivalent HTML-only body")
	if f.Store.IsPostgreSQL() {
		var searchIsNull, versionIsNull bool
		require.NoError(f.Store.DB().QueryRow(`
			SELECT search_fts IS NULL, indexing_version IS NULL
			FROM messages WHERE id = $1
		`, messageID).Scan(&searchIsNull, &versionIsNull), "check PostgreSQL invalidation")
		assert.True(searchIsNull, "changed body must clear its old FTS document")
		assert.True(versionIsNull, "changed body must require a fresh index version")
	} else {
		var count int
		require.NoError(f.Store.DB().QueryRow(
			"SELECT COUNT(*) FROM messages_fts WHERE rowid = ?", messageID,
		).Scan(&count), "check SQLite invalidation")
		assert.Zero(count, "changed body must delete its old FTS document")
	}
}

func TestStore_MessageExistsBatch_Empty(t *testing.T) {
	f := storetest.New(t)

	// Check with empty list
	result, err := f.Store.MessageExistsBatch(f.Source.ID, []string{})
	require.NoError(t, err, "MessageExistsBatch(empty)")
	assert.Empty(t, result)
}

func TestStore_MessageMetadataBatch_Empty(t *testing.T) {
	f := storetest.New(t)

	result, err := f.Store.MessageMetadataBatch(f.Source.ID, nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestStore_ReplaceMessageLabels_Empty(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-labels")

	labels := f.EnsureLabels(map[string]string{
		"INBOX":   "Inbox",
		"STARRED": "Starred",
	}, "system")

	// Add labels
	err := f.Store.ReplaceMessageLabels(msgID, []int64{labels["INBOX"], labels["STARRED"]})
	require.NoError(t, err, "ReplaceMessageLabels")

	f.AssertLabelCount(msgID, 2)

	// Replace with empty list (remove all labels)
	err = f.Store.ReplaceMessageLabels(msgID, []int64{})
	require.NoError(t, err, "ReplaceMessageLabels(empty)")

	f.AssertLabelCount(msgID, 0)
}

func TestStore_ReplaceMessageRecipients_Empty(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-recip")

	pid1 := f.EnsureParticipant("alice@example.com", "Alice", "example.com")

	// Add recipient
	err := f.Store.ReplaceMessageRecipients(msgID, "to", []int64{pid1}, []string{"Alice"})
	require.NoError(t, err, "ReplaceMessageRecipients")

	f.AssertRecipientCount(msgID, "to", 1)

	// Replace with empty list
	err = f.Store.ReplaceMessageRecipients(msgID, "to", []int64{}, []string{})
	require.NoError(t, err, "ReplaceMessageRecipients(empty)")

	f.AssertRecipientCount(msgID, "to", 0)
}

// TestStore_ReplaceMessageRecipients_DuplicateParticipants is the regression for
// the production crash where one recipient set contained the same participant
// twice (e.g. a calendar event listing the same attendee twice, or two address
// forms resolving to one participant). The plain INSERT tripped the
// UNIQUE(message_id, participant_id, recipient_type) constraint and aborted the
// write. Duplicates must collapse to a single row instead of erroring.
func TestStore_ReplaceMessageRecipients_DuplicateParticipants(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-dup-recip")
	pid1 := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	pid2 := f.EnsureParticipant("bob@example.com", "Bob", "example.com")

	// alice appears twice; this must not error and must store one row per
	// distinct participant.
	err := f.Store.ReplaceMessageRecipients(msgID, "to",
		[]int64{pid1, pid1, pid2}, []string{"Alice", "Alice (dup)", "Bob"})
	require.NoError(t, err, "duplicate participant IDs must not error")

	f.AssertRecipientCount(msgID, "to", 2)
}

func TestStore_GetActiveSync_NoSync(t *testing.T) {
	f := storetest.New(t)
	f.AssertNoActiveSync()
}

func TestStore_GetLastSuccessfulSync_None(t *testing.T) {
	f := storetest.New(t)

	// No successful sync yet
	lastSync, err := f.Store.GetLastSuccessfulSync(f.Source.ID)
	require.ErrorIs(t, err, store.ErrSyncRunNotFound, "GetLastSuccessfulSync()")
	assert.Nil(t, lastSync, "expected nil last sync")
}

func TestStore_GetSourceByIdentifier_NotFound(t *testing.T) {
	f := storetest.New(t)

	source, err := f.Store.GetSourceByIdentifier("nonexistent@example.com")
	require.ErrorIs(t, err, store.ErrSourceNotFound, "GetSourceByIdentifier()")
	assert.Nil(t, source, "expected nil source")
}

func TestStore_GetStats_WithData(t *testing.T) {
	f := storetest.New(t)

	// Add multiple messages
	f.CreateMessages(5)

	stats, err := f.Store.GetStats()
	require.NoError(t, err, "GetStats()")

	assert.Equal(t, int64(5), stats.MessageCount, "MessageCount")
	assert.NotZero(t, stats.ThreadCount, "ThreadCount should be non-zero")
}

func TestStore_GetStats_ExcludesDedupHidden(t *testing.T) {
	f := storetest.New(t)
	ids := f.CreateMessages(3)

	// Soft-delete one via dedup (deleted_at).
	_, err := f.Store.DB().Exec(
		f.Store.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?"), ids[0])
	require.NoError(t, err, "set deleted_at")

	stats, err := f.Store.GetStats()
	require.NoError(t, err, "GetStats()")
	assert.Equal(t, int64(2), stats.MessageCount, "MessageCount (dedup-hidden row excluded)")
}

func TestStore_GetStats_ExcludesSourceDeleted(t *testing.T) {
	f := storetest.New(t)
	ids := f.CreateMessages(3)

	// Mark one as deleted from source.
	_, err := f.Store.DB().Exec(
		f.Store.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"), ids[1])
	require.NoError(t, err, "set deleted_from_source_at")

	stats, err := f.Store.GetStats()
	require.NoError(t, err, "GetStats()")
	assert.Equal(t, int64(2), stats.MessageCount, "MessageCount (source-deleted row excluded)")
}

func TestStore_GetStats_ClosedDB(t *testing.T) {
	st := testutil.NewTestStore(t)

	// Close the database
	err := st.Close()
	require.NoError(t, err, "Close()")

	// GetStats should return an error for closed DB (not silently ignore)
	_, err = st.GetStats()
	assert.Error(t, err, "GetStats() should return error on closed DB")
}

func TestStore_GetStats_MissingTable(t *testing.T) {
	st := testutil.NewTestStore(t)

	// Drop a table to simulate missing table scenario
	_, err := st.DB().Exec("DROP TABLE IF EXISTS attachments")
	require.NoError(t, err, "DROP TABLE attachments")

	// GetStats should ignore missing tables and return partial stats
	stats, err := st.GetStats()
	require.NoError(t, err, "GetStats() with missing table")

	// AttachmentCount should be 0 (table missing, ignored)
	assert.Equal(t, int64(0), stats.AttachmentCount, "AttachmentCount (missing table)")
}

func TestStore_CountMessagesForSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	// Initially zero
	count, err := f.Store.CountMessagesForSource(f.Source.ID)
	require.NoError(err, "CountMessagesForSource()")
	assert.Equal(int64(0), count, "count")

	// Add messages
	f.CreateMessages(3)

	count, err = f.Store.CountMessagesForSource(f.Source.ID)
	require.NoError(err, "CountMessagesForSource()")
	assert.Equal(int64(3), count, "count")

	// Mark one as deleted - should not be counted
	err = f.Store.MarkMessageDeleted(f.Source.ID, "msg-0")
	require.NoError(err, "MarkMessageDeleted")

	count, err = f.Store.CountMessagesForSource(f.Source.ID)
	require.NoError(err, "CountMessagesForSource() after delete")
	assert.Equal(int64(2), count, "count after delete")
}

func TestStore_CountMessagesWithRaw(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	// Initially zero
	count, err := f.Store.CountMessagesWithRaw(f.Source.ID)
	require.NoError(err, "CountMessagesWithRaw()")
	assert.Equal(int64(0), count, "count")

	// Add messages, some with raw data
	for i := range 4 {
		msgID := f.CreateMessage(fmt.Sprintf("raw-count-msg-%d", i))

		// Only store raw for first 2 messages
		if i < 2 {
			err = f.Store.UpsertMessageRaw(msgID, sampleRawMessage)
			require.NoError(err, "UpsertMessageRaw")
		}
	}

	count, err = f.Store.CountMessagesWithRaw(f.Source.ID)
	require.NoError(err, "CountMessagesWithRaw()")
	assert.Equal(int64(2), count, "count")
}

func TestStore_GetRandomMessageIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	// Empty source
	ids, err := f.Store.GetRandomMessageIDs(f.Source.ID, 5)
	require.NoError(err, "GetRandomMessageIDs(empty)")
	assert.Empty(ids, "len(ids) for empty source")

	// Add 10 messages
	allIDs := make(map[int64]bool)
	createdIDs := f.CreateMessages(10)
	for _, id := range createdIDs {
		allIDs[id] = true
	}

	// Sample fewer than available
	ids, err = f.Store.GetRandomMessageIDs(f.Source.ID, 5)
	require.NoError(err, "GetRandomMessageIDs()")
	assert.Len(ids, 5)

	// All returned IDs should be valid
	for _, id := range ids {
		assert.True(allIDs[id], "returned ID %d is not in allIDs", id)
	}

	// All returned IDs should be unique
	seen := make(map[int64]bool)
	for _, id := range ids {
		assert.False(seen[id], "duplicate ID %d returned", id)
		seen[id] = true
	}

	// Sample more than available - should return all
	ids, err = f.Store.GetRandomMessageIDs(f.Source.ID, 20)
	require.NoError(err, "GetRandomMessageIDs(more than available)")
	assert.Len(ids, 10)
}

func TestStore_GetRandomMessageIDs_ExcludesDeleted(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	// Add 5 messages
	f.CreateMessages(5)

	// Delete 2 messages
	err := f.Store.MarkMessageDeleted(f.Source.ID, "msg-0")
	require.NoError(err, "MarkMessageDeleted msg-0")
	err = f.Store.MarkMessageDeleted(f.Source.ID, "msg-2")
	require.NoError(err, "MarkMessageDeleted msg-2")

	// Should only return 3 (non-deleted) messages
	ids, err := f.Store.GetRandomMessageIDs(f.Source.ID, 10)
	require.NoError(err, "GetRandomMessageIDs()")
	assert.Len(t, ids, 3, "len(ids) (5 total - 2 deleted)")
}

func TestStore_ReplaceMessageRecipients_LargeBatch(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-large-recipients")

	// Create 300 participants (exceeds SQLite limit of ~249 rows with 4 params each)
	const numRecipients = 300
	participantIDs := make([]int64, numRecipients)
	displayNames := make([]string, numRecipients)
	for i := range numRecipients {
		email := fmt.Sprintf("user%d@example.com", i)
		pid := f.EnsureParticipant(email, fmt.Sprintf("User %d", i), "example.com")
		participantIDs[i] = pid
		displayNames[i] = fmt.Sprintf("User %d", i)
	}

	// This should work without hitting SQLite parameter limit
	err := f.Store.ReplaceMessageRecipients(msgID, "to", participantIDs, displayNames)
	require.NoError(t, err, "ReplaceMessageRecipients(300 recipients)")

	f.AssertRecipientCount(msgID, "to", numRecipients)

	// Replace with a different large batch to ensure chunked delete+insert works
	err = f.Store.ReplaceMessageRecipients(msgID, "to", participantIDs[:150], displayNames[:150])
	require.NoError(t, err, "ReplaceMessageRecipients(150 recipients)")

	f.AssertRecipientCount(msgID, "to", 150)
}

func TestStore_UpsertFTS(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "directly queries the SQLite FTS5 vtable with MATCH; PG uses a tsvector column tested via FTSSearchClause")
	f := storetest.New(t)

	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	msgID := f.CreateMessage("msg-fts-1")

	// Store body for the message
	err := f.Store.UpsertMessageBody(msgID,
		sql.NullString{String: "hello world body text", Valid: true},
		sql.NullString{String: "<p>hello</p>", Valid: true},
	)
	require.NoError(err, "UpsertMessageBody")

	// Upsert FTS
	err = f.Store.UpsertFTS(msgID, "Test Subject", "hello world body text", "alice@example.com", "bob@example.com", "carol@example.com")
	require.NoError(err, "UpsertFTS")

	// Verify FTS row exists and is searchable
	var count int
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'hello'").Scan(&count)
	require.NoError(err, "FTS MATCH query")
	assert.Equal(1, count, "FTS match count")

	// Search by subject
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'subject'").Scan(&count)
	require.NoError(err, "FTS MATCH subject")
	assert.Equal(1, count, "FTS match subject count")

	// Search by from address
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'alice'").Scan(&count)
	require.NoError(err, "FTS MATCH from_addr")
	assert.Equal(1, count, "FTS match from_addr count")

	// Replace (upsert) FTS with updated content
	err = f.Store.UpsertFTS(msgID, "Updated Subject", "updated body", "alice@example.com", "bob@example.com", "")
	require.NoError(err, "UpsertFTS update")

	// Old content should no longer match
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'hello'").Scan(&count)
	require.NoError(err, "FTS MATCH after update")
	assert.Equal(0, count, "FTS match 'hello' after update")

	// New content should match
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'updated'").Scan(&count)
	require.NoError(err, "FTS MATCH 'updated'")
	assert.Equal(1, count, "FTS match 'updated'")
}

func TestStore_BackfillFTS(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "directly queries the SQLite FTS5 vtable; PG backfill is exercised separately via FTSBackfillBatchSQL")
	f := storetest.New(t)

	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	// Create messages with bodies and recipients
	msgID1 := f.CreateMessage("msg-backfill-1")
	err := f.Store.UpsertMessageBody(msgID1,
		sql.NullString{String: "first message body", Valid: true},
		sql.NullString{},
	)
	require.NoError(err, "UpsertMessageBody 1")

	pid1 := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
	err = f.Store.ReplaceMessageRecipients(msgID1, "from", []int64{pid1}, []string{"Sender"})
	require.NoError(err, "ReplaceMessageRecipients from")

	pid2 := f.EnsureParticipant("recipient@example.com", "Recipient", "example.com")
	err = f.Store.ReplaceMessageRecipients(msgID1, "to", []int64{pid2}, []string{"Recipient"})
	require.NoError(err, "ReplaceMessageRecipients to")

	msgID2 := f.CreateMessage("msg-backfill-2")
	err = f.Store.UpsertMessageBody(msgID2,
		sql.NullString{String: "second message unique content", Valid: true},
		sql.NullString{},
	)
	require.NoError(err, "UpsertMessageBody 2")

	// FTS should already have been auto-populated by InitSchema, so clear it first
	_, err = f.Store.DB().Exec("DELETE FROM messages_fts")
	require.NoError(err, "clear FTS")

	// Verify FTS is empty
	var count int
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts").Scan(&count)
	require.NoError(err, "count FTS")
	require.Equal(0, count, "FTS count after clear")

	// Run backfill
	rowsInserted, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")
	assert.Equal(int64(2), rowsInserted, "BackfillFTS rows")

	// Verify FTS is populated
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts").Scan(&count)
	require.NoError(err, "count FTS after backfill")
	assert.Equal(2, count, "FTS count after backfill")

	// Search for first message body
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'first'").Scan(&count)
	require.NoError(err, "FTS MATCH first")
	assert.Equal(1, count, "FTS match 'first'")

	// Search for sender email (populated via backfill from participants)
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'sender'").Scan(&count)
	require.NoError(err, "FTS MATCH sender")
	assert.Equal(1, count, "FTS match 'sender'")

	// Search for second message unique content
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'unique'").Scan(&count)
	require.NoError(err, "FTS MATCH unique")
	assert.Equal(1, count, "FTS match 'unique'")
}

func TestStore_FTS5Available(t *testing.T) {
	f := storetest.New(t)

	// FTS5Available should return a boolean (true on most builds)
	available := f.Store.FTS5Available()
	t.Logf("FTS5Available = %v", available)
}

func TestStore_NeedsFTSBackfill(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "directly mutates the SQLite FTS5 vtable; PG NeedsFTSBackfill probes the tsvector column instead")
	f := storetest.New(t)

	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	// Create messages with bodies so there's data to backfill
	msgID := f.CreateMessage("msg-auto-backfill")
	err := f.Store.UpsertMessageBody(msgID,
		sql.NullString{String: "auto backfill test body", Valid: true},
		sql.NullString{},
	)
	require.NoError(err, "UpsertMessageBody")

	pid := f.EnsureParticipant("autotest@example.com", "Auto", "example.com")
	err = f.Store.ReplaceMessageRecipients(msgID, "from", []int64{pid}, []string{"Auto"})
	require.NoError(err, "ReplaceMessageRecipients")

	// Clear FTS to simulate a pre-existing DB without FTS population
	_, err = f.Store.DB().Exec("DELETE FROM messages_fts")
	require.NoError(err, "clear FTS")

	// NeedsFTSBackfill should return true (empty FTS + existing messages)
	require.True(f.Store.NeedsFTSBackfill(), "NeedsFTSBackfill()")

	// Run backfill (simulating what CLI commands do after checking)
	n, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")
	assert.NotZero(n, "BackfillFTS returned 0 rows")

	// NeedsFTSBackfill should now return false
	assert.False(f.Store.NeedsFTSBackfill(), "NeedsFTSBackfill() after backfill")

	// Verify the backfilled data is searchable
	var count int
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'backfill'").Scan(&count)
	require.NoError(err, "FTS MATCH backfill")
	assert.Equal(1, count, "FTS match 'backfill'")
}

func TestStore_ReplaceMessageLabels_LargeBatch(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-large-labels")

	// Create 600 labels (exceeds SQLite limit of ~499 rows with 2 params each)
	const numLabels = 600
	labelIDs := make([]int64, numLabels)
	for i := range numLabels {
		sourceLabelID := fmt.Sprintf("Label_%d", i)
		lid, err := f.Store.EnsureLabel(f.Source.ID, sourceLabelID, fmt.Sprintf("Label %d", i), "user")
		require.NoError(t, err, "EnsureLabel")
		labelIDs[i] = lid
	}

	// This should work without hitting SQLite parameter limit
	err := f.Store.ReplaceMessageLabels(msgID, labelIDs)
	require.NoError(t, err, "ReplaceMessageLabels(600 labels)")

	f.AssertLabelCount(msgID, numLabels)

	// Replace with a different large batch to ensure chunked delete+insert works
	err = f.Store.ReplaceMessageLabels(msgID, labelIDs[:250])
	require.NoError(t, err, "ReplaceMessageLabels(250 labels)")

	f.AssertLabelCount(msgID, 250)
}

func TestStore_AddMessageLabels(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-add-labels")
	labels := f.EnsureLabels(map[string]string{
		"INBOX":   "Inbox",
		"STARRED": "Starred",
		"SENT":    "Sent",
		"TRASH":   "Trash",
	}, "system")

	// Start with INBOX
	err := f.Store.ReplaceMessageLabels(msgID, []int64{labels["INBOX"]})
	require.NoError(err, "ReplaceMessageLabels(INBOX)")
	f.AssertLabelCount(msgID, 1)

	// Add STARRED — should go from 1 to 2
	err = f.Store.AddMessageLabels(msgID, []int64{labels["STARRED"]})
	require.NoError(err, "AddMessageLabels(STARRED)")
	f.AssertLabelCount(msgID, 2)

	// Add INBOX again — should be a no-op (INSERT OR IGNORE)
	err = f.Store.AddMessageLabels(msgID, []int64{labels["INBOX"]})
	require.NoError(err, "AddMessageLabels(INBOX duplicate)")
	f.AssertLabelCount(msgID, 2)

	// Add multiple labels at once, including one that already exists
	err = f.Store.AddMessageLabels(msgID, []int64{labels["SENT"], labels["TRASH"], labels["STARRED"]})
	require.NoError(err, "AddMessageLabels(SENT, TRASH, STARRED)")
	f.AssertLabelCount(msgID, 4)

	// Empty list is a no-op
	err = f.Store.AddMessageLabels(msgID, []int64{})
	require.NoError(err, "AddMessageLabels(empty)")
	f.AssertLabelCount(msgID, 4)
}

func TestStore_RemoveMessageLabels(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-remove-labels")
	labels := f.EnsureLabels(map[string]string{
		"INBOX":   "Inbox",
		"STARRED": "Starred",
		"SENT":    "Sent",
		"TRASH":   "Trash",
	}, "system")

	// Start with all 4 labels
	err := f.Store.ReplaceMessageLabels(msgID, []int64{labels["INBOX"], labels["STARRED"], labels["SENT"], labels["TRASH"]})
	require.NoError(err, "ReplaceMessageLabels")
	f.AssertLabelCount(msgID, 4)

	// Remove STARRED — should go from 4 to 3
	err = f.Store.RemoveMessageLabels(msgID, []int64{labels["STARRED"]})
	require.NoError(err, "RemoveMessageLabels(STARRED)")
	f.AssertLabelCount(msgID, 3)

	// Remove STARRED again — should be a no-op (already removed)
	err = f.Store.RemoveMessageLabels(msgID, []int64{labels["STARRED"]})
	require.NoError(err, "RemoveMessageLabels(STARRED again)")
	f.AssertLabelCount(msgID, 3)

	// Remove multiple including INBOX and SENT
	err = f.Store.RemoveMessageLabels(msgID, []int64{labels["INBOX"], labels["SENT"]})
	require.NoError(err, "RemoveMessageLabels(INBOX, SENT)")
	f.AssertLabelCount(msgID, 1)

	// Verify the remaining label is TRASH
	labelID := f.GetSingleLabelID(msgID)
	assert.Equal(t, labels["TRASH"], labelID, "remaining label_id (TRASH)")

	// Empty list is a no-op
	err = f.Store.RemoveMessageLabels(msgID, []int64{})
	require.NoError(err, "RemoveMessageLabels(empty)")
	f.AssertLabelCount(msgID, 1)
}

func TestStore_MarkMessagesDeletedBatch(t *testing.T) {
	f := storetest.New(t)

	// Create several messages
	msgIDs := []string{"batch-del-1", "batch-del-2", "batch-del-3", "batch-del-4"}
	internalIDs := make(map[string]int64)
	for _, id := range msgIDs {
		internalIDs[id] = f.CreateMessage(id)
	}

	// Verify none are deleted
	for _, id := range msgIDs {
		f.AssertMessageNotDeleted(internalIDs[id])
	}

	// Batch delete first two
	err := f.Store.MarkMessagesDeletedBatch(f.Source.ID, []string{"batch-del-1", "batch-del-2"})
	require.NoError(t, err, "MarkMessagesDeletedBatch")

	// First two should be deleted, last two should not
	f.AssertMessageDeleted(internalIDs["batch-del-1"])
	f.AssertMessageDeleted(internalIDs["batch-del-2"])
	f.AssertMessageNotDeleted(internalIDs["batch-del-3"])
	f.AssertMessageNotDeleted(internalIDs["batch-del-4"])

	// Batch with non-existent IDs should not error
	err = f.Store.MarkMessagesDeletedBatch(f.Source.ID, []string{"nonexistent-1", "nonexistent-2"})
	require.NoError(t, err, "MarkMessagesDeletedBatch(nonexistent)")

	// Empty list is a no-op
	err = f.Store.MarkMessagesDeletedBatch(f.Source.ID, []string{})
	require.NoError(t, err, "MarkMessagesDeletedBatch(empty)")
}

func TestStore_PersistMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	pid1 := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	pid2 := f.EnsureParticipant("bob@example.org", "Bob", "example.org")

	labels := f.EnsureLabels(map[string]string{
		"INBOX":   "Inbox",
		"STARRED": "Starred",
	}, "system")

	msg := storetest.NewMessage(f.Source.ID, f.ConvID).
		WithSourceMessageID("persist-1").
		WithSubject("Hello").
		WithSnippet("preview").
		WithSize(1024).
		Build()

	data := &store.MessagePersistData{
		Message:  msg,
		BodyText: sql.NullString{String: "body text", Valid: true},
		BodyHTML: sql.NullString{String: "<p>body</p>", Valid: true},
		RawMIME:  sampleRawMessage,
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: []int64{pid1}, DisplayNames: []string{"Alice"}},
			{Type: "to", ParticipantIDs: []int64{pid2}, DisplayNames: []string{"Bob"}},
		},
		LabelIDs: []int64{labels["INBOX"], labels["STARRED"]},
	}

	messageID, err := f.Store.PersistMessage(data)
	require.NoError(err, "PersistMessage")
	require.NotZero(messageID, "message ID should be non-zero")

	// Verify message fields
	got := f.GetMessageFields(messageID)
	assert.Equal("Hello", got.Subject, "subject")

	// Verify body
	bodyText, bodyHTML := f.GetMessageBody(messageID)
	assert.Equal("body text", bodyText.String, "body_text")
	assert.Equal("<p>body</p>", bodyHTML.String, "body_html")

	// Verify raw MIME
	raw, err := f.Store.GetMessageRaw(messageID)
	require.NoError(err, "GetMessageRaw")
	assert.Equal(string(sampleRawMessage), string(raw), "raw")

	// Verify recipients
	f.AssertRecipientCount(messageID, "from", 1)
	f.AssertRecipientCount(messageID, "to", 1)

	// Verify labels
	f.AssertLabelCount(messageID, 2)
}

func TestStore_PersistMessage_Atomicity(t *testing.T) {
	f := storetest.New(t)

	msg := storetest.NewMessage(f.Source.ID, f.ConvID).
		WithSourceMessageID("persist-atomic").
		WithSubject("Atomic Test").
		Build()

	// Use a non-existent label ID to trigger an FK violation
	// during replaceMessageLabelsTx, which should roll back
	// the entire transaction including the message insert.
	data := &store.MessagePersistData{
		Message:  msg,
		BodyText: sql.NullString{String: "text", Valid: true},
		RawMIME:  sampleRawMessage,
		LabelIDs: []int64{999999},
	}

	_, err := f.Store.PersistMessage(data)
	require.Error(t, err, "PersistMessage should fail with invalid label ID")

	// Verify the message was NOT committed
	existing, err := f.Store.MessageExistsBatch(f.Source.ID, []string{"persist-atomic"})
	require.NoError(t, err, "MessageExistsBatch")
	assert.Empty(t, existing, "message should not exist after failed PersistMessage")
}

func TestStore_OAuthAppColumn(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	// Default source should have null oauth_app
	assert.False(f.Source.OAuthApp.Valid,
		"new source OAuthApp should be null, got %q", f.Source.OAuthApp.String)

	// Update oauth_app
	err := f.Store.UpdateSourceOAuthApp(f.Source.ID, sql.NullString{String: "acme", Valid: true})
	require.NoError(err, "UpdateSourceOAuthApp")

	// Read it back via ListSources
	sources, err := f.Store.ListSources("")
	require.NoError(err, "ListSources")

	found := false
	for _, src := range sources {
		if src.ID == f.Source.ID {
			found = true
			assert.True(src.OAuthApp.Valid, "OAuthApp should be valid")
			assert.Equal("acme", src.OAuthApp.String, "OAuthApp value")
		}
	}
	assert.True(found, "source not found in ListSources")
}

func TestStore_OAuthAppColumn_NullRoundTrip(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	// Set to acme
	err := f.Store.UpdateSourceOAuthApp(f.Source.ID, sql.NullString{String: "acme", Valid: true})
	require.NoError(err, "UpdateSourceOAuthApp(acme)")

	// Set back to null
	err = f.Store.UpdateSourceOAuthApp(f.Source.ID, sql.NullString{})
	require.NoError(err, "UpdateSourceOAuthApp(null)")

	// Verify via GetSourcesByIdentifier
	sources, err := f.Store.GetSourcesByIdentifier(f.Source.Identifier)
	require.NoError(err, "GetSourcesByIdentifier")

	require.NotEmpty(sources, "no sources found")
	assert.False(t, sources[0].OAuthApp.Valid,
		"OAuthApp should be null after clearing, got %q", sources[0].OAuthApp.String)
}

func TestStore_PersistMessage_Upsert(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	msg := storetest.NewMessage(f.Source.ID, f.ConvID).
		WithSourceMessageID("persist-upsert").
		WithSubject("Original").
		WithSnippet("preview").
		Build()

	data := &store.MessagePersistData{
		Message:  msg,
		BodyText: sql.NullString{String: "original body", Valid: true},
		RawMIME:  sampleRawMessage,
	}

	msgID1, err := f.Store.PersistMessage(data)
	require.NoError(err, "PersistMessage first call")

	// Update the message with different body
	msg.Subject = sql.NullString{String: "Updated", Valid: true}
	data.BodyText = sql.NullString{String: "updated body", Valid: true}

	msgID2, err := f.Store.PersistMessage(data)
	require.NoError(err, "PersistMessage second call")

	assert.Equal(msgID1, msgID2, "second call ID")

	// Verify updated fields
	got := f.GetMessageFields(msgID1)
	assert.Equal("Updated", got.Subject, "subject")

	bodyText, _ := f.GetMessageBody(msgID1)
	assert.Equal("updated body", bodyText.String, "body_text")
}

func TestStore_PersistMessageClearsEmbedGenWhenEmbeddingInputsChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()

	msg := storetest.NewMessage(f.Source.ID, f.ConvID).
		WithSourceMessageID("persist-embed-gen").
		WithSubject("Original subject").
		WithSnippet("preview").
		Build()
	data := &store.MessagePersistData{
		Message:  msg,
		BodyText: sql.NullString{String: "original body", Valid: true},
		RawMIME:  sampleRawMessage,
	}

	msgID, err := f.Store.PersistMessage(data)
	require.NoError(err, "PersistMessage first call")

	const gen = int64(7)
	require.NoError(f.Store.SetEmbedGen(ctx, []int64{msgID}, gen), "SetEmbedGen")
	assert.Equal(sql.NullInt64{Int64: gen, Valid: true}, readEmbedGen(t, f.Store, msgID),
		"precondition: message is stamped")

	msg.Snippet = sql.NullString{String: "preview changed", Valid: true}
	_, err = f.Store.PersistMessage(data)
	require.NoError(err, "PersistMessage unchanged embedding inputs")
	assert.Equal(sql.NullInt64{Int64: gen, Valid: true}, readEmbedGen(t, f.Store, msgID),
		"non-embedding metadata must not clear embed_gen")

	data.BodyHTML = sql.NullString{String: "<p>rendering changed</p>", Valid: true}
	_, err = f.Store.PersistMessage(data)
	require.NoError(err, "PersistMessage changed HTML with unchanged plaintext")
	assert.Equal(sql.NullInt64{Int64: gen, Valid: true}, readEmbedGen(t, f.Store, msgID),
		"HTML-only changes must not clear embed_gen while plaintext is the embedded body")

	data.BodyText = sql.NullString{String: "updated body", Valid: true}
	_, err = f.Store.PersistMessage(data)
	require.NoError(err, "PersistMessage changed body")
	assert.False(readEmbedGen(t, f.Store, msgID).Valid, "body change must clear embed_gen")

	require.NoError(f.Store.SetEmbedGen(ctx, []int64{msgID}, gen), "SetEmbedGen after body change")
	msg.Subject = sql.NullString{String: "Updated subject", Valid: true}
	_, err = f.Store.PersistMessage(data)
	require.NoError(err, "PersistMessage changed subject")
	assert.False(readEmbedGen(t, f.Store, msgID).Valid, "subject change must clear embed_gen")
}

func TestStore_PersistMessagePreservesEmbedGenForEquivalentHTMLFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()

	msg := storetest.NewMessage(f.Source.ID, f.ConvID).
		WithSourceMessageID("persist-html-only-embed-gen").
		WithSubject("HTML only").
		Build()
	data := &store.MessagePersistData{
		Message:  msg,
		BodyHTML: sql.NullString{String: "<p>Rendered body</p>", Valid: true},
		RawMIME:  sampleRawMessage,
	}

	msgID, err := f.Store.PersistMessage(data)
	require.NoError(err, "PersistMessage first call")

	const gen = int64(7)
	require.NoError(f.Store.SetEmbedGen(ctx, []int64{msgID}, gen), "SetEmbedGen")

	data.BodyHTML = sql.NullString{String: "<div><span>Rendered body</span></div>", Valid: true}
	_, err = f.Store.PersistMessage(data)
	require.NoError(err, "PersistMessage equivalent HTML fallback")
	assert.Equal(sql.NullInt64{Int64: gen, Valid: true}, readEmbedGen(t, f.Store, msgID),
		"markup-only HTML fallback changes must not clear embed_gen")
}

func readEmbedGen(t *testing.T, st *store.Store, msgID int64) sql.NullInt64 {
	t.Helper()
	var got sql.NullInt64
	err := st.DB().QueryRowContext(t.Context(),
		st.Rebind(`SELECT embed_gen FROM messages WHERE id = ?`), msgID).Scan(&got)
	require.NoError(t, err, "read embed_gen")
	return got
}

// --- GetStatsForScope tests ---

// makeSecondSource creates a second source and conversation in the same store as f.
func makeSecondSource(t *testing.T, f *storetest.Fixture, identifier string) (*store.Source, int64) {
	t.Helper()
	src, err := f.Store.GetOrCreateSource("gmail", identifier)
	require.NoError(t, err, "GetOrCreateSource "+identifier)
	convID, err := f.Store.EnsureConversation(src.ID, "thread-b-1", "Thread B")
	require.NoError(t, err, "EnsureConversation "+identifier)
	return src, convID
}

// createMessagesForSource inserts count messages under srcID/convID and returns their IDs.
func createMessagesForSource(t *testing.T, st *store.Store, srcID, convID int64, prefix string, count int) []int64 {
	t.Helper()
	ids := make([]int64, 0, count)
	for i := range count {
		id, err := st.UpsertMessage(&store.Message{
			ConversationID:  convID,
			SourceID:        srcID,
			SourceMessageID: fmt.Sprintf("%s-msg-%d", prefix, i),
			MessageType:     "email",
			SizeEstimate:    1000,
		})
		require.NoError(t, err, "UpsertMessage %s-%d", prefix, i)
		ids = append(ids, id)
	}
	return ids
}

func TestStore_GetStatsForScope_SingleSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	srcB, convB := makeSecondSource(t, f, "b@example.com")

	createMessagesForSource(t, f.Store, f.Source.ID, f.ConvID, "a", 3)
	createMessagesForSource(t, f.Store, srcB.ID, convB, "b", 2)

	// Scoped to source A only.
	statsA, err := f.Store.GetStatsForScope([]int64{f.Source.ID})
	require.NoError(err, "GetStatsForScope A")
	assert.Equal(int64(3), statsA.MessageCount, "MessageCount (A only)")
	assert.Equal(int64(1), statsA.SourceCount, "SourceCount (A only)")

	// Scoped to both sources.
	statsAB, err := f.Store.GetStatsForScope([]int64{f.Source.ID, srcB.ID})
	require.NoError(err, "GetStatsForScope A+B")
	assert.Equal(int64(5), statsAB.MessageCount, "MessageCount (A+B)")
	assert.Equal(int64(2), statsAB.SourceCount, "SourceCount (A+B)")

	// Unscoped (nil) should count all messages across both sources.
	statsAll, err := f.Store.GetStatsForScope(nil)
	require.NoError(err, "GetStatsForScope nil")
	assert.Equal(int64(5), statsAll.MessageCount, "MessageCount (nil/global)")
	assert.Equal(int64(2), statsAll.SourceCount, "SourceCount (nil/global)")
}

func TestStore_GetStatsForScope_ExcludesDedupHidden(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	srcB, convB := makeSecondSource(t, f, "b-dedup@example.com")

	idsA := createMessagesForSource(t, f.Store, f.Source.ID, f.ConvID, "a-dedup", 2)
	createMessagesForSource(t, f.Store, srcB.ID, convB, "b-dedup", 1)

	// Soft-delete one message in source A via dedup (deleted_at).
	_, err := f.Store.DB().Exec(
		f.Store.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?"), idsA[0])
	require.NoError(err, "set deleted_at")

	// Scoped to A: should see only the live message.
	statsA, err := f.Store.GetStatsForScope([]int64{f.Source.ID})
	require.NoError(err, "GetStatsForScope A")
	assert.Equal(int64(1), statsA.MessageCount, "MessageCount (A scoped, dedup-hidden excluded)")

	// Unscoped: should also exclude the dedup-hidden message (2 live, not 3).
	statsAll, err := f.Store.GetStatsForScope(nil)
	require.NoError(err, "GetStatsForScope nil")
	assert.Equal(int64(2), statsAll.MessageCount, "MessageCount (nil/global, dedup-hidden excluded)")
}

func TestStore_GetStatsForScope_ExcludesSourceDeleted(t *testing.T) {
	f := storetest.New(t)
	ids := createMessagesForSource(t, f.Store, f.Source.ID, f.ConvID, "a-srcdeleted", 2)

	// Mark one as deleted from source.
	_, err := f.Store.DB().Exec(
		f.Store.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"), ids[0])
	require.NoError(t, err, "set deleted_from_source_at")

	stats, err := f.Store.GetStatsForScope([]int64{f.Source.ID})
	require.NoError(t, err, "GetStatsForScope")
	assert.Equal(t, int64(1), stats.MessageCount, "MessageCount (source-deleted excluded)")
}
