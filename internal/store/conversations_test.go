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
	assert.Equal(want, metadata)

	require.NoError(st.SetConversationMetadata(conversationID, sql.NullString{}))
	metadata, err = st.GetConversationMetadata(conversationID)
	require.NoError(err)
	assert.False(metadata.Valid)
}
