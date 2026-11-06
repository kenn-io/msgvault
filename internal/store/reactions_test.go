package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
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
	assertions.Equal(known, gotKnown.Time.UTC())
	assertions.False(gotUnknown.Valid, "a source without reaction time must not fabricate one")

	// Persisted snapshots also refresh timestamps without counting a new reaction.
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	pid := f.EnsureParticipant("reactor@example.com", "Reactor", "example.com")
	reactions := []store.ReactionRef{{ParticipantID: pid, Type: "emoji", Value: "👍", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
	data := &store.MessagePersistData{
		Message:  &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "reaction-time", MessageType: "signal"},
		BodyText: sql.NullString{String: "Group message", Valid: true}, Reactions: &reactions,
	}
	id, err := f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	require.Equal(int64(1), data.ReactionsAdded)
	// Updating a timestamp must keep the reaction's identity and addition count.
	reactions[0].CreatedAt = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	assert.Zero(data.ReactionsAdded)
	var createdAt time.Time
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT created_at FROM reactions WHERE message_id = ? AND reaction_value = '👍'`), id).Scan(&createdAt))
	assert.True(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).Equal(createdAt))
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
