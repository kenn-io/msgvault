package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestManagedIMAPDraftLifecycleAndRetention(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "imap://alice@example.com:143")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "draft-lifecycle", "Draft lifecycle")
	requirements.NoError(err)
	receipt := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 1}
	draft, err := st.PersistIMAPDraftContext(t.Context(), receipt, nil, func(_ []int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{
				SourceID: source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
				MessageType: store.MessageTypeEmail, ConversationID: conversationID,
			},
			BodyText: sql.NullString{String: "old", Valid: true},
			RawMIME:  []byte("From: alice@example.com\r\nTo: bob@example.com\r\nContent-Type: text/plain\r\n\r\nold\r\n"),
		}
	})
	requirements.NoError(err)
	requirements.NotEmpty(draft.DraftID)
	requirements.Equal(int64(1), draft.Revision)

	candidateRaw := []byte("candidate")
	claimed, err := st.ClaimIMAPDraftContext(t.Context(), draft.DraftID, 1, store.IMAPDraftOperationEdit, candidateRaw)
	requirements.NoError(err)
	requirements.Equal(candidateRaw, claimed.Pending.Raw)
	_, err = st.ClaimIMAPDraftContext(t.Context(), draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
	requirements.ErrorIs(err, store.ErrIMAPDraftPending)

	replacement := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 2}
	requirements.NoError(st.RecordIMAPDraftOutcomeContext(t.Context(), draft.DraftID, 1, "append_uidplus", &replacement))
	published, err := st.PublishIMAPDraftReplacementContext(t.Context(), draft.DraftID, 1, nil, func(_ []int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{
				SourceID: source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(replacement),
				MessageType: store.MessageTypeEmail, ConversationID: conversationID,
			},
			BodyText: sql.NullString{String: "candidate", Valid: true},
			RawMIME:  candidateRaw,
		}
	})
	requirements.NoError(err)
	requirements.Equal(int64(2), published.Revision)
	requirements.Equal(replacement.UID, published.CurrentReceipt.UID)
	requirements.NoError(st.RecordIMAPDraftOutcomeContext(t.Context(), draft.DraftID, 2, "survivor", nil))

	_, err = st.FinishIMAPDraftRemovalContext(t.Context(), draft.DraftID, 2)
	requirements.ErrorIs(err, store.ErrIMAPDraftState)
	requirements.NoError(st.RecordIMAPDraftOutcomeContext(t.Context(), draft.DraftID, 2, store.IMAPDraftCodeRemoved, nil))
	finished, err := st.FinishIMAPDraftRemovalContext(t.Context(), draft.DraftID, 2)
	requirements.NoError(err)
	requirements.Nil(finished.Pending)
	requirements.Equal(int64(2), finished.Revision)

	var oldDeleted sql.NullTime
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT deleted_from_source_at FROM messages WHERE id = ?
	`), draft.CurrentMessageID).Scan(&oldDeleted))
	requirements.True(oldDeleted.Valid)
}

func TestManagedIMAPDraftReplacementUIDReuse(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		uidValidity uint32
		conflict    bool
	}{
		{name: "previous generation", uidValidity: 1},
		{name: "same generation", uidValidity: 2, conflict: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("imap", "imap://alice@example.com:143")
			requirements.NoError(err)
			conversationID, err := st.EnsureConversation(source.ID, "draft-uid-reuse", "Draft UID reuse")
			requirements.NoError(err)
			build := func(receipt store.IMAPDraftReceipt, raw string) func([]int64) *store.MessagePersistData {
				return func([]int64) *store.MessagePersistData {
					return &store.MessagePersistData{
						Message: &store.Message{
							SourceID: source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
							MessageType: store.MessageTypeEmail, ConversationID: conversationID,
						},
						RawMIME: []byte(raw),
					}
				}
			}
			oldReceipt := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: scenario.uidValidity, UID: 2}
			archived, err := st.PersistIMAPDraftContext(t.Context(), oldReceipt, nil, build(oldReceipt, "archived"))
			requirements.NoError(err)
			current := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 2, UID: 1}
			draft, err := st.PersistIMAPDraftContext(t.Context(), current, nil, build(current, "current"))
			requirements.NoError(err)
			_, err = st.ClaimIMAPDraftContext(t.Context(), draft.DraftID, 1, store.IMAPDraftOperationEdit, []byte("replacement"))
			requirements.NoError(err)
			replacement := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 2, UID: 2}
			requirements.NoError(st.RecordIMAPDraftOutcomeContext(t.Context(), draft.DraftID, 1, "append_uidplus", &replacement))
			published, err := st.PublishIMAPDraftReplacementContext(t.Context(), draft.DraftID, 1, nil, build(replacement, "replacement"))
			if scenario.conflict {
				requirements.ErrorContains(err, "replacement source key already belongs")
			} else {
				requirements.NoError(err)
				assertions.Equal(int64(2), published.Revision)
				assertions.Equal(replacement, published.CurrentReceipt)
				raw, err := st.GetMessageRaw(published.CurrentMessageID)
				requirements.NoError(err)
				assertions.Equal("replacement", string(raw))
			}
			raw, err := st.GetMessageRaw(archived.CurrentMessageID)
			requirements.NoError(err)
			assertions.Equal("archived", string(raw))
		})
	}
}

func TestManagedIMAPDraftRetainedByGCWhileCurrent(t *testing.T) {
	testutil.SkipIfPostgres(t, "archive GC is SQLite-only")
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "imap://gc@example.com:143")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "draft-gc", "Draft GC")
	requirements.NoError(err)
	receipt := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 1}
	draft, err := st.PersistIMAPDraftContext(t.Context(), receipt, nil, func(_ []int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{SourceID: source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt), MessageType: store.MessageTypeEmail, ConversationID: conversationID},
			RawMIME: []byte("From: alice@example.com\r\nTo: bob@example.com\r\n\r\nbody\r\n"),
		}
	})
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), draft.CurrentMessageID)
	requirements.NoError(err)
	plan, err := st.PlanGCContext(t.Context())
	requirements.NoError(err)
	requirements.Equal(int64(0), plan.SourceDeleted)
	requirements.Empty(plan.SourceDeletedIDs)
}

func TestManagedIMAPDraftRemovalAdvancesDerivedRevisionWithSurvivingMembership(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, source, draft, _ := newReviewManagedDraft(t, "surviving-membership", 61, "body")

	archiveLabelID, err := st.EnsureLabel(source.ID, "Archive", "Archive", "user")
	requirements.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO imap_message_memberships
			(source_id, mailbox, uidvalidity, uid, message_id)
		VALUES (?, ?, ?, ?, ?)
	`), source.ID, "Archive", 1, 62, draft.CurrentMessageID)
	requirements.NoError(err)
	requirements.NoError(st.AddMessageLabels(draft.CurrentMessageID, []int64{archiveLabelID}))

	beforeRevision, err := st.DerivedDataRevision()
	requirements.NoError(err)
	_, err = st.ClaimIMAPDraftContext(
		t.Context(), draft.DraftID, draft.Revision, store.IMAPDraftOperationDelete, nil,
	)
	requirements.NoError(err)
	_, err = st.FinishIMAPDraftRemovalContext(t.Context(), draft.DraftID, draft.Revision)
	requirements.ErrorIs(err, store.ErrIMAPDraftState)
	requirements.NoError(st.RecordIMAPDraftOutcomeContext(t.Context(), draft.DraftID, draft.Revision, store.IMAPDraftCodeRemoved, nil))
	finished, err := st.FinishIMAPDraftRemovalContext(t.Context(), draft.DraftID, draft.Revision)
	requirements.NoError(err)

	afterRevision, err := st.DerivedDataRevision()
	requirements.NoError(err)
	assertions.Equal(beforeRevision+1, afterRevision)
	assertions.Equal([]string{"Archive"}, messageLabels(t, st, draft.CurrentMessageID))
	assertions.False(messageTombstoned(t, st, draft.CurrentMessageID))
	assertions.Nil(finished.Pending)

	var retainedMailbox string
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mailbox FROM imap_message_memberships
		WHERE source_id = ? AND message_id = ?
	`), source.ID, draft.CurrentMessageID).Scan(&retainedMailbox))
	assertions.Equal("Archive", retainedMailbox)
}
