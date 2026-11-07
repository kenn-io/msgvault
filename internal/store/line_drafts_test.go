package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLineChatDraftRequiresArchivedReply(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000001")
	require.NoError(err)
	chat, err := st.EnsureConversationWithType(source.ID, "user:U00000000000000000000000000000002", "direct_chat", "Example")
	require.NoError(err)
	parent, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: chat, SourceMessageID: "100", MessageType: "line"})
	require.NoError(err)
	draft, err := st.CreateChatDraftContext(t.Context(), chat, parent, "review me", allowChatDraft)
	require.NoError(err)
	assert.Equal("100", draft.ReplyToSourceMessageID)
	assert.Equal(source.ID, draft.SourceID)
	_, err = st.CreateChatDraftContext(t.Context(), chat, 0, "invalid", allowChatDraft)
	require.ErrorIs(err, store.ErrChatDraftInvalidDestination)
}
