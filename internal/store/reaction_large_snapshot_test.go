package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReactionSnapshotUpdates(t *testing.T) {
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
		CREATE TRIGGER record_reaction_insert AFTER INSERT ON reactions BEGIN INSERT INTO reaction_writes VALUES ('insert'); END;
		CREATE TRIGGER record_reaction_update AFTER UPDATE ON reactions BEGIN INSERT INTO reaction_writes VALUES ('update'); END`)
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
	var values string
	require.NoError(st.DB().QueryRow(`SELECT GROUP_CONCAT(reaction_value, ',') FROM (SELECT reaction_value FROM reactions ORDER BY reaction_value)`).Scan(&values))
	assert.Equal("❤,👍", values)
}
