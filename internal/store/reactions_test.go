package store_test

import (
	"database/sql"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReplaceReactionsPreservesKnownTimestampAndStoresUnknownAsNull(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("inline", "api.inline.chat:user:42")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "chat:123", "group_chat", "Example group")
	require.NoError(err)
	messageID := insertStoreTestMessage(t, st, source.ID, conversationID, "chat:123:message:7")
	participantID, err := st.EnsureParticipantByIdentifier("inline", "api.inline.chat:user:99", "Example participant")
	require.NoError(err)
	known := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(st.ReplaceReactions(messageID, []store.ReactionRef{
		{ParticipantID: participantID, Type: "emoji", Value: "👍", CreatedAt: known},
		{ParticipantID: participantID, Type: "emoji", Value: "👀"},
	}))
	var gotKnown, gotUnknown sql.NullTime
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT created_at FROM reactions WHERE message_id = ? AND reaction_value = ?`), messageID, "👍").Scan(&gotKnown))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT created_at FROM reactions WHERE message_id = ? AND reaction_value = ?`), messageID, "👀").Scan(&gotUnknown))
	require.True(gotKnown.Valid)
	assert.Equal(known, gotKnown.Time)
	assert.False(gotUnknown.Valid, "a source without reaction time must not fabricate one")
}
