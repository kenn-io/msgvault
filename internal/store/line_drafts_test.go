package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLineChatDraftRequiresSameSourceReply(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000001")
	require.NoError(err)
	other, err := st.GetOrCreateSource("line", "2000000002")
	require.NoError(err)
	chat, err := st.EnsureConversationWithType(source.ID, "user:U00000000000000000000000000000002", "direct_chat", "Example")
	require.NoError(err)
	otherChat, err := st.EnsureConversationWithType(source.ID, "group:C00000000000000000000000000000003", "group_chat", "Example group")
	require.NoError(err)
	parent, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: chat, SourceMessageID: "100", MessageType: "line"})
	require.NoError(err)
	wrongChat, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: otherChat, SourceMessageID: "101", MessageType: "line"})
	require.NoError(err)
	wrongSource, err := st.UpsertMessage(&store.Message{SourceID: other.ID, ConversationID: chat, SourceMessageID: "102", MessageType: "line"})
	require.NoError(err)
	draft, err := st.CreateChatDraftContext(t.Context(), chat, parent, "review me", allowChatDraft)
	require.NoError(err)
	assert.Equal("100", draft.ReplyToSourceMessageID)
	assert.Equal(source.ID, draft.SourceID)
	for _, target := range []int64{0, wrongChat, wrongSource} {
		_, err := st.CreateChatDraftContext(t.Context(), chat, target, "invalid", allowChatDraft)
		require.ErrorIs(err, store.ErrChatDraftInvalidDestination)
	}
	edited, err := st.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, "reviewed")
	require.NoError(err)
	assert.Equal(int64(2), edited.Revision)
	require.NoError(st.DeleteChatDraftContext(t.Context(), draft.DraftID, 2))
}

// Hegel shrank a destination failure to target=0 and body="".
func TestLineChatDraftRejectsMissingReplyTarget(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000001")
	require.NoError(t, err)
	chat, err := st.EnsureConversationWithType(source.ID, "user:U00000000000000000000000000000002", "direct_chat", "")
	require.NoError(t, err)
	_, err = st.CreateChatDraftContext(t.Context(), chat, 0, "", allowChatDraft)
	require.ErrorIs(t, err, store.ErrChatDraftInvalidDestination)
}
