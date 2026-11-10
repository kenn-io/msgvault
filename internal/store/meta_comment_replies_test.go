package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMetaCommentReplyRecords(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("facebook_comments", "app:9:page:100")
	require.NoError(err)
	latest := func() (store.MetaCommentReply, error) {
		return st.LatestMetaCommentReplyContext(t.Context(), source.ID, "100_201", "Thanks")
	}
	_, err = latest()
	require.ErrorIs(err, sql.ErrNoRows)

	refused, err := st.StartMetaCommentReplyContext(t.Context(), source.ID, "100_201", "Thanks")
	require.NoError(err)
	require.NoError(st.FinishMetaCommentReplyContext(t.Context(), refused, store.MetaCommentReplyFailed, ""))
	_, err = latest()
	require.ErrorIs(err, sql.ErrNoRows, "a failed reply does not block a resend")

	sending, err := st.StartMetaCommentReplyContext(t.Context(), source.ID, "100_201", "Thanks")
	require.NoError(err)
	got, err := latest()
	require.NoError(err)
	assert.Equal(store.MetaCommentReply{ID: sending, State: store.MetaCommentReplySending}, got)
	_, err = st.LatestMetaCommentReplyContext(t.Context(), source.ID, "100_201", "Other text")
	require.ErrorIs(err, sql.ErrNoRows, "only the same text is matched")

	require.NoError(st.FinishMetaCommentReplyContext(t.Context(), sending, store.MetaCommentReplySent, "100_202"))
	sent, err := st.SentMetaCommentReplyContext(t.Context(), source.ID, "100_202")
	require.NoError(err)
	assert.True(sent)
	sent, err = st.SentMetaCommentReplyContext(t.Context(), source.ID, "100_299")
	require.NoError(err)
	assert.False(sent)
	require.NoError(st.FinishMetaCommentReplyContext(t.Context(), sending, store.MetaCommentReplyFailed, ""))
	got, err = latest()
	require.NoError(err)
	assert.Equal(store.MetaCommentReplySent, got.State, "a finished reply keeps its outcome")
}
