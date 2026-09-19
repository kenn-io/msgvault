package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func gmailTestBuild(sourceID, conversationID int64, receipt store.GmailDraftReceipt, raw []byte) func([]int64) *store.MessagePersistData {
	return func(ids []int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{
				SourceID: sourceID, SourceMessageID: receipt.GmailMessageID,
				ConversationID: conversationID, MessageType: store.MessageTypeEmail,
				SenderID: sql.NullInt64{Int64: ids[0], Valid: true},
			},
			Conversation: &store.ConversationPersistData{
				SourceConversationID: receipt.ThreadID,
				ConversationType:     "email_thread",
				Title:                "Draft thread",
			},
			BodyText: sql.NullString{String: string(raw), Valid: true},
			RawMIME:  raw,
			Recipients: []store.RecipientSet{
				{Type: "from", ParticipantIDs: ids[:1], EmailAddresses: []string{"alice@example.com"}},
				{Type: "to", ParticipantIDs: ids[1:], EmailAddresses: []string{"user@example.com"}},
			},
		}
	}
}

func TestManagedGmailDraftLifecycleAndRetention(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "thread-1", "Draft thread")
	require.NoError(err)
	participants := []store.ParticipantPersistData{
		{EmailAddress: "alice@example.com", Domain: "example.com"},
		{EmailAddress: "user@example.com", Domain: "example.com"},
	}
	receipt := store.GmailDraftReceipt{
		SourceID: source.ID, GmailDraftID: "gmail-draft-1",
		GmailMessageID: "gmail-message-1", ThreadID: "thread-1",
	}
	draft, err := st.PersistGmailDraftContext(t.Context(), receipt, participants,
		gmailTestBuild(source.ID, conversationID, receipt, []byte("old")))
	require.NoError(err)
	require.Equal(int64(1), draft.Revision)
	require.NotEmpty(draft.DraftID)

	var labelCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_labels ml
		JOIN labels l ON l.id = ml.label_id
		WHERE ml.message_id = ? AND l.source_label_id = 'DRAFT'
	`), draft.CurrentMessageID).Scan(&labelCount))
	assert.Equal(1, labelCount)

	candidate := []byte("new")
	_, err = st.ClaimGmailDraftContext(t.Context(), draft.DraftID, 1, store.GmailDraftOperationEdit, candidate)
	require.NoError(err)
	require.NoError(st.RecordGmailDraftOutcomeContext(t.Context(), draft.DraftID, 1, "accepted_local_failed", "gmail-message-2"))
	replacement := store.GmailDraftReceipt{
		SourceID: source.ID, GmailDraftID: receipt.GmailDraftID,
		GmailMessageID: "gmail-message-2", ThreadID: receipt.ThreadID,
	}
	published, err := st.PublishGmailDraftReplacementContext(
		t.Context(), draft.DraftID, 1, replacement.GmailMessageID, participants,
		gmailTestBuild(source.ID, conversationID, replacement, candidate),
	)
	require.NoError(err)
	assert.Equal(int64(2), published.Revision)
	assert.Equal(replacement.GmailMessageID, published.CurrentReceipt.GmailMessageID)
	var deleted sql.NullTime
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at FROM messages WHERE id = ?`), draft.CurrentMessageID).Scan(&deleted))
	assert.True(deleted.Valid)

	_, err = st.ClaimGmailDraftContext(t.Context(), draft.DraftID, 2, store.GmailDraftOperationDelete, nil)
	require.NoError(err)
	loaded, err := st.GetGmailDraftContext(t.Context(), draft.DraftID)
	require.NoError(err)
	require.NotNil(loaded.Pending)
	assert.Equal(store.GmailDraftOperationDelete, loaded.Pending.Operation)

	finished, err := st.FinishGmailDraftDeleteContext(t.Context(), draft.DraftID, 2, false)
	require.NoError(err)
	assert.Equal(int64(3), finished.Revision)
	assert.NotNil(finished.DiscardedAt)
	assert.Nil(finished.Pending)

	plan, err := st.PlanGCContext(t.Context())
	require.NoError(err)
	assert.Equal(int64(1), plan.SourceDeleted)
}

func TestGmailDraftPendingOriginalRetainedByGC(t *testing.T) {
	testutil.SkipIfPostgres(t, "archive GC is SQLite-only")
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "pending@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "pending-thread", "Pending thread")
	require.NoError(err)
	participants := []store.ParticipantPersistData{
		{EmailAddress: "pending@example.com", Domain: "example.com"},
		{EmailAddress: "user@example.com", Domain: "example.com"},
	}
	receipt := store.GmailDraftReceipt{
		SourceID: source.ID, GmailDraftID: "gmail-draft-pending",
		GmailMessageID: "gmail-message-pending", ThreadID: "pending-thread",
	}
	draft, err := st.PersistGmailDraftContext(t.Context(), receipt, participants,
		gmailTestBuild(source.ID, conversationID, receipt, []byte("original")))
	require.NoError(err)
	_, err = st.ClaimGmailDraftContext(t.Context(), draft.DraftID, draft.Revision,
		store.GmailDraftOperationEdit, []byte("candidate"))
	require.NoError(err)

	// A sync deletion can arrive while the local replacement is pending.
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?
	`), draft.CurrentMessageID)
	require.NoError(err)

	plan, err := st.PlanGCContext(t.Context())
	require.NoError(err)
	assert.Zero(plan.SourceDeleted)
	assert.Empty(plan.SourceDeletedIDs)
	deleted, err := st.ExecuteGCContext(t.Context(), plan)
	require.NoError(err)
	assert.Zero(deleted)
	_, err = st.GetMessageContext(t.Context(), draft.CurrentMessageID)
	require.NoError(err)
}

func TestGmailDraftDeleteInspection404FinishesWithoutClaim(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "thread-404", "Draft thread")
	require.NoError(err)
	participants := []store.ParticipantPersistData{{EmailAddress: "alice@example.com"}, {EmailAddress: "user@example.com"}}
	receipt := store.GmailDraftReceipt{SourceID: source.ID, GmailDraftID: "gmail-draft-404", GmailMessageID: "gmail-message-404", ThreadID: "thread-404"}
	draft, err := st.PersistGmailDraftContext(t.Context(), receipt, participants, gmailTestBuild(source.ID, conversationID, receipt, []byte("body")))
	require.NoError(err)
	finished, err := st.FinishGmailDraftDeleteContext(t.Context(), draft.DraftID, draft.Revision, true)
	require.NoError(err)
	require.NotNil(finished.DiscardedAt)
	require.Equal(int64(2), finished.Revision)
}
