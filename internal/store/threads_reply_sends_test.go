package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func threadsDraftDestination(t *testing.T) (*store.Store, int64, int64, int64) {
	t.Helper()
	st, source := threadsStore(t)
	conv, err := st.EnsureConversationWithType(source, "100", "channel", "Synthetic post")
	require.NoError(t, err)
	parent, err := st.UpsertMessage(&store.Message{SourceID: source, ConversationID: conv, SourceMessageID: "100", MessageType: "threads"})
	require.NoError(t, err)
	require.NoError(t, st.SetMessageMetadata(parent, sql.NullString{String: `{"root_owned":true,"root_id":"100"}`, Valid: true}))
	return st, source, conv, parent
}

func TestThreadsDraftRequiresArchivedParent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, conv, parent := threadsDraftDestination(t)
	otherConv, err := st.EnsureConversationWithType(source, "200", "channel", "Other")
	require.NoError(err)
	other, err := st.UpsertMessage(&store.Message{SourceID: source, ConversationID: otherConv, SourceMessageID: "200", MessageType: "threads"})
	require.NoError(err)
	for _, bad := range []int64{0, other, parent + 10000} {
		_, err := st.CreateChatDraftContext(t.Context(), conv, bad, "reviewed", allowChatDraft)
		assert.ErrorIs(err, store.ErrChatDraftInvalidDestination)
	}
	draft, err := st.CreateChatDraftContext(t.Context(), conv, parent, "reviewed", allowChatDraft)
	require.NoError(err)
	assert.Equal("100", draft.ReplyToSourceMessageID)
	// A copied message with a different source must not become a reply target.
	foreign, err := st.GetOrCreateSource("threads", "threads:100:201")
	require.NoError(err)
	crossed, err := st.UpsertMessage(&store.Message{SourceID: foreign.ID, ConversationID: conv, SourceMessageID: "300", MessageType: "threads"})
	require.NoError(err)
	_, err = st.CreateChatDraftContext(t.Context(), conv, crossed, "reviewed", allowChatDraft)
	assert.ErrorIs(err, store.ErrChatDraftInvalidDestination)
	// External-root conversations remain usable for unsent review.
	_, err = st.CreateChatDraftContext(t.Context(), otherConv, other, "reviewed", allowChatDraft)
	assert.NoError(err)
}

func TestThreadsReceiptClaimAndLifecycleRaces(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, conv, parent := threadsDraftDestination(t)
	draft, err := st.CreateChatDraftContext(t.Context(), conv, parent, "immutable reviewed body", allowChatDraft)
	require.NoError(err)
	receipt, err := st.ClaimThreadsReplySendContext(t.Context(), draft.DraftID, 1)
	require.NoError(err)
	assert.Equal(draft.Body, receipt.Body)
	assert.Equal("100", receipt.RootMediaID)
	assert.Equal("100", receipt.ParentMediaID)
	require.Equal(int64(1), receipt.RowRevision)
	assert.Equal("claimed", receipt.State)
	repeated, err := st.ClaimThreadsReplySendContext(t.Context(), draft.DraftID, 1)
	require.NoError(err)
	assert.Equal(receipt, repeated)
	_, err = st.ClaimThreadsReplySendContext(t.Context(), draft.DraftID, 2)
	assert.ErrorIs(err, store.ErrChatDraftRevisionConflict)
	_, err = st.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, "retargeted")
	assert.ErrorIs(err, store.ErrChatDraftClaimed)
	assert.ErrorIs(st.DeleteChatDraftContext(t.Context(), draft.DraftID, 1), store.ErrChatDraftClaimed)
	_, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, 99, store.ThreadsReplyTransition{State: "container_saved", ContainerID: "400"})
	assert.ErrorIs(err, store.ErrThreadsReplyConflict)
	at := time.Now().UTC()
	receipt, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{State: "container_saved", ContainerID: "400", ContainerCreatedAt: &at})
	require.NoError(err)
	_, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{ContainerID: "401"})
	assert.Error(err)
	receipt, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{State: "ready"})
	require.NoError(err)
	won, err := st.TryDispatchThreadsReplyContext(t.Context(), draft.DraftID, receipt.RowRevision)
	require.NoError(err)
	require.True(won)
	won, _ = st.TryDispatchThreadsReplyContext(t.Context(), draft.DraftID, receipt.RowRevision)
	assert.False(won)
	receipt, err = st.GetThreadsReplySendContext(t.Context(), draft.DraftID)
	require.NoError(err)
	require.NotNil(receipt.PublishDispatchedAt)
	receipt, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{State: "remote_unknown"})
	require.NoError(err)
	for _, state := range []string{"ready", "claimed", "send_failed"} {
		_, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{State: state})
		assert.Error(err)
	}
	receipt, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{State: "sent_archive_pending", MediaID: "500"})
	require.NoError(err)
	_, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{MediaID: "501"})
	assert.ErrorIs(err, store.ErrThreadsReplyConflict)
	assert.Error(st.FinishThreadsReplyArchiveContext(t.Context(), draft.DraftID, 1, parent))
	message, err := st.UpsertMessage(&store.Message{SourceID: source, ConversationID: conv, SourceMessageID: "500", MessageType: "threads"})
	require.NoError(err)
	require.NoError(st.FinishThreadsReplyArchiveContext(t.Context(), draft.DraftID, 1, message))
	_, err = st.GetChatDraftContext(t.Context(), draft.DraftID)
	assert.ErrorIs(err, store.ErrChatDraftNotFound)
	receipt, err = st.ClaimThreadsReplySendContext(t.Context(), draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("complete", receipt.State)
	assert.True(receipt.ArchiveComplete)
	assert.Equal(message, receipt.LocalMessageID)
	_, err = st.ClaimThreadsReplySendContext(t.Context(), draft.DraftID, 2)
	assert.ErrorIs(err, store.ErrChatDraftRevisionConflict)
	_, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{MediaID: "501"})
	assert.Error(err)
}

func TestThreadsReceiptUnpublishedTerminalReleasesDraft(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, conv, parent := threadsDraftDestination(t)
	draft, err := st.CreateChatDraftContext(t.Context(), conv, parent, "first", allowChatDraft)
	require.NoError(err)
	receipt, err := st.ClaimThreadsReplySendContext(t.Context(), draft.DraftID, 1)
	require.NoError(err)
	receipt, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{State: "send_failed"})
	require.NoError(err)
	edited, err := st.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, "second")
	require.NoError(err)
	assert.Equal(int64(2), edited.Revision)
	_, err = st.ClaimThreadsReplySendContext(t.Context(), draft.DraftID, 2)
	assert.ErrorIs(err, store.ErrChatDraftRevisionConflict)
	old, err := st.ClaimThreadsReplySendContext(t.Context(), draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("first", old.Body)
	require.NoError(st.DeleteChatDraftContext(t.Context(), draft.DraftID, 2))
	old, err = st.GetThreadsReplySendContext(t.Context(), draft.DraftID)
	require.NoError(err)
	assert.Equal(receipt, old)
}

// Hegel's minimal operation sequence was one claim of revision 1.
func TestThreadsClaimInitialBodyRegression(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, conv, parent := threadsDraftDestination(t)
	draft, err := st.CreateChatDraftContext(t.Context(), conv, parent, "initial", allowChatDraft)
	require.NoError(err)
	got, err := st.ClaimThreadsReplySendContext(t.Context(), draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("initial", got.Body)
	assert.Equal(int64(1), got.DraftRevision)
}

func TestThreadsReceiptImmutableSourceBinding(t *testing.T) {
	for _, change := range []string{"identifier", "retired"} {
		t.Run(change, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, source, conv, parent := threadsDraftDestination(t)
			receipt := threadsReadyReceipt(t, st, conv, parent)
			var err error
			if change == "identifier" {
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET identifier=? WHERE id=?`), "threads:100:201", source)
			} else {
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO source_settings(source_id,history_only) VALUES(?,TRUE)`), source)
			}
			require.NoError(err)
			_, err = st.ClaimThreadsReplySendContext(t.Context(), receipt.DraftID, 1)
			assert.ErrorIs(err, store.ErrSourceRetired)
			_, err = st.AdvanceThreadsReplySendContext(t.Context(), receipt.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{State: "ready"})
			assert.ErrorIs(err, store.ErrSourceRetired)
			won, err := st.TryDispatchThreadsReplyContext(t.Context(), receipt.DraftID, receipt.RowRevision)
			assert.ErrorIs(err, store.ErrSourceRetired)
			assert.False(won)
			saved, err := st.GetThreadsReplySendContext(t.Context(), receipt.DraftID)
			require.NoError(err)
			assert.Equal(receipt, saved)
		})
	}
}

func TestThreadsReceiptRejectsUnstatedContainerTransition(t *testing.T) {
	st, _, conv, parent := threadsDraftDestination(t)
	draft, err := st.CreateChatDraftContext(t.Context(), conv, parent, "initial", allowChatDraft)
	require.NoError(t, err)
	receipt, err := st.ClaimThreadsReplySendContext(t.Context(), draft.DraftID, 1)
	require.NoError(t, err)
	// Recording a known container must also enter container_saved and pin its
	// creation time; otherwise a resumed claimed attempt can look uncreated.
	_, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{ContainerID: "1"})
	assert.ErrorIs(t, err, store.ErrThreadsReplyConflict)
}

// Hegel reduced the ingestion/receipt disagreement to the accepted media ID
// "01". Media strings must retain their spelling across every receipt stage.
func TestThreadsReceiptPreservesLeadingZeroMediaIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, _, _ := threadsDraftDestination(t)
	conv, err := st.EnsureConversationWithType(source, "01", "channel", "Synthetic post")
	require.NoError(err)
	parent, err := st.UpsertMessage(&store.Message{SourceID: source, ConversationID: conv, SourceMessageID: "01", MessageType: "threads"})
	require.NoError(err)
	require.NoError(st.SetMessageMetadata(parent, sql.NullString{String: `{"root_owned":true,"root_id":"01"}`, Valid: true}))
	draft, err := st.CreateChatDraftContext(t.Context(), conv, parent, "reviewed", allowChatDraft)
	require.NoError(err)
	receipt, err := st.ClaimThreadsReplySendContext(t.Context(), draft.DraftID, 1)
	require.NoError(err)
	assert.Equal("01", receipt.RootMediaID)
	assert.Equal("01", receipt.ParentMediaID)
	at := time.Now().UTC()
	receipt, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{State: "container_saved", ContainerID: "002", ContainerCreatedAt: &at})
	require.NoError(err)
	assert.Equal("002", receipt.ContainerID)
	receipt, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{State: "ready"})
	require.NoError(err)
	won, err := st.TryDispatchThreadsReplyContext(t.Context(), draft.DraftID, receipt.RowRevision)
	require.NoError(err)
	require.True(won)
	receipt, err = st.GetThreadsReplySendContext(t.Context(), draft.DraftID)
	require.NoError(err)
	receipt, err = st.AdvanceThreadsReplySendContext(t.Context(), draft.DraftID, receipt.RowRevision, store.ThreadsReplyTransition{State: "sent_archive_pending", MediaID: "003"})
	require.NoError(err)
	assert.Equal("003", receipt.MediaID)
}
