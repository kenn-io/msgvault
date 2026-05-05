package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/wesm/msgvault/internal/mime"
	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/testutil"
	"github.com/wesm/msgvault/internal/testutil/storetest"
)

var sampleRawMessage = []byte("From: test@example.com\r\nSubject: Test\r\n\r\nBody")

func TestStore_Open(t *testing.T) {
	st := testutil.NewTestStore(t)

	// Store should be usable
	if st.DB() == nil {
		t.Error("DB() returned nil")
	}
}

func TestStore_GetStats_Empty(t *testing.T) {
	st := testutil.NewTestStore(t)

	stats, err := st.GetStats()
	testutil.MustNoErr(t, err, "GetStats()")

	if stats.MessageCount != 0 {
		t.Errorf("MessageCount = %d, want 0", stats.MessageCount)
	}
	if stats.ThreadCount != 0 {
		t.Errorf("ThreadCount = %d, want 0", stats.ThreadCount)
	}
	if stats.SourceCount != 0 {
		t.Errorf("SourceCount = %d, want 0", stats.SourceCount)
	}
}

func TestStore_Source_CreateAndGet(t *testing.T) {
	st := testutil.NewTestStore(t)

	// Create source
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	testutil.MustNoErr(t, err, "GetOrCreateSource()")

	if source.ID == 0 {
		t.Error("source ID should be non-zero")
	}
	if source.SourceType != "gmail" {
		t.Errorf("SourceType = %q, want %q", source.SourceType, "gmail")
	}
	if source.Identifier != "test@example.com" {
		t.Errorf("Identifier = %q, want %q", source.Identifier, "test@example.com")
	}

	// Get same source again (should return existing)
	source2, err := st.GetOrCreateSource("gmail", "test@example.com")
	testutil.MustNoErr(t, err, "GetOrCreateSource() second call")

	if source2.ID != source.ID {
		t.Errorf("second call ID = %d, want %d", source2.ID, source.ID)
	}
}

func TestStore_Source_UpdateSyncCursor(t *testing.T) {
	f := storetest.New(t)

	err := f.Store.UpdateSourceSyncCursor(f.Source.ID, "12345")
	testutil.MustNoErr(t, err, "UpdateSourceSyncCursor()")

	// Verify cursor was updated
	updated, err := f.Store.GetSourceByIdentifier("test@example.com")
	testutil.MustNoErr(t, err, "GetSourceByIdentifier()")

	if !updated.SyncCursor.Valid || updated.SyncCursor.String != "12345" {
		t.Errorf("SyncCursor = %v, want 12345", updated.SyncCursor)
	}
}

func TestStore_Source_UpdateDisplayName(t *testing.T) {
	f := storetest.New(t)

	err := f.Store.UpdateSourceDisplayName(f.Source.ID, "Work Account")
	testutil.MustNoErr(t, err, "UpdateSourceDisplayName()")

	// Verify display name was updated
	updated, err := f.Store.GetSourceByIdentifier("test@example.com")
	testutil.MustNoErr(t, err, "GetSourceByIdentifier()")

	if !updated.DisplayName.Valid || updated.DisplayName.String != "Work Account" {
		t.Errorf("DisplayName = %v, want 'Work Account'", updated.DisplayName)
	}
}

func TestStore_ListSources(t *testing.T) {
	f := storetest.New(t)

	sources, err := f.Store.ListSources("")
	testutil.MustNoErr(t, err, "ListSources()")

	if len(sources) != 1 {
		t.Fatalf("len(sources) = %d, want 1", len(sources))
	}
	if sources[0].Identifier != "test@example.com" {
		t.Errorf("Identifier = %q, want %q", sources[0].Identifier, "test@example.com")
	}
	if sources[0].ID != f.Source.ID {
		t.Errorf("ID = %d, want %d", sources[0].ID, f.Source.ID)
	}
}

func TestStore_Conversation(t *testing.T) {
	f := storetest.New(t)

	// Create conversation
	convID, err := f.Store.EnsureConversation(f.Source.ID, "thread-123", "Test Thread")
	testutil.MustNoErr(t, err, "EnsureConversation()")

	if convID == 0 {
		t.Error("conversation ID should be non-zero")
	}

	// Get same conversation (should return existing)
	convID2, err := f.Store.EnsureConversation(f.Source.ID, "thread-123", "Test Thread")
	testutil.MustNoErr(t, err, "EnsureConversation() second call")

	if convID2 != convID {
		t.Errorf("second call ID = %d, want %d", convID2, convID)
	}
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
			f := storetest.New(t)
			msg := tt.msg(f.Source.ID, f.ConvID)

			// Insert
			msgID, err := f.Store.UpsertMessage(msg)
			testutil.MustNoErr(t, err, "UpsertMessage insert")
			if msgID == 0 {
				t.Error("message ID should be non-zero")
			}

			// Update: mutate fields and upsert again
			msg.Subject = sql.NullString{String: "Updated Subject", Valid: true}
			msg.Snippet = sql.NullString{String: "Updated snippet", Valid: true}
			msg.HasAttachments = !msg.HasAttachments
			msgID2, err := f.Store.UpsertMessage(msg)
			testutil.MustNoErr(t, err, "UpsertMessage update")
			if msgID2 != msgID {
				t.Errorf("update ID = %d, want %d", msgID2, msgID)
			}

			// Verify updated fields are persisted
			got := f.GetMessageFields(msgID)
			if got.Subject != "Updated Subject" {
				t.Errorf("subject = %q, want %q", got.Subject, "Updated Subject")
			}
			if got.Snippet != "Updated snippet" {
				t.Errorf("snippet = %q, want %q", got.Snippet, "Updated snippet")
			}
			if got.HasAttachments != msg.HasAttachments {
				t.Errorf("has_attachments = %v, want %v", got.HasAttachments, msg.HasAttachments)
			}

			// Verify stats show exactly one message
			stats, err := f.Store.GetStats()
			testutil.MustNoErr(t, err, "GetStats")
			if stats.MessageCount != 1 {
				t.Errorf("MessageCount = %d, want 1", stats.MessageCount)
			}
		})
	}
}

func TestStore_MessageExistsBatch(t *testing.T) {
	f := storetest.New(t)

	// Insert some messages
	ids := []string{"msg-1", "msg-2", "msg-3"}
	for _, id := range ids {
		f.CreateMessage(id)
	}

	// Check which exist
	checkIDs := []string{"msg-1", "msg-2", "msg-4", "msg-5"}
	existing, err := f.Store.MessageExistsBatch(f.Source.ID, checkIDs)
	testutil.MustNoErr(t, err, "MessageExistsBatch()")

	if len(existing) != 2 {
		t.Errorf("len(existing) = %d, want 2", len(existing))
	}
	if _, ok := existing["msg-1"]; !ok {
		t.Error("msg-1 should exist")
	}
	if _, ok := existing["msg-2"]; !ok {
		t.Error("msg-2 should exist")
	}
	if _, ok := existing["msg-4"]; ok {
		t.Error("msg-4 should not exist")
	}
}

func TestStore_MessageRaw(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-1")

	// Store raw data
	err := f.Store.UpsertMessageRaw(msgID, sampleRawMessage)
	testutil.MustNoErr(t, err, "UpsertMessageRaw()")

	// Retrieve raw data
	retrieved, err := f.Store.GetMessageRaw(msgID)
	testutil.MustNoErr(t, err, "GetMessageRaw()")

	if string(retrieved) != string(sampleRawMessage) {
		t.Errorf("retrieved data = %q, want %q", retrieved, sampleRawMessage)
	}
}

func TestStore_Participant(t *testing.T) {
	f := storetest.New(t)

	// Create participant
	pid := f.EnsureParticipant("alice@example.com", "Alice Smith", "example.com")
	if pid == 0 {
		t.Error("participant ID should be non-zero")
	}

	// Get same participant (should return existing)
	pid2 := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	if pid2 != pid {
		t.Errorf("second call ID = %d, want %d", pid2, pid)
	}
}

func TestStore_EnsureParticipantsBatch(t *testing.T) {
	f := storetest.New(t)

	addresses := []mime.Address{
		{Email: "alice@example.com", Name: "Alice", Domain: "example.com"},
		{Email: "bob@example.org", Name: "Bob", Domain: "example.org"},
		{Email: "", Name: "No Email", Domain: ""}, // Should be skipped
	}

	result, err := f.Store.EnsureParticipantsBatch(addresses)
	testutil.MustNoErr(t, err, "EnsureParticipantsBatch()")

	if len(result) != 2 {
		t.Errorf("len(result) = %d, want 2", len(result))
	}
	if _, ok := result["alice@example.com"]; !ok {
		t.Error("alice@example.com should be in result")
	}
	if _, ok := result["bob@example.org"]; !ok {
		t.Error("bob@example.org should be in result")
	}
}

func TestStore_Label(t *testing.T) {
	f := storetest.New(t)

	// Create label
	lid, err := f.Store.EnsureLabel(f.Source.ID, "INBOX", "Inbox", "system")
	testutil.MustNoErr(t, err, "EnsureLabel()")

	if lid == 0 {
		t.Error("label ID should be non-zero")
	}

	// Get same label
	lid2, err := f.Store.EnsureLabel(f.Source.ID, "INBOX", "Inbox", "system")
	testutil.MustNoErr(t, err, "EnsureLabel() second call")

	if lid2 != lid {
		t.Errorf("second call ID = %d, want %d", lid2, lid)
	}
}

func TestStore_EnsureLabel_NameConflict(t *testing.T) {
	f := storetest.New(t)

	// Create label L1 with name "Work"
	lid1, err := f.Store.EnsureLabel(f.Source.ID, "L1", "Work", "user")
	testutil.MustNoErr(t, err, "create L1")

	// Insert a different label L2 with the same name — should upsert,
	// not crash with UNIQUE constraint violation (#232).
	lid2, err := f.Store.EnsureLabel(f.Source.ID, "L2", "Work", "user")
	testutil.MustNoErr(t, err, "create L2 with same name")

	if lid2 != lid1 {
		t.Errorf("L2 ID = %d, want %d (should reuse existing row)", lid2, lid1)
	}

	// L1's source_label_id was overwritten — looking up L2 should work
	lid2Again, err := f.Store.EnsureLabel(f.Source.ID, "L2", "Work", "user")
	testutil.MustNoErr(t, err, "re-lookup L2")
	if lid2Again != lid1 {
		t.Errorf("re-lookup L2 ID = %d, want %d", lid2Again, lid1)
	}
}

func TestStore_EnsureLabel_Rename(t *testing.T) {
	f := storetest.New(t)

	// Create label L1 with name "OldName"
	lid, err := f.Store.EnsureLabel(f.Source.ID, "L1", "OldName", "user")
	testutil.MustNoErr(t, err, "create L1")

	// Same source_label_id, different name (label was renamed in Gmail)
	lid2, err := f.Store.EnsureLabel(f.Source.ID, "L1", "NewName", "user")
	testutil.MustNoErr(t, err, "rename L1")

	if lid2 != lid {
		t.Errorf("renamed label ID = %d, want %d", lid2, lid)
	}
}

func TestStore_EnsureLabel_RenameAndReuse(t *testing.T) {
	f := storetest.New(t)

	// Scenario: L1 named "Foo" is renamed to "Bar", then L2 takes "Foo".
	_, err := f.Store.EnsureLabel(f.Source.ID, "L1", "Foo", "user")
	testutil.MustNoErr(t, err, "create L1=Foo")

	// L1 renamed to "Bar" — should update name in place
	_, err = f.Store.EnsureLabel(f.Source.ID, "L1", "Bar", "user")
	testutil.MustNoErr(t, err, "rename L1=Bar")

	// New label L2 takes the old name "Foo" — should succeed
	_, err = f.Store.EnsureLabel(f.Source.ID, "L2", "Foo", "user")
	testutil.MustNoErr(t, err, "create L2=Foo")
}

func TestStore_EnsureLabel_RenameSwap(t *testing.T) {
	f := storetest.New(t)

	// L1="Foo" exists, and L1 is renamed to the name of an existing
	// label "Bar" (which has source_label_id "L2"). The rename merges
	// L2 into L1 so L1 can take the name.
	lid1, err := f.Store.EnsureLabel(f.Source.ID, "L1", "Foo", "user")
	testutil.MustNoErr(t, err, "create L1=Foo")

	lid2, err := f.Store.EnsureLabel(f.Source.ID, "L2", "Bar", "user")
	testutil.MustNoErr(t, err, "create L2=Bar")

	// Tag a message with L2 so we can verify associations survive merge
	msgID := f.CreateMessage("swap-msg")
	err = f.Store.ReplaceMessageLabels(msgID, []int64{lid2})
	testutil.MustNoErr(t, err, "tag message with L2")

	// L1 renamed to "Bar" — merges L2 into L1
	lid1After, err := f.Store.EnsureLabel(f.Source.ID, "L1", "Bar", "user")
	testutil.MustNoErr(t, err, "rename L1=Bar (was L2's name)")

	if lid1After != lid1 {
		t.Errorf("L1 ID changed: got %d, want %d", lid1After, lid1)
	}

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
	testutil.MustNoErr(t, err, "EnsureLabelsBatch()")

	if len(result) != 3 {
		t.Errorf("len(result) = %d, want 3", len(result))
	}
	for sourceLabelID := range labels {
		if _, ok := result[sourceLabelID]; !ok {
			t.Errorf("%s should be in result", sourceLabelID)
		}
	}
}

func TestStore_EnsureLabelsBatch_CrossRename(t *testing.T) {
	f := storetest.New(t)

	// Initial state: L1="Foo", L2="Bar"
	initial := map[string]store.LabelInfo{
		"L1": {Name: "Foo", Type: "user"},
		"L2": {Name: "Bar", Type: "user"},
	}
	ids, err := f.Store.EnsureLabelsBatch(f.Source.ID, initial)
	testutil.MustNoErr(t, err, "initial batch")

	l1ID, l2ID := ids["L1"], ids["L2"]

	// Tag messages with each label
	msg1 := f.CreateMessage("cross-1")
	msg2 := f.CreateMessage("cross-2")
	err = f.Store.ReplaceMessageLabels(msg1, []int64{l1ID})
	testutil.MustNoErr(t, err, "tag msg1 with L1")
	err = f.Store.ReplaceMessageLabels(msg2, []int64{l2ID})
	testutil.MustNoErr(t, err, "tag msg2 with L2")

	// Cross-rename: L1→"Bar", L2→"Foo"
	swapped := map[string]store.LabelInfo{
		"L1": {Name: "Bar", Type: "user"},
		"L2": {Name: "Foo", Type: "user"},
	}
	ids2, err := f.Store.EnsureLabelsBatch(f.Source.ID, swapped)
	testutil.MustNoErr(t, err, "cross-rename batch")

	// IDs should be preserved
	if ids2["L1"] != l1ID {
		t.Errorf("L1 id changed: got %d, want %d", ids2["L1"], l1ID)
	}
	if ids2["L2"] != l2ID {
		t.Errorf("L2 id changed: got %d, want %d", ids2["L2"], l2ID)
	}

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
	testutil.MustNoErr(t, err, "ReplaceMessageLabels()")

	f.AssertLabelCount(msgID, 2)

	// Replace with different labels
	err = f.Store.ReplaceMessageLabels(msgID, []int64{labels["SENT"]})
	testutil.MustNoErr(t, err, "ReplaceMessageLabels() replace")

	f.AssertLabelCount(msgID, 1)

	// Verify it's the right label
	labelID := f.GetSingleLabelID(msgID)
	if labelID != labels["SENT"] {
		t.Errorf("label_id = %d, want %d (SENT)", labelID, labels["SENT"])
	}
}

func TestStore_MessageRecipients(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-1")

	pid1 := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	pid2 := f.EnsureParticipant("bob@example.org", "Bob", "example.org")

	// Set recipients
	err := f.Store.ReplaceMessageRecipients(msgID, "to", []int64{pid1, pid2}, []string{"Alice", "Bob"})
	testutil.MustNoErr(t, err, "ReplaceMessageRecipients()")

	f.AssertRecipientCount(msgID, "to", 2)

	// Replace recipients
	err = f.Store.ReplaceMessageRecipients(msgID, "to", []int64{pid1}, []string{"Alice"})
	testutil.MustNoErr(t, err, "ReplaceMessageRecipients() replace")

	f.AssertRecipientCount(msgID, "to", 1)

	// Verify it's the right recipient
	participantID := f.GetSingleRecipientID(msgID, "to")
	if participantID != pid1 {
		t.Errorf("participant_id = %d, want %d (alice)", participantID, pid1)
	}
}

func TestStore_MarkMessageDeleted(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-1")

	f.AssertMessageNotDeleted(msgID)

	err := f.Store.MarkMessageDeleted(f.Source.ID, "msg-1")
	testutil.MustNoErr(t, err, "MarkMessageDeleted()")

	f.AssertMessageDeleted(msgID)
}

func TestStore_Attachment(t *testing.T) {
	f := storetest.New(t)

	msgID := storetest.NewMessage(f.Source.ID, f.ConvID).
		WithSourceMessageID("msg-1").
		WithAttachmentCount(1).
		Create(t, f.Store)

	err := f.Store.UpsertAttachment(msgID, "document.pdf", "application/pdf", "/path/to/file", "abc123hash", 1024)
	testutil.MustNoErr(t, err, "UpsertAttachment()")

	// Upsert same attachment (should not error, dedupe by content_hash)
	err = f.Store.UpsertAttachment(msgID, "document.pdf", "application/pdf", "/path/to/file", "abc123hash", 1024)
	testutil.MustNoErr(t, err, "UpsertAttachment() duplicate")

	stats, err := f.Store.GetStats()
	testutil.MustNoErr(t, err, "GetStats")
	if stats.AttachmentCount != 1 {
		t.Errorf("AttachmentCount = %d, want 1", stats.AttachmentCount)
	}
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
	testutil.MustNoErr(t, err, "UpdateSyncCheckpoint()")

	// Verify checkpoint was saved
	f.AssertActiveSync(syncID, "running")
	active, err := f.Store.GetActiveSync(f.Source.ID)
	testutil.MustNoErr(t, err, "GetActiveSync")
	if active.MessagesProcessed != 100 {
		t.Errorf("sync MessagesProcessed = %d, want %d", active.MessagesProcessed, 100)
	}
}

func TestStore_SyncComplete(t *testing.T) {
	f := storetest.New(t)

	syncID := f.StartSync()

	err := f.Store.CompleteSync(syncID, "history-12345")
	testutil.MustNoErr(t, err, "CompleteSync()")

	f.AssertNoActiveSync()

	// Should have a successful sync
	lastSync, err := f.Store.GetLastSuccessfulSync(f.Source.ID)
	testutil.MustNoErr(t, err, "GetLastSuccessfulSync()")
	if lastSync == nil {
		t.Fatal("expected last successful sync, got nil")
	}
	if lastSync.Status != "completed" {
		t.Errorf("status = %q, want %q", lastSync.Status, "completed")
	}
}

func TestStore_SyncFail(t *testing.T) {
	f := storetest.New(t)

	syncID := f.StartSync()

	err := f.Store.FailSync(syncID, "network error")
	testutil.MustNoErr(t, err, "FailSync()")

	f.AssertNoActiveSync()

	// Verify sync status is "failed" and error message is stored
	status, errorMsg := f.GetSyncRun(syncID)
	if status != "failed" {
		t.Errorf("sync status = %q, want %q", status, "failed")
	}
	if errorMsg != "network error" {
		t.Errorf("error_message = %q, want %q", errorMsg, "network error")
	}
}

func TestStore_GetMessage_DeletedMessageVisibleByID(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("deleted-msg-1")
	_, err := f.Store.DB().Exec(
		f.Store.Rebind("UPDATE messages SET deleted_from_source_at = ? WHERE id = ?"),
		time.Date(2026, 3, 18, 14, 30, 0, 0, time.UTC),
		msgID,
	)
	testutil.MustNoErr(t, err, "mark deleted")

	msg, err := f.Store.GetMessage(msgID)
	testutil.MustNoErr(t, err, "GetMessage()")
	if msg == nil {
		t.Fatal("GetMessage() = nil, want deleted message")
	}
}

func TestStore_MarkMessageDeletedByGmailID(t *testing.T) {
	f := storetest.New(t)

	f.CreateMessage("gmail-msg-123")

	// Mark as deleted (trash)
	err := f.Store.MarkMessageDeletedByGmailID(false, "gmail-msg-123")
	testutil.MustNoErr(t, err, "MarkMessageDeletedByGmailID(trash)")

	// Mark as permanently deleted
	err = f.Store.MarkMessageDeletedByGmailID(true, "gmail-msg-123")
	testutil.MustNoErr(t, err, "MarkMessageDeletedByGmailID(permanent)")

	// Non-existent message should not error (no rows affected is OK)
	err = f.Store.MarkMessageDeletedByGmailID(true, "nonexistent-id")
	testutil.MustNoErr(t, err, "MarkMessageDeletedByGmailID(nonexistent)")
}

func TestStore_MarkMessagesDeletedByGmailIDBatch(t *testing.T) {
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
	testutil.MustNoErr(t, err, "MarkMessagesDeletedByGmailIDBatch")

	// Verify all are marked deleted
	var deletedCount int
	err = f.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE deleted_from_source_at IS NOT NULL`,
	).Scan(&deletedCount)
	testutil.MustNoErr(t, err, "count deleted")

	if deletedCount != count {
		t.Errorf("deleted count = %d, want %d", deletedCount, count)
	}

	// Empty batch should be a no-op
	err = f.Store.MarkMessagesDeletedByGmailIDBatch(nil)
	testutil.MustNoErr(t, err, "MarkMessagesDeletedByGmailIDBatch(nil)")
}

func TestStore_GetMessageRaw_NotFound(t *testing.T) {
	f := storetest.New(t)

	// Try to get raw for non-existent message
	_, err := f.Store.GetMessageRaw(99999)
	if err == nil {
		t.Error("GetMessageRaw() should error for non-existent message")
	}
}

func TestStore_UpsertMessageRaw_Update(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-raw-update")

	// Insert raw data
	rawData1 := []byte("Original raw content")
	err := f.Store.UpsertMessageRaw(msgID, rawData1)
	testutil.MustNoErr(t, err, "UpsertMessageRaw()")

	// Update with new raw data
	rawData2 := []byte("Updated raw content that is different")
	err = f.Store.UpsertMessageRaw(msgID, rawData2)
	testutil.MustNoErr(t, err, "UpsertMessageRaw() update")

	// Verify updated data
	retrieved, err := f.Store.GetMessageRaw(msgID)
	testutil.MustNoErr(t, err, "GetMessageRaw")
	if string(retrieved) != string(rawData2) {
		t.Errorf("retrieved = %q, want %q", retrieved, rawData2)
	}
}

func TestStore_UpsertMessageBody(t *testing.T) {
	f := storetest.New(t)
	msgID := f.CreateMessage("msg-body-test")

	// Insert body
	err := f.Store.UpsertMessageBody(msgID,
		sql.NullString{String: "hello text", Valid: true},
		sql.NullString{String: "<p>hello html</p>", Valid: true},
	)
	testutil.MustNoErr(t, err, "UpsertMessageBody()")

	// Verify via helper
	bodyText, bodyHTML := f.GetMessageBody(msgID)
	if bodyText.String != "hello text" {
		t.Errorf("body_text = %q, want %q", bodyText.String, "hello text")
	}
	if bodyHTML.String != "<p>hello html</p>" {
		t.Errorf("body_html = %q, want %q", bodyHTML.String, "<p>hello html</p>")
	}

	// Update body (upsert)
	err = f.Store.UpsertMessageBody(msgID,
		sql.NullString{String: "updated text", Valid: true},
		sql.NullString{},
	)
	testutil.MustNoErr(t, err, "UpsertMessageBody() update")
	bodyText, bodyHTML = f.GetMessageBody(msgID)
	if bodyText.String != "updated text" {
		t.Errorf("after update: body_text = %q, want %q", bodyText.String, "updated text")
	}
	// UpsertMessageBody overwrites both columns; invalid NullString NULLs the column
	if bodyHTML.Valid {
		t.Errorf("after update: body_html should be NULL, got %q", bodyHTML.String)
	}
}

func TestStore_MessageExistsBatch_Empty(t *testing.T) {
	f := storetest.New(t)

	// Check with empty list
	result, err := f.Store.MessageExistsBatch(f.Source.ID, []string{})
	testutil.MustNoErr(t, err, "MessageExistsBatch(empty)")
	if len(result) != 0 {
		t.Errorf("len(result) = %d, want 0", len(result))
	}
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
	testutil.MustNoErr(t, err, "ReplaceMessageLabels")

	f.AssertLabelCount(msgID, 2)

	// Replace with empty list (remove all labels)
	err = f.Store.ReplaceMessageLabels(msgID, []int64{})
	testutil.MustNoErr(t, err, "ReplaceMessageLabels(empty)")

	f.AssertLabelCount(msgID, 0)
}

func TestStore_ReplaceMessageRecipients_Empty(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-recip")

	pid1 := f.EnsureParticipant("alice@example.com", "Alice", "example.com")

	// Add recipient
	err := f.Store.ReplaceMessageRecipients(msgID, "to", []int64{pid1}, []string{"Alice"})
	testutil.MustNoErr(t, err, "ReplaceMessageRecipients")

	f.AssertRecipientCount(msgID, "to", 1)

	// Replace with empty list
	err = f.Store.ReplaceMessageRecipients(msgID, "to", []int64{}, []string{})
	testutil.MustNoErr(t, err, "ReplaceMessageRecipients(empty)")

	f.AssertRecipientCount(msgID, "to", 0)
}

func TestStore_GetActiveSync_NoSync(t *testing.T) {
	f := storetest.New(t)
	f.AssertNoActiveSync()
}

func TestStore_GetLastSuccessfulSync_None(t *testing.T) {
	f := storetest.New(t)

	// No successful sync yet
	lastSync, err := f.Store.GetLastSuccessfulSync(f.Source.ID)
	testutil.MustNoErr(t, err, "GetLastSuccessfulSync()")
	if lastSync != nil {
		t.Errorf("expected nil last sync, got %+v", lastSync)
	}
}

func TestStore_GetSourceByIdentifier_NotFound(t *testing.T) {
	f := storetest.New(t)

	source, err := f.Store.GetSourceByIdentifier("nonexistent@example.com")
	testutil.MustNoErr(t, err, "GetSourceByIdentifier()")
	if source != nil {
		t.Errorf("expected nil source, got %+v", source)
	}
}

func TestStore_GetStats_WithData(t *testing.T) {
	f := storetest.New(t)

	// Add multiple messages
	f.CreateMessages(5)

	stats, err := f.Store.GetStats()
	testutil.MustNoErr(t, err, "GetStats()")

	if stats.MessageCount != 5 {
		t.Errorf("MessageCount = %d, want 5", stats.MessageCount)
	}
	if stats.ThreadCount == 0 {
		t.Error("ThreadCount should be non-zero")
	}
}

func TestStore_GetStats_ExcludesDedupHidden(t *testing.T) {
	f := storetest.New(t)
	ids := f.CreateMessages(3)

	// Soft-delete one via dedup (deleted_at).
	_, err := f.Store.DB().Exec(
		f.Store.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?"), ids[0])
	testutil.MustNoErr(t, err, "set deleted_at")

	stats, err := f.Store.GetStats()
	testutil.MustNoErr(t, err, "GetStats()")
	if stats.MessageCount != 2 {
		t.Errorf("MessageCount = %d, want 2 (dedup-hidden row excluded)", stats.MessageCount)
	}
}

func TestStore_GetStats_ExcludesSourceDeleted(t *testing.T) {
	f := storetest.New(t)
	ids := f.CreateMessages(3)

	// Mark one as deleted from source.
	_, err := f.Store.DB().Exec(
		f.Store.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"), ids[1])
	testutil.MustNoErr(t, err, "set deleted_from_source_at")

	stats, err := f.Store.GetStats()
	testutil.MustNoErr(t, err, "GetStats()")
	if stats.MessageCount != 2 {
		t.Errorf("MessageCount = %d, want 2 (source-deleted row excluded)", stats.MessageCount)
	}
}

func TestStore_GetStats_ClosedDB(t *testing.T) {
	st := testutil.NewTestStore(t)

	// Close the database
	err := st.Close()
	testutil.MustNoErr(t, err, "Close()")

	// GetStats should return an error for closed DB (not silently ignore)
	_, err = st.GetStats()
	if err == nil {
		t.Error("GetStats() should return error on closed DB")
	}
}

func TestStore_GetStats_MissingTable(t *testing.T) {
	st := testutil.NewTestStore(t)

	// Drop a table to simulate missing table scenario
	_, err := st.DB().Exec("DROP TABLE IF EXISTS attachments")
	testutil.MustNoErr(t, err, "DROP TABLE attachments")

	// GetStats should ignore missing tables and return partial stats
	stats, err := st.GetStats()
	testutil.MustNoErr(t, err, "GetStats() with missing table")

	// AttachmentCount should be 0 (table missing, ignored)
	if stats.AttachmentCount != 0 {
		t.Errorf("AttachmentCount = %d, want 0 for missing table", stats.AttachmentCount)
	}
}

func TestStore_CountMessagesForSource(t *testing.T) {
	f := storetest.New(t)

	// Initially zero
	count, err := f.Store.CountMessagesForSource(f.Source.ID)
	testutil.MustNoErr(t, err, "CountMessagesForSource()")
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}

	// Add messages
	f.CreateMessages(3)

	count, err = f.Store.CountMessagesForSource(f.Source.ID)
	testutil.MustNoErr(t, err, "CountMessagesForSource()")
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}

	// Mark one as deleted - should not be counted
	err = f.Store.MarkMessageDeleted(f.Source.ID, "msg-0")
	testutil.MustNoErr(t, err, "MarkMessageDeleted")

	count, err = f.Store.CountMessagesForSource(f.Source.ID)
	testutil.MustNoErr(t, err, "CountMessagesForSource() after delete")
	if count != 2 {
		t.Errorf("count after delete = %d, want 2", count)
	}
}

func TestStore_CountMessagesWithRaw(t *testing.T) {
	f := storetest.New(t)

	// Initially zero
	count, err := f.Store.CountMessagesWithRaw(f.Source.ID)
	testutil.MustNoErr(t, err, "CountMessagesWithRaw()")
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}

	// Add messages, some with raw data
	for i := 0; i < 4; i++ {
		msgID := f.CreateMessage(fmt.Sprintf("raw-count-msg-%d", i))

		// Only store raw for first 2 messages
		if i < 2 {
			err = f.Store.UpsertMessageRaw(msgID, sampleRawMessage)
			testutil.MustNoErr(t, err, "UpsertMessageRaw")
		}
	}

	count, err = f.Store.CountMessagesWithRaw(f.Source.ID)
	testutil.MustNoErr(t, err, "CountMessagesWithRaw()")
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestStore_GetRandomMessageIDs(t *testing.T) {
	f := storetest.New(t)

	// Empty source
	ids, err := f.Store.GetRandomMessageIDs(f.Source.ID, 5)
	testutil.MustNoErr(t, err, "GetRandomMessageIDs(empty)")
	if len(ids) != 0 {
		t.Errorf("len(ids) = %d, want 0 for empty source", len(ids))
	}

	// Add 10 messages
	allIDs := make(map[int64]bool)
	createdIDs := f.CreateMessages(10)
	for _, id := range createdIDs {
		allIDs[id] = true
	}

	// Sample fewer than available
	ids, err = f.Store.GetRandomMessageIDs(f.Source.ID, 5)
	testutil.MustNoErr(t, err, "GetRandomMessageIDs()")
	if len(ids) != 5 {
		t.Errorf("len(ids) = %d, want 5", len(ids))
	}

	// All returned IDs should be valid
	for _, id := range ids {
		if !allIDs[id] {
			t.Errorf("returned ID %d is not in allIDs", id)
		}
	}

	// All returned IDs should be unique
	seen := make(map[int64]bool)
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate ID %d returned", id)
		}
		seen[id] = true
	}

	// Sample more than available - should return all
	ids, err = f.Store.GetRandomMessageIDs(f.Source.ID, 20)
	testutil.MustNoErr(t, err, "GetRandomMessageIDs(more than available)")
	if len(ids) != 10 {
		t.Errorf("len(ids) = %d, want 10", len(ids))
	}
}

func TestStore_GetRandomMessageIDs_ExcludesDeleted(t *testing.T) {
	f := storetest.New(t)

	// Add 5 messages
	f.CreateMessages(5)

	// Delete 2 messages
	err := f.Store.MarkMessageDeleted(f.Source.ID, "msg-0")
	testutil.MustNoErr(t, err, "MarkMessageDeleted msg-0")
	err = f.Store.MarkMessageDeleted(f.Source.ID, "msg-2")
	testutil.MustNoErr(t, err, "MarkMessageDeleted msg-2")

	// Should only return 3 (non-deleted) messages
	ids, err := f.Store.GetRandomMessageIDs(f.Source.ID, 10)
	testutil.MustNoErr(t, err, "GetRandomMessageIDs()")
	if len(ids) != 3 {
		t.Errorf("len(ids) = %d, want 3 (5 total - 2 deleted)", len(ids))
	}
}

func TestStore_ReplaceMessageRecipients_LargeBatch(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-large-recipients")

	// Create 300 participants (exceeds SQLite limit of ~249 rows with 4 params each)
	const numRecipients = 300
	participantIDs := make([]int64, numRecipients)
	displayNames := make([]string, numRecipients)
	for i := 0; i < numRecipients; i++ {
		email := fmt.Sprintf("user%d@example.com", i)
		pid := f.EnsureParticipant(email, fmt.Sprintf("User %d", i), "example.com")
		participantIDs[i] = pid
		displayNames[i] = fmt.Sprintf("User %d", i)
	}

	// This should work without hitting SQLite parameter limit
	err := f.Store.ReplaceMessageRecipients(msgID, "to", participantIDs, displayNames)
	testutil.MustNoErr(t, err, "ReplaceMessageRecipients(300 recipients)")

	f.AssertRecipientCount(msgID, "to", numRecipients)

	// Replace with a different large batch to ensure chunked delete+insert works
	err = f.Store.ReplaceMessageRecipients(msgID, "to", participantIDs[:150], displayNames[:150])
	testutil.MustNoErr(t, err, "ReplaceMessageRecipients(150 recipients)")

	f.AssertRecipientCount(msgID, "to", 150)
}

func TestStore_UpsertFTS(t *testing.T) {
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
	testutil.MustNoErr(t, err, "UpsertMessageBody")

	// Upsert FTS
	err = f.Store.UpsertFTS(msgID, "Test Subject", "hello world body text", "alice@example.com", "bob@example.com", "carol@example.com")
	testutil.MustNoErr(t, err, "UpsertFTS")

	// Verify FTS row exists and is searchable
	var count int
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'hello'").Scan(&count)
	testutil.MustNoErr(t, err, "FTS MATCH query")
	if count != 1 {
		t.Errorf("FTS match count = %d, want 1", count)
	}

	// Search by subject
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'subject'").Scan(&count)
	testutil.MustNoErr(t, err, "FTS MATCH subject")
	if count != 1 {
		t.Errorf("FTS match subject count = %d, want 1", count)
	}

	// Search by from address
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'alice'").Scan(&count)
	testutil.MustNoErr(t, err, "FTS MATCH from_addr")
	if count != 1 {
		t.Errorf("FTS match from_addr count = %d, want 1", count)
	}

	// Replace (upsert) FTS with updated content
	err = f.Store.UpsertFTS(msgID, "Updated Subject", "updated body", "alice@example.com", "bob@example.com", "")
	testutil.MustNoErr(t, err, "UpsertFTS update")

	// Old content should no longer match
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'hello'").Scan(&count)
	testutil.MustNoErr(t, err, "FTS MATCH after update")
	if count != 0 {
		t.Errorf("FTS match 'hello' after update = %d, want 0", count)
	}

	// New content should match
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'updated'").Scan(&count)
	testutil.MustNoErr(t, err, "FTS MATCH 'updated'")
	if count != 1 {
		t.Errorf("FTS match 'updated' = %d, want 1", count)
	}
}

func TestStore_BackfillFTS(t *testing.T) {
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
	testutil.MustNoErr(t, err, "UpsertMessageBody 1")

	pid1 := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
	err = f.Store.ReplaceMessageRecipients(msgID1, "from", []int64{pid1}, []string{"Sender"})
	testutil.MustNoErr(t, err, "ReplaceMessageRecipients from")

	pid2 := f.EnsureParticipant("recipient@example.com", "Recipient", "example.com")
	err = f.Store.ReplaceMessageRecipients(msgID1, "to", []int64{pid2}, []string{"Recipient"})
	testutil.MustNoErr(t, err, "ReplaceMessageRecipients to")

	msgID2 := f.CreateMessage("msg-backfill-2")
	err = f.Store.UpsertMessageBody(msgID2,
		sql.NullString{String: "second message unique content", Valid: true},
		sql.NullString{},
	)
	testutil.MustNoErr(t, err, "UpsertMessageBody 2")

	// FTS should already have been auto-populated by InitSchema, so clear it first
	_, err = f.Store.DB().Exec("DELETE FROM messages_fts")
	testutil.MustNoErr(t, err, "clear FTS")

	// Verify FTS is empty
	var count int
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts").Scan(&count)
	testutil.MustNoErr(t, err, "count FTS")
	if count != 0 {
		t.Fatalf("FTS count = %d, want 0 after clear", count)
	}

	// Run backfill
	rowsInserted, err := f.Store.BackfillFTS(nil)
	testutil.MustNoErr(t, err, "BackfillFTS")
	if rowsInserted != 2 {
		t.Errorf("BackfillFTS rows = %d, want 2", rowsInserted)
	}

	// Verify FTS is populated
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts").Scan(&count)
	testutil.MustNoErr(t, err, "count FTS after backfill")
	if count != 2 {
		t.Errorf("FTS count after backfill = %d, want 2", count)
	}

	// Search for first message body
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'first'").Scan(&count)
	testutil.MustNoErr(t, err, "FTS MATCH first")
	if count != 1 {
		t.Errorf("FTS match 'first' = %d, want 1", count)
	}

	// Search for sender email (populated via backfill from participants)
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'sender'").Scan(&count)
	testutil.MustNoErr(t, err, "FTS MATCH sender")
	if count != 1 {
		t.Errorf("FTS match 'sender' = %d, want 1", count)
	}

	// Search for second message unique content
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'unique'").Scan(&count)
	testutil.MustNoErr(t, err, "FTS MATCH unique")
	if count != 1 {
		t.Errorf("FTS match 'unique' = %d, want 1", count)
	}
}

func TestStore_FTS5Available(t *testing.T) {
	f := storetest.New(t)

	// FTS5Available should return a boolean (true on most builds)
	available := f.Store.FTS5Available()
	t.Logf("FTS5Available = %v", available)
}

func TestStore_NeedsFTSBackfill(t *testing.T) {
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
	testutil.MustNoErr(t, err, "UpsertMessageBody")

	pid := f.EnsureParticipant("autotest@example.com", "Auto", "example.com")
	err = f.Store.ReplaceMessageRecipients(msgID, "from", []int64{pid}, []string{"Auto"})
	testutil.MustNoErr(t, err, "ReplaceMessageRecipients")

	// Clear FTS to simulate a pre-existing DB without FTS population
	_, err = f.Store.DB().Exec("DELETE FROM messages_fts")
	testutil.MustNoErr(t, err, "clear FTS")

	// NeedsFTSBackfill should return true (empty FTS + existing messages)
	if !f.Store.NeedsFTSBackfill() {
		t.Fatal("NeedsFTSBackfill() = false, want true")
	}

	// Run backfill (simulating what CLI commands do after checking)
	n, err := f.Store.BackfillFTS(nil)
	testutil.MustNoErr(t, err, "BackfillFTS")
	if n == 0 {
		t.Error("BackfillFTS returned 0 rows")
	}

	// NeedsFTSBackfill should now return false
	if f.Store.NeedsFTSBackfill() {
		t.Error("NeedsFTSBackfill() = true after backfill, want false")
	}

	// Verify the backfilled data is searchable
	var count int
	err = f.Store.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'backfill'").Scan(&count)
	testutil.MustNoErr(t, err, "FTS MATCH backfill")
	if count != 1 {
		t.Errorf("FTS match 'backfill' = %d, want 1", count)
	}
}

func TestStore_ReplaceMessageLabels_LargeBatch(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("msg-large-labels")

	// Create 600 labels (exceeds SQLite limit of ~499 rows with 2 params each)
	const numLabels = 600
	labelIDs := make([]int64, numLabels)
	for i := 0; i < numLabels; i++ {
		sourceLabelID := fmt.Sprintf("Label_%d", i)
		lid, err := f.Store.EnsureLabel(f.Source.ID, sourceLabelID, fmt.Sprintf("Label %d", i), "user")
		testutil.MustNoErr(t, err, "EnsureLabel")
		labelIDs[i] = lid
	}

	// This should work without hitting SQLite parameter limit
	err := f.Store.ReplaceMessageLabels(msgID, labelIDs)
	testutil.MustNoErr(t, err, "ReplaceMessageLabels(600 labels)")

	f.AssertLabelCount(msgID, numLabels)

	// Replace with a different large batch to ensure chunked delete+insert works
	err = f.Store.ReplaceMessageLabels(msgID, labelIDs[:250])
	testutil.MustNoErr(t, err, "ReplaceMessageLabels(250 labels)")

	f.AssertLabelCount(msgID, 250)
}

func TestStore_AddMessageLabels(t *testing.T) {
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
	testutil.MustNoErr(t, err, "ReplaceMessageLabels(INBOX)")
	f.AssertLabelCount(msgID, 1)

	// Add STARRED — should go from 1 to 2
	err = f.Store.AddMessageLabels(msgID, []int64{labels["STARRED"]})
	testutil.MustNoErr(t, err, "AddMessageLabels(STARRED)")
	f.AssertLabelCount(msgID, 2)

	// Add INBOX again — should be a no-op (INSERT OR IGNORE)
	err = f.Store.AddMessageLabels(msgID, []int64{labels["INBOX"]})
	testutil.MustNoErr(t, err, "AddMessageLabels(INBOX duplicate)")
	f.AssertLabelCount(msgID, 2)

	// Add multiple labels at once, including one that already exists
	err = f.Store.AddMessageLabels(msgID, []int64{labels["SENT"], labels["TRASH"], labels["STARRED"]})
	testutil.MustNoErr(t, err, "AddMessageLabels(SENT, TRASH, STARRED)")
	f.AssertLabelCount(msgID, 4)

	// Empty list is a no-op
	err = f.Store.AddMessageLabels(msgID, []int64{})
	testutil.MustNoErr(t, err, "AddMessageLabels(empty)")
	f.AssertLabelCount(msgID, 4)
}

func TestStore_RemoveMessageLabels(t *testing.T) {
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
	testutil.MustNoErr(t, err, "ReplaceMessageLabels")
	f.AssertLabelCount(msgID, 4)

	// Remove STARRED — should go from 4 to 3
	err = f.Store.RemoveMessageLabels(msgID, []int64{labels["STARRED"]})
	testutil.MustNoErr(t, err, "RemoveMessageLabels(STARRED)")
	f.AssertLabelCount(msgID, 3)

	// Remove STARRED again — should be a no-op (already removed)
	err = f.Store.RemoveMessageLabels(msgID, []int64{labels["STARRED"]})
	testutil.MustNoErr(t, err, "RemoveMessageLabels(STARRED again)")
	f.AssertLabelCount(msgID, 3)

	// Remove multiple including INBOX and SENT
	err = f.Store.RemoveMessageLabels(msgID, []int64{labels["INBOX"], labels["SENT"]})
	testutil.MustNoErr(t, err, "RemoveMessageLabels(INBOX, SENT)")
	f.AssertLabelCount(msgID, 1)

	// Verify the remaining label is TRASH
	labelID := f.GetSingleLabelID(msgID)
	if labelID != labels["TRASH"] {
		t.Errorf("remaining label_id = %d, want %d (TRASH)", labelID, labels["TRASH"])
	}

	// Empty list is a no-op
	err = f.Store.RemoveMessageLabels(msgID, []int64{})
	testutil.MustNoErr(t, err, "RemoveMessageLabels(empty)")
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
	testutil.MustNoErr(t, err, "MarkMessagesDeletedBatch")

	// First two should be deleted, last two should not
	f.AssertMessageDeleted(internalIDs["batch-del-1"])
	f.AssertMessageDeleted(internalIDs["batch-del-2"])
	f.AssertMessageNotDeleted(internalIDs["batch-del-3"])
	f.AssertMessageNotDeleted(internalIDs["batch-del-4"])

	// Batch with non-existent IDs should not error
	err = f.Store.MarkMessagesDeletedBatch(f.Source.ID, []string{"nonexistent-1", "nonexistent-2"})
	testutil.MustNoErr(t, err, "MarkMessagesDeletedBatch(nonexistent)")

	// Empty list is a no-op
	err = f.Store.MarkMessagesDeletedBatch(f.Source.ID, []string{})
	testutil.MustNoErr(t, err, "MarkMessagesDeletedBatch(empty)")
}

func TestStore_PersistMessage(t *testing.T) {
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
	testutil.MustNoErr(t, err, "PersistMessage")
	if messageID == 0 {
		t.Fatal("message ID should be non-zero")
	}

	// Verify message fields
	got := f.GetMessageFields(messageID)
	if got.Subject != "Hello" {
		t.Errorf("subject = %q, want %q", got.Subject, "Hello")
	}

	// Verify body
	bodyText, bodyHTML := f.GetMessageBody(messageID)
	if bodyText.String != "body text" {
		t.Errorf("body_text = %q, want %q", bodyText.String, "body text")
	}
	if bodyHTML.String != "<p>body</p>" {
		t.Errorf("body_html = %q, want %q", bodyHTML.String, "<p>body</p>")
	}

	// Verify raw MIME
	raw, err := f.Store.GetMessageRaw(messageID)
	testutil.MustNoErr(t, err, "GetMessageRaw")
	if string(raw) != string(sampleRawMessage) {
		t.Errorf("raw = %q, want %q", raw, sampleRawMessage)
	}

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
	if err == nil {
		t.Fatal("PersistMessage should fail with invalid label ID")
	}

	// Verify the message was NOT committed
	existing, err := f.Store.MessageExistsBatch(f.Source.ID, []string{"persist-atomic"})
	testutil.MustNoErr(t, err, "MessageExistsBatch")
	if len(existing) != 0 {
		t.Errorf("message should not exist after failed PersistMessage, found %d", len(existing))
	}
}

func TestStore_OAuthAppColumn(t *testing.T) {
	f := storetest.New(t)

	// Default source should have null oauth_app
	if f.Source.OAuthApp.Valid {
		t.Errorf("new source OAuthApp should be null, got %q", f.Source.OAuthApp.String)
	}

	// Update oauth_app
	err := f.Store.UpdateSourceOAuthApp(f.Source.ID, sql.NullString{String: "acme", Valid: true})
	testutil.MustNoErr(t, err, "UpdateSourceOAuthApp")

	// Read it back via ListSources
	sources, err := f.Store.ListSources("")
	testutil.MustNoErr(t, err, "ListSources")

	found := false
	for _, src := range sources {
		if src.ID == f.Source.ID {
			found = true
			if !src.OAuthApp.Valid || src.OAuthApp.String != "acme" {
				t.Errorf("OAuthApp = %v, want {acme, true}", src.OAuthApp)
			}
		}
	}
	if !found {
		t.Error("source not found in ListSources")
	}
}

func TestStore_OAuthAppColumn_NullRoundTrip(t *testing.T) {
	f := storetest.New(t)

	// Set to acme
	err := f.Store.UpdateSourceOAuthApp(f.Source.ID, sql.NullString{String: "acme", Valid: true})
	testutil.MustNoErr(t, err, "UpdateSourceOAuthApp(acme)")

	// Set back to null
	err = f.Store.UpdateSourceOAuthApp(f.Source.ID, sql.NullString{})
	testutil.MustNoErr(t, err, "UpdateSourceOAuthApp(null)")

	// Verify via GetSourcesByIdentifier
	sources, err := f.Store.GetSourcesByIdentifier(f.Source.Identifier)
	testutil.MustNoErr(t, err, "GetSourcesByIdentifier")

	if len(sources) == 0 {
		t.Fatal("no sources found")
	}
	if sources[0].OAuthApp.Valid {
		t.Errorf("OAuthApp should be null after clearing, got %q", sources[0].OAuthApp.String)
	}
}

func TestStore_PersistMessage_Upsert(t *testing.T) {
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
	testutil.MustNoErr(t, err, "PersistMessage first call")

	// Update the message with different body
	msg.Subject = sql.NullString{String: "Updated", Valid: true}
	data.BodyText = sql.NullString{String: "updated body", Valid: true}

	msgID2, err := f.Store.PersistMessage(data)
	testutil.MustNoErr(t, err, "PersistMessage second call")

	if msgID2 != msgID1 {
		t.Errorf("second call ID = %d, want %d", msgID2, msgID1)
	}

	// Verify updated fields
	got := f.GetMessageFields(msgID1)
	if got.Subject != "Updated" {
		t.Errorf("subject = %q, want %q", got.Subject, "Updated")
	}

	bodyText, _ := f.GetMessageBody(msgID1)
	if bodyText.String != "updated body" {
		t.Errorf("body_text = %q, want %q", bodyText.String, "updated body")
	}
}

// --- GetStatsForScope tests ---

// makeSecondSource creates a second source and conversation in the same store as f.
func makeSecondSource(t *testing.T, f *storetest.Fixture, identifier string) (*store.Source, int64) {
	t.Helper()
	src, err := f.Store.GetOrCreateSource("gmail", identifier)
	testutil.MustNoErr(t, err, "GetOrCreateSource "+identifier)
	convID, err := f.Store.EnsureConversation(src.ID, "thread-b-1", "Thread B")
	testutil.MustNoErr(t, err, "EnsureConversation "+identifier)
	return src, convID
}

// createMessagesForSource inserts count messages under srcID/convID and returns their IDs.
func createMessagesForSource(t *testing.T, st *store.Store, srcID, convID int64, prefix string, count int) []int64 {
	t.Helper()
	ids := make([]int64, 0, count)
	for i := 0; i < count; i++ {
		id, err := st.UpsertMessage(&store.Message{
			ConversationID:  convID,
			SourceID:        srcID,
			SourceMessageID: fmt.Sprintf("%s-msg-%d", prefix, i),
			MessageType:     "email",
			SizeEstimate:    1000,
		})
		testutil.MustNoErr(t, err, fmt.Sprintf("UpsertMessage %s-%d", prefix, i))
		ids = append(ids, id)
	}
	return ids
}

func TestStore_GetStatsForScope_SingleSource(t *testing.T) {
	f := storetest.New(t)
	srcB, convB := makeSecondSource(t, f, "b@example.com")

	createMessagesForSource(t, f.Store, f.Source.ID, f.ConvID, "a", 3)
	createMessagesForSource(t, f.Store, srcB.ID, convB, "b", 2)

	// Scoped to source A only.
	statsA, err := f.Store.GetStatsForScope([]int64{f.Source.ID})
	testutil.MustNoErr(t, err, "GetStatsForScope A")
	if statsA.MessageCount != 3 {
		t.Errorf("MessageCount (A only) = %d, want 3", statsA.MessageCount)
	}
	if statsA.SourceCount != 1 {
		t.Errorf("SourceCount (A only) = %d, want 1", statsA.SourceCount)
	}

	// Scoped to both sources.
	statsAB, err := f.Store.GetStatsForScope([]int64{f.Source.ID, srcB.ID})
	testutil.MustNoErr(t, err, "GetStatsForScope A+B")
	if statsAB.MessageCount != 5 {
		t.Errorf("MessageCount (A+B) = %d, want 5", statsAB.MessageCount)
	}
	if statsAB.SourceCount != 2 {
		t.Errorf("SourceCount (A+B) = %d, want 2", statsAB.SourceCount)
	}

	// Unscoped (nil) should count all messages across both sources.
	statsAll, err := f.Store.GetStatsForScope(nil)
	testutil.MustNoErr(t, err, "GetStatsForScope nil")
	if statsAll.MessageCount != 5 {
		t.Errorf("MessageCount (nil/global) = %d, want 5", statsAll.MessageCount)
	}
	if statsAll.SourceCount != 2 {
		t.Errorf("SourceCount (nil/global) = %d, want 2", statsAll.SourceCount)
	}
}

func TestStore_GetStatsForScope_ExcludesDedupHidden(t *testing.T) {
	f := storetest.New(t)
	srcB, convB := makeSecondSource(t, f, "b-dedup@example.com")

	idsA := createMessagesForSource(t, f.Store, f.Source.ID, f.ConvID, "a-dedup", 2)
	createMessagesForSource(t, f.Store, srcB.ID, convB, "b-dedup", 1)

	// Soft-delete one message in source A via dedup (deleted_at).
	_, err := f.Store.DB().Exec(
		f.Store.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?"), idsA[0])
	testutil.MustNoErr(t, err, "set deleted_at")

	// Scoped to A: should see only the live message.
	statsA, err := f.Store.GetStatsForScope([]int64{f.Source.ID})
	testutil.MustNoErr(t, err, "GetStatsForScope A")
	if statsA.MessageCount != 1 {
		t.Errorf("MessageCount (A scoped) = %d, want 1 (dedup-hidden excluded)", statsA.MessageCount)
	}

	// Unscoped: should also exclude the dedup-hidden message (2 live, not 3).
	statsAll, err := f.Store.GetStatsForScope(nil)
	testutil.MustNoErr(t, err, "GetStatsForScope nil")
	if statsAll.MessageCount != 2 {
		t.Errorf("MessageCount (nil/global) = %d, want 2 (dedup-hidden excluded)", statsAll.MessageCount)
	}
}

func TestStore_GetStatsForScope_ExcludesSourceDeleted(t *testing.T) {
	f := storetest.New(t)
	ids := createMessagesForSource(t, f.Store, f.Source.ID, f.ConvID, "a-srcdeleted", 2)

	// Mark one as deleted from source.
	_, err := f.Store.DB().Exec(
		f.Store.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"), ids[0])
	testutil.MustNoErr(t, err, "set deleted_from_source_at")

	stats, err := f.Store.GetStatsForScope([]int64{f.Source.ID})
	testutil.MustNoErr(t, err, "GetStatsForScope")
	if stats.MessageCount != 1 {
		t.Errorf("MessageCount = %d, want 1 (source-deleted excluded)", stats.MessageCount)
	}
}
