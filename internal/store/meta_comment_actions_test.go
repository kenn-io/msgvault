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
	st, c := actionStoreFixture(t)
	row := claimAction(t, st, c)
	sent, err := st.MarkMetaCommentActionDispatchedContext(t.Context(), row.ID, row.RowRevision)
	require.NoError(t, err)
	assert.NotNil(t, sent.DispatchedAt)
	_, err = st.FinishMetaCommentActionContext(t.Context(), row.ID, row.RowRevision, store.MetaCommentActionOutcome{State: "succeeded"})
	assert.ErrorIs(t, err, store.ErrMetaCommentActionConflict)
	_, err = st.MarkMetaCommentActionDispatchedContext(t.Context(), sent.ID, sent.RowRevision)
	assert.ErrorIs(t, err, store.ErrMetaCommentActionConflict)
}
func TestMetaCommentActionUnknownBlocksReclaim(t *testing.T) {
	st, c := actionStoreFixture(t)
	row := finishAction(t, st, claimAction(t, st, c), "unknown")
	got, _, err := st.ClaimMetaCommentActionContext(t.Context(), c)
	assert.ErrorIs(t, err, store.ErrMetaCommentActionUnknown)
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
	st, c := actionStoreFixture(t)
	c.Action = "private_reply"
	c.Body = "Example reply"
	row := claimAction(t, st, c)
	c.ActionKey = "another-key"
	c.Body = "Different reply"
	got, existing, err := st.ClaimMetaCommentActionContext(t.Context(), c)
	require.NoError(t, err)
	assert.True(t, existing)
	assert.Equal(t, row.ID, got.ID)
	assert.Equal(t, row.Body, got.Body)
	finishAction(t, st, row, "failed")
	next := claimAction(t, st, c)
	assert.NotEqual(t, row.ID, next.ID)
	c.ActionKey = "synthetic-key"
	active, existing, err := st.ClaimMetaCommentActionContext(t.Context(), c)
	require.NoError(t, err)
	assert.True(t, existing)
	assert.Equal(t, next.ID, active.ID)
}
func TestMetaCommentActionResolveUnknown(t *testing.T) {
	for _, sent := range []bool{true, false} {
		t.Run(map[bool]string{true: "sent", false: "not-sent"}[sent], func(t *testing.T) {
			st, c := actionStoreFixture(t)
			row := finishAction(t, st, claimAction(t, st, c), "unknown")
			got, err := st.ResolveUnknownMetaCommentActionContext(t.Context(), row.ID, row.RowRevision, sent)
			require.NoError(t, err)
			if sent {
				assert.Equal(t, "succeeded", got.State)
			} else {
				assert.Equal(t, "failed", got.State)
				assert.Equal(t, "resolved_not_sent", got.ErrorCode)
			}
			_, err = st.ResolveUnknownMetaCommentActionContext(t.Context(), row.ID, row.RowRevision, sent)
			assert.ErrorIs(t, err, store.ErrMetaCommentActionConflict)
		})
	}
}
func TestMetaCommentActionSurvivesMessageDelete(t *testing.T) {
	st, c := actionStoreFixture(t)
	claimAction(t, st, c)
	_, err := st.DB().Exec(st.Rebind("DELETE FROM messages WHERE id=?"), c.MessageID)
	require.NoError(t, err)
	rows, err := st.ListMetaCommentActionsContext(t.Context(), c.SourceID, c.TargetCommentID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].MessageID.Valid)
}
func TestMetaCommentActionListDerivesArchive(t *testing.T) {
	st, c := actionStoreFixture(t)
	c.Action = "public_reply"
	c.Body = "Reply"
	row := claimAction(t, st, c)
	row, err := st.MarkMetaCommentActionDispatchedContext(t.Context(), row.ID, row.RowRevision)
	require.NoError(t, err)
	row, err = st.FinishMetaCommentActionContext(t.Context(), row.ID, row.RowRevision, store.MetaCommentActionOutcome{State: "succeeded", RemoteID: "100_200_400"})
	require.NoError(t, err)
	var conv int64
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT conversation_id FROM messages WHERE id=?"), c.MessageID).Scan(&conv))
	id, err := st.UpsertMessage(&store.Message{SourceID: c.SourceID, ConversationID: conv, SourceMessageID: "100_200_400", MessageType: "facebook_comments"})
	require.NoError(t, err)
	rows, err := st.ListMetaCommentActionsContext(t.Context(), c.SourceID, c.TargetCommentID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].ArchiveComplete)
	assert.Equal(t, sql.NullInt64{Int64: id, Valid: true}, rows[0].ReplyMessageID)
	assert.Equal(t, row.RowRevision, rows[0].RowRevision)
	assert.True(t, row.UpdatedAt.Equal(rows[0].UpdatedAt))
	var saved sql.NullInt64
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT reply_message_id FROM meta_comment_actions WHERE id=?"), row.ID).Scan(&saved))
	assert.False(t, saved.Valid, "GET must not repair stored receipt")
}
func TestMetaCommentActionDeleteKeyNoNonce(t *testing.T) {
	st, c := actionStoreFixture(t)
	row := finishAction(t, st, claimAction(t, st, c), "succeeded")
	got, existing, err := st.ClaimMetaCommentActionContext(t.Context(), c)
	require.NoError(t, err)
	assert.True(t, existing)
	assert.Equal(t, row.ID, got.ID)
	assert.Equal(t, "succeeded", got.State)
}
func TestMetaCommentActionSchemaParityPG(t *testing.T) {
	st, c := actionStoreFixture(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL profile exercises real schema parity")
	}
	for _, action := range []string{"block", "unblock"} {
		c.Action = action
		c.ActionKey = action
		claimAction(t, st, c)
	}
	c.Action = "private_reply"
	c.ActionKey = "first"
	row := claimAction(t, st, c)
	c.ActionKey = "second"
	got, existing, err := st.ClaimMetaCommentActionContext(t.Context(), c)
	require.NoError(t, err)
	assert.True(t, existing)
	assert.Equal(t, row.ID, got.ID)
}

func TestMetaCommentActionUnblockGeneration(t *testing.T) {
	st, c := actionStoreFixture(t)
	c.Action, c.TargetAuthorID = "unblock", "300"
	first := finishAction(t, st, claimAction(t, st, c), "succeeded")
	c.ActionKey = "later-failed-unblock"
	finishAction(t, st, claimAction(t, st, c), "failed")
	for _, tc := range []struct {
		source             int64
		identifier, author string
		want               int64
	}{
		{c.SourceID, c.SourceIdentifier, "300", first.ID},
		{c.SourceID + 100, c.SourceIdentifier, "300", 0},
		{c.SourceID, "another-source", "300", 0},
		{c.SourceID, c.SourceIdentifier, "301", 0},
	} {
		got, _, err := st.LatestSuccessfulMetaCommentUnblockContext(t.Context(), tc.source, tc.identifier, tc.author)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
}
