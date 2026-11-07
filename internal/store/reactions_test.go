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
	requires.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT created_at FROM reactions WHERE message_id = ? AND reaction_value = ?`), messageID, "👍").Scan(&gotKnown))
	requires.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT created_at FROM reactions WHERE message_id = ? AND reaction_value = ?`), messageID, "👀").Scan(&gotUnknown))
	requires.True(gotKnown.Valid)
	assertions.Equal(known, gotKnown.Time.UTC())
	assertions.False(gotUnknown.Valid, "a source without reaction time must not fabricate one")

	updated := known.Add(24 * time.Hour)
	requires.NoError(st.ReplaceReactions(messageID, []store.ReactionRef{
		{ParticipantID: participantID, Type: "emoji", Value: "👍", CreatedAt: updated},
		{ParticipantID: participantID, Type: "emoji", Value: "👀"},
	}))
	requires.NoError(st.DB().QueryRow(st.Rebind(`SELECT created_at FROM reactions WHERE message_id = ? AND reaction_value = ?`), messageID, "👍").Scan(&gotKnown))
	requires.True(gotKnown.Valid)
	assertions.Equal(updated, gotKnown.Time.UTC())
}

func TestReplaceReactionsRestoresRemovedReaction(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("inline", "api.inline.chat:user:42")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "chat:123", "group_chat", "Example group")
	require.NoError(err)
	messageID := insertStoreTestMessage(t, st, source.ID, conversationID, "chat:123:message:7")
	participantID, err := st.EnsureParticipantByIdentifier("inline", "api.inline.chat:user:99", "Example participant")
	require.NoError(err)
	known := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	reactions := []store.ReactionRef{{ParticipantID: participantID, Type: "emoji", Value: "👍", CreatedAt: known}}
	require.NoError(st.ReplaceReactions(messageID, reactions))
	_, err = st.DB().Exec(st.Rebind(`UPDATE reactions SET removed_at = ? WHERE message_id = ?`), known.Add(time.Hour), messageID)
	require.NoError(err)

	// The source snapshot includes the reaction again with its original time.
	require.NoError(st.ReplaceReactions(messageID, reactions))
	var createdAt, removedAt sql.NullTime
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT created_at, removed_at FROM reactions WHERE message_id = ?`), messageID).Scan(&createdAt, &removedAt))
	require.True(createdAt.Valid)
	assert.Equal(known, createdAt.Time.UTC())
	assert.False(removedAt.Valid, "a reaction in the current snapshot is active")
}
