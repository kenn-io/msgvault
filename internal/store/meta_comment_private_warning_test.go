package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetaCommentPrivateReplyWarningAfterReclaimAndGC(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, claim := actionStoreFixture(t)
	claim.Action = "private_reply"
	finishAction(t, st, claimAction(t, st, claim), "failed")
	_, err := st.DB().Exec(st.Rebind(`UPDATE meta_comment_actions SET created_at=? WHERE message_id=?`), time.Now().Add(-8*24*time.Hour), claim.MessageID)
	require.NoError(err)
	claim.TargetAuthorID = "300"
	sent := finishAction(t, st, claimAction(t, st, claim), "succeeded")
	assert.Equal(claim.TargetAuthorID, sent.TargetAuthorID)
	if st.IsPostgreSQL() {
		// GC is SQLite-only. Exercise the same ON DELETE SET NULL contract.
		_, err = st.DB().Exec(st.Rebind(`DELETE FROM messages WHERE id=?`), claim.MessageID)
		require.NoError(err)
	} else {
		require.NoError(st.MarkMessageDeleted(claim.SourceID, claim.TargetCommentID))
		plan, planErr := st.PlanGCContext(t.Context())
		require.NoError(planErr)
		deleted, deleteErr := st.ExecuteGCContext(t.Context(), plan)
		require.NoError(deleteErr)
		assert.Equal(int64(1), deleted)
	}
	rows, err := st.ListMetaCommentActionsContext(t.Context(), claim.SourceID, claim.TargetCommentID)
	require.NoError(err)
	require.Len(rows, 1)
	assert.False(rows[0].MessageID.Valid)
	since := time.Now().Add(-7 * 24 * time.Hour)
	prior, err := st.HasRecentPrivateCommentReplyToAuthorContext(t.Context(), claim.SourceID, 0, claim.TargetAuthorID, since)
	require.NoError(err)
	assert.True(prior, "the successful retry is recent even though its first claim is old")
	prior, err = st.HasRecentPrivateCommentReplyToAuthorContext(t.Context(), claim.SourceID, 0, "301", since)
	require.NoError(err)
	assert.False(prior)
	prior, err = st.HasRecentPrivateCommentReplyToAuthorContext(t.Context(), claim.SourceID+1, 0, claim.TargetAuthorID, since)
	require.NoError(err)
	assert.False(prior, "provider author IDs are source scoped")
	prior, err = st.HasRecentPrivateCommentReplyToAuthorContext(t.Context(), claim.SourceID, 0, claim.TargetAuthorID, time.Now().Add(time.Hour))
	require.NoError(err)
	assert.False(prior, "the warning window follows the dispatch time")
}
