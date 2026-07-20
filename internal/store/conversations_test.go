package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationMetadataGetSetAndClear(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "123456789012345678")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "234567890123456789", "channel", "general",
	)
	require.NoError(err)

	metadata, err := st.GetConversationMetadata(conversationID)
	require.NoError(err)
	assert.False(metadata.Valid)

	want := sql.NullString{
		String: `{"guild_id":"123456789012345678","nsfw":false}`,
		Valid:  true,
	}
	require.NoError(st.SetConversationMetadata(conversationID, want))

	metadata, err = st.GetConversationMetadata(conversationID)
	require.NoError(err)
	assert.True(metadata.Valid)
	assert.JSONEq(want.String, metadata.String)

	require.NoError(st.SetConversationMetadata(conversationID, sql.NullString{}))
	metadata, err = st.GetConversationMetadata(conversationID)
	require.NoError(err)
	assert.False(metadata.Valid)
}

func TestConversationMetadataBatchScopesSourceAndPreservesMissingMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "guild-1")
	require.NoError(err)
	otherSource, err := st.GetOrCreateSource("discord", "guild-2")
	require.NoError(err)
	withMetadata, err := st.EnsureConversationWithType(source.ID, "thread-1", "channel", "Thread")
	require.NoError(err)
	_, err = st.EnsureConversationWithType(source.ID, "channel-1", "channel", "Channel")
	require.NoError(err)
	otherConversation, err := st.EnsureConversationWithType(otherSource.ID, "thread-1", "channel", "Other")
	require.NoError(err)
	require.NoError(st.SetConversationMetadata(withMetadata, sql.NullString{
		String: `{"parent_channel_id":"parent-1","discord_channel_type":11}`, Valid: true,
	}))
	require.NoError(st.SetConversationMetadata(otherConversation, sql.NullString{
		String: `{"parent_channel_id":"wrong-source","discord_channel_type":11}`, Valid: true,
	}))

	metadata, err := st.ConversationMetadataBatch(source.ID, []string{"thread-1", "channel-1", "missing"})
	require.NoError(err)
	require.Len(metadata, 2)
	assert.JSONEq(`{"parent_channel_id":"parent-1","discord_channel_type":11}`, metadata["thread-1"].String)
	assert.False(metadata["channel-1"].Valid)
	assert.NotContains(metadata, "missing")
}
