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
	assertions.Equal(known, gotKnown.Time.UTC())
	assertions.False(gotUnknown.Valid, "a source without reaction time must not fabricate one")
}

func TestReplaceReactionsWritesOnlyChanges(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("signal", "reaction-changes")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "chat", "group_chat", "Example group")
	require.NoError(err)
	messageID := insertStoreTestMessage(t, st, source.ID, conversationID, "message")
	participantID, err := st.EnsureParticipantByIdentifier("signal", "reactor", "Example participant")
	require.NoError(err)
	reactions := []store.ReactionRef{
		{ParticipantID: participantID, Type: "emoji", Value: "👍"},
		{ParticipantID: participantID, Type: "emoji", Value: "👀"},
	}
	require.NoError(st.ReplaceReactions(messageID, reactions))
	_, err = st.DB().Exec(`CREATE TABLE reaction_writes (operation TEXT);
		CREATE TRIGGER record_reaction_delete AFTER DELETE ON reactions BEGIN INSERT INTO reaction_writes VALUES ('delete'); END;
		CREATE TRIGGER record_reaction_insert AFTER INSERT ON reactions BEGIN INSERT INTO reaction_writes VALUES ('insert'); END`)
	require.NoError(err)
	require.NoError(st.ReplaceReactions(messageID, reactions))
	var writes, inserts, deletes int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reaction_writes`).Scan(&writes))
	assert.Zero(writes, "an unchanged snapshot must leave reaction rows untouched")
	_, err = st.DB().Exec(`DELETE FROM reaction_writes`)
	require.NoError(err)
	reactions[1].Value = "❤"
	require.NoError(st.ReplaceReactions(messageID, reactions))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reaction_writes WHERE operation = 'insert'`).Scan(&inserts))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reaction_writes WHERE operation = 'delete'`).Scan(&deletes))
	assert.Equal(1, inserts)
	assert.Equal(1, deletes)
}
