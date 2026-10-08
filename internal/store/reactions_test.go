package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReplaceReactionsPreservesKnownTimestampAndStoresUnknownAsNull(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("inline", "api.inline.chat:user:42")
	requires.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "chat:123", "group_chat", "Example group")
	requires.NoError(err)
	messageID := insertStoreTestMessage(t, st, source.ID, conversationID, "chat:123:message:7")
	participantID, err := st.EnsureParticipantByIdentifier("inline", "api.inline.chat:user:99", "Example participant")
	requires.NoError(err)
	known := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	requires.NoError(st.ReplaceReactions(messageID, []store.ReactionRef{
		{ParticipantID: participantID, Type: "emoji", Value: "👍", CreatedAt: known},
		{ParticipantID: participantID, Type: "emoji", Value: "👀"},
	}))
	var gotKnown, gotUnknown sql.NullTime
	requires.NoError(st.DB().QueryRow(st.Rebind(`SELECT created_at FROM reactions WHERE message_id = ? AND reaction_value = ?`), messageID, "👍").Scan(&gotKnown))
	requires.NoError(st.DB().QueryRow(st.Rebind(`SELECT created_at FROM reactions WHERE message_id = ? AND reaction_value = ?`), messageID, "👀").Scan(&gotUnknown))
	requires.True(gotKnown.Valid)
	assertions.Equal(known, gotKnown.Time)
	assertions.False(gotUnknown.Valid, "a source without reaction time must not fabricate one")
}
