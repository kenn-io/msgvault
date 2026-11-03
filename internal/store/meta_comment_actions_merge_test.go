package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestMetaCommentActionArchiveEvidenceSurvivesSourceMerges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, claim := actionStoreFixture(t)
	claim.Action = "public_reply"
	row := claimAction(t, st, claim)
	row, err := st.MarkMetaCommentActionDispatchedContext(t.Context(), row.ID, row.RowRevision)
	require.NoError(err)
	_, err = st.FinishMetaCommentActionContext(t.Context(), row.ID, row.RowRevision, store.MetaCommentActionOutcome{State: "succeeded", RemoteID: "100_200_400"})
	require.NoError(err)
	var conv int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_id FROM messages WHERE id=?`), claim.MessageID).Scan(&conv))
	replyID, err := st.UpsertMessage(&store.Message{SourceID: claim.SourceID, ConversationID: conv, SourceMessageID: "100_200_400", MessageType: "facebook_comments", Snippet: sql.NullString{String: "Example sent reply", Valid: true}})
	require.NoError(err)
	before, err := st.GetMetaCommentActionByKeyContext(t.Context(), claim.ActionKey)
	require.NoError(err)
	require.True(before.ArchiveComplete)
	require.Equal(replyID, before.ReplyMessageID.Int64)

	from := claim.SourceID
	for _, identifier := range []string{"app:9:page:101", "app:9:page:102"} {
		into, createErr := st.GetOrCreateSource("facebook_comments", identifier)
		require.NoError(createErr)
		_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from, IntoSourceID: into.ID})
		require.NoError(err)
		saved, readErr := st.GetMetaCommentActionByKeyContext(t.Context(), claim.ActionKey)
		require.NoError(readErr)
		assert.True(saved.ArchiveComplete)
		assert.Equal(sql.NullInt64{Int64: replyID, Valid: true}, saved.ReplyMessageID)
		assert.Equal(claim.SourceID, saved.SourceID, "receipt keeps its original provider audit scope")
		assert.Equal(claim.SourceIdentifier, saved.SourceIdentifier)
		assert.Equal("100_200_400", saved.RemoteID)
		rows, listErr := st.ListMessageMetaCommentActionsContext(t.Context(), claim.MessageID)
		require.NoError(listErr)
		require.Len(rows, 1)
		assert.Equal(saved, rows[0])
		from = into.ID
	}
}

func TestCommentQueuePrivateReplyEvidenceSurvivesSourceMerge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, claim, _ := queueFixture(t)
	claim.Action = "private_reply"
	finishAction(t, st, claimAction(t, st, claim), "succeeded")
	into, err := st.GetOrCreateSource("facebook_comments", "app:9:page:101")
	require.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: claim.SourceID, IntoSourceID: into.ID})
	require.NoError(err)
	rows, err := st.ListCommentQueueContext(t.Context(), store.CommentQueueFilter{SourceID: into.ID})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(claim.MessageID, rows[0].MessageID)
	assert.Nil(rows[0].PrivateReplyOpenUntil, "a source merge must not reopen an already-used private reply window")
}
