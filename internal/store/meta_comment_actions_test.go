package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func actionStoreFixture(t *testing.T) (*store.Store, store.MetaCommentActionClaim) {
	t.Helper()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("facebook_comments", "app:9:page:100")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(src.ID, "100_200", "Example post")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "100_200_300", MessageType: "facebook_comments"})
	require.NoError(t, err)
	return st, store.MetaCommentActionClaim{ActionKey: "synthetic-key", SourceID: src.ID, SourceIdentifier: src.Identifier, MessageID: id, TargetCommentID: "100_200_300", Action: "delete"}
}
func claimAction(t *testing.T, st *store.Store, c store.MetaCommentActionClaim) store.MetaCommentAction {
	t.Helper()
	row, existing, err := st.ClaimMetaCommentActionContext(t.Context(), c)
	require.NoError(t, err)
	require.False(t, existing)
	return row
}
func finishAction(t *testing.T, st *store.Store, row store.MetaCommentAction, state string) store.MetaCommentAction {
	t.Helper()
	row, err := st.MarkMetaCommentActionDispatchedContext(t.Context(), row.ID, row.RowRevision)
	require.NoError(t, err)
	row, err = st.FinishMetaCommentActionContext(t.Context(), row.ID, row.RowRevision, store.MetaCommentActionOutcome{State: state})
	require.NoError(t, err)
	return row
}
func TestMetaCommentActionClaimIdempotent(t *testing.T) {
	st, c := actionStoreFixture(t)
	row := claimAction(t, st, c)
	again, existing, err := st.ClaimMetaCommentActionContext(t.Context(), c)
	require.NoError(t, err)
	assert.True(t, existing)
	assert.Equal(t, row.ID, again.ID)
}
func TestMetaCommentActionLifecycleCAS(t *testing.T) {
	require := require.New(t)
	st, c := actionStoreFixture(t)
	row := claimAction(t, st, c)
	sent, err := st.MarkMetaCommentActionDispatchedContext(t.Context(), row.ID, row.RowRevision)
	require.NoError(err)
	assert.NotNil(t, sent.DispatchedAt)
	_, err = st.FinishMetaCommentActionContext(t.Context(), row.ID, row.RowRevision, store.MetaCommentActionOutcome{State: "succeeded"})
	require.ErrorIs(err, store.ErrMetaCommentActionConflict)
	_, err = st.MarkMetaCommentActionDispatchedContext(t.Context(), sent.ID, sent.RowRevision)
	require.ErrorIs(err, store.ErrMetaCommentActionConflict)
}
func TestMetaCommentActionUnknownBlocksReclaim(t *testing.T) {
	st, c := actionStoreFixture(t)
	row := finishAction(t, st, claimAction(t, st, c), "unknown")
	got, _, err := st.ClaimMetaCommentActionContext(t.Context(), c)
	require.ErrorIs(t, err, store.ErrMetaCommentActionUnknown)
	assert.Equal(t, row.ID, got.ID)
}
func TestMetaCommentActionFailedReclaim(t *testing.T) {
	st, c := actionStoreFixture(t)
	row := finishAction(t, st, claimAction(t, st, c), "failed")
	got := claimAction(t, st, c)
	assert.Equal(t, row.ID, got.ID)
	assert.Greater(t, got.RowRevision, row.RowRevision)
	assert.Nil(t, got.DispatchedAt)
}
func TestMetaCommentActionPrivateReplyUnique(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, c := actionStoreFixture(t)
	c.Action = "private_reply"
	c.Body = "Example reply"
	row := claimAction(t, st, c)
	c.ActionKey = "another-key"
	c.Body = "Different reply"
	got, existing, err := st.ClaimMetaCommentActionContext(t.Context(), c)
	require.NoError(err)
	assert.True(existing)
	assert.Equal(row.ID, got.ID)
	assert.Equal(row.Body, got.Body)
	finishAction(t, st, row, "failed")
	next := claimAction(t, st, c)
	assert.NotEqual(row.ID, next.ID)
	c.ActionKey = "synthetic-key"
	active, existing, err := st.ClaimMetaCommentActionContext(t.Context(), c)
	require.NoError(err)
	assert.True(existing)
	assert.Equal(next.ID, active.ID)
}
func TestMetaCommentActionResolveUnknown(t *testing.T) {
	for _, sent := range []bool{true, false} {
		t.Run(map[bool]string{true: "sent", false: "not-sent"}[sent], func(t *testing.T) {
			assert := assert.New(t)
			st, c := actionStoreFixture(t)
			row := finishAction(t, st, claimAction(t, st, c), "unknown")
			got, err := st.ResolveUnknownMetaCommentActionContext(t.Context(), row.ID, row.RowRevision, sent)
			require.NoError(t, err)
			if sent {
				assert.Equal("succeeded", got.State)
			} else {
				assert.Equal("failed", got.State)
				assert.Equal("resolved_not_sent", got.ErrorCode)
			}
			_, err = st.ResolveUnknownMetaCommentActionContext(t.Context(), row.ID, row.RowRevision, sent)
			assert.ErrorIs(err, store.ErrMetaCommentActionConflict)
		})
	}
}
func TestMetaCommentActionSurvivesMessageDelete(t *testing.T) {
	require := require.New(t)
	st, c := actionStoreFixture(t)
	claimAction(t, st, c)
	_, err := st.DB().Exec(st.Rebind("DELETE FROM messages WHERE id=?"), c.MessageID)
	require.NoError(err)
	rows, err := st.ListMetaCommentActionsContext(t.Context(), c.SourceID, c.TargetCommentID)
	require.NoError(err)
	require.Len(rows, 1)
	assert.False(t, rows[0].MessageID.Valid)
}
func TestMetaCommentActionListDerivesArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, c := actionStoreFixture(t)
	c.Action = "public_reply"
	c.Body = "Reply"
	row := claimAction(t, st, c)
	row, err := st.MarkMetaCommentActionDispatchedContext(t.Context(), row.ID, row.RowRevision)
	require.NoError(err)
	row, err = st.FinishMetaCommentActionContext(t.Context(), row.ID, row.RowRevision, store.MetaCommentActionOutcome{State: "succeeded", RemoteID: "100_200_400"})
	require.NoError(err)
	var conv int64
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT conversation_id FROM messages WHERE id=?"), c.MessageID).Scan(&conv))
	id, err := st.UpsertMessage(&store.Message{SourceID: c.SourceID, ConversationID: conv, SourceMessageID: "100_200_400", MessageType: "facebook_comments"})
	require.NoError(err)
	rows, err := st.ListMetaCommentActionsContext(t.Context(), c.SourceID, c.TargetCommentID)
	require.NoError(err)
	require.Len(rows, 1)
	assert.True(rows[0].ArchiveComplete)
	assert.Equal(sql.NullInt64{Int64: id, Valid: true}, rows[0].ReplyMessageID)
	assert.Equal(row.RowRevision, rows[0].RowRevision)
	assert.True(row.UpdatedAt.Equal(rows[0].UpdatedAt))
	var saved sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT reply_message_id FROM meta_comment_actions WHERE id=?"), row.ID).Scan(&saved))
	assert.False(saved.Valid, "GET must not repair stored receipt")
}
func TestMetaCommentActionDeleteKeyNoNonce(t *testing.T) {
	assert := assert.New(t)
	st, c := actionStoreFixture(t)
	row := finishAction(t, st, claimAction(t, st, c), "succeeded")
	got, existing, err := st.ClaimMetaCommentActionContext(t.Context(), c)
	require.NoError(t, err)
	assert.True(existing)
	assert.Equal(row.ID, got.ID)
	assert.Equal("succeeded", got.State)
}
func TestMergeSourcesCommentActionSafety(t *testing.T) {
	for _, state := range []string{"claimed", "dispatched", "unknown", "succeeded", "failed"} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, c := actionStoreFixture(t)
			into, err := st.GetOrCreateSource("facebook_comments", "app:8:page:200")
			require.NoError(err)
			row := claimAction(t, st, c)
			switch state {
			case "dispatched":
				row, err = st.MarkMetaCommentActionDispatchedContext(t.Context(), row.ID, row.RowRevision)
				require.NoError(err)
			case "unknown", "succeeded", "failed":
				row = finishAction(t, st, row, state)
			}
			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: c.SourceID, IntoSourceID: into.ID})
			if state == "claimed" || state == "dispatched" || state == "unknown" {
				require.ErrorIs(err, store.ErrSourceMergeInvalid)
				return
			}
			require.NoError(err)
			saved, err := st.GetMetaCommentActionByKeyContext(t.Context(), c.ActionKey)
			require.NoError(err)
			assert.Equal(c.SourceID, saved.SourceID)
			assert.Equal(c.SourceIdentifier, saved.SourceIdentifier)
			assert.Equal(row.ID, saved.ID)
		})
	}
}

func TestMetaCommentActionResolveSentPreservesPartialEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, claim := actionStoreFixture(t)
	claim.Action = "private_reply"
	row := claimAction(t, st, claim)
	row, err := st.MarkMetaCommentActionDispatchedContext(t.Context(), row.ID, row.RowRevision)
	require.NoError(err)
	row, err = st.FinishMetaCommentActionContext(t.Context(), row.ID, row.RowRevision, store.MetaCommentActionOutcome{State: "unknown", RemoteID: "400", RemoteRecipientID: "300"})
	require.NoError(err)
	row, err = st.ResolveUnknownMetaCommentActionContext(t.Context(), row.ID, row.RowRevision, true)
	require.NoError(err)
	assert.Equal("succeeded", row.State)
	assert.Equal("400", row.RemoteID)
	assert.Equal("300", row.RemoteRecipientID)
	saved, err := st.GetMetaCommentActionByKeyContext(t.Context(), claim.ActionKey)
	require.NoError(err)
	assert.Equal(row.RemoteID, saved.RemoteID)
	assert.Equal(row.RemoteRecipientID, saved.RemoteRecipientID)
}
