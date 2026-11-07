package youtube

import (
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestResolveReplyTargetUsesTopLevelEvidenceAndRejectsRoots(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/owner-1")
	require.NoError(err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "video-1", "Example video")
	require.NoError(err)
	add := func(id, meta string) int64 {
		msgID, e := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: id, MessageType: "comment"})
		require.NoError(e)
		require.NoError(st.SetMessageMetadata(msgID, sql.NullString{String: meta, Valid: true}))
		return msgID
	}
	root := add("video:video-1", `{"provider":"youtube","channel_id":"owner-1","video_id":"video-1"}`)
	parent := add("parent-1", `{"provider":"youtube","channel_id":"owner-1","video_id":"video-1","comment_id":"parent-1","top_level_comment_id":"parent-1","moderation_status":"published","thread_id":"thread-1","can_reply":true}`)
	nested := add("nested-1", `{"provider":"youtube","channel_id":"owner-1","video_id":"video-1","comment_id":"nested-1","top_level_comment_id":"parent-1","moderation_status":"heldForReview"}`)
	target, err := ResolveReplyTarget(t.Context(), st, nested)
	require.NoError(err)
	assert.Equal("parent-1", target.ParentID)
	assert.Equal("published", target.ParentStatus)
	assert.True(target.CanReply)
	_, err = ResolveReplyTarget(t.Context(), st, root)
	require.Error(err)
	require.NoError(st.SetMessageMetadata(parent, sql.NullString{String: `{"provider":"youtube","channel_id":"owner-1","video_id":"video-1","comment_id":"parent-1","top_level_comment_id":"parent-1","moderation_status":"heldForReview","thread_id":"thread-1","can_reply":true}`, Valid: true}))
	heldTarget, err := ResolveReplyTarget(t.Context(), st, nested)
	require.NoError(err)
	require.Error(validateReply(heldTarget, "Example reply"))
}

func TestResolveReplyTargetRejectsCrossVideoMetadata(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/owner-1")
	require.NoError(err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "video-1", "Example video")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "parent-1", MessageType: "comment"})
	require.NoError(err)
	require.NoError(st.SetMessageMetadata(id, sql.NullString{Valid: true, String: `{"provider":"youtube","channel_id":"owner-1","video_id":"another-video","comment_id":"parent-1","top_level_comment_id":"parent-1","moderation_status":"published","thread_id":"thread-1","can_reply":true}`}))
	_, err = ResolveReplyTarget(t.Context(), st, id)
	require.Error(err)
}
