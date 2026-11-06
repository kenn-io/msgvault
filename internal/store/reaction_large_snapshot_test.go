package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestReactionLargeSnapshot(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("signal", "large-reaction-snapshot")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "chat", "group_chat", "Example group")
	require.NoError(err)
	messageID := insertStoreTestMessage(t, st, source.ID, conversationID, "message")
	participantID, err := st.EnsureParticipantByIdentifier("signal", "reactor", "Example participant")
	require.NoError(err)
	reactions := make([]store.ReactionRef, 11001)
	for i := range reactions {
		reactions[i] = store.ReactionRef{ParticipantID: participantID, Type: "emoji", Value: fmt.Sprintf("reaction-%d", i)}
	}
	require.NoError(st.ReplaceReactions(messageID, reactions))
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions WHERE message_id = ?`, messageID).Scan(&count))
	assert.Equal(11001, count)
	require.NoError(st.ReplaceReactions(messageID, reactions))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions WHERE message_id = ?`, messageID).Scan(&count))
	assert.Equal(11001, count)

	require.NoError(st.ReplaceReactions(messageID, reactions[:1]))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions WHERE message_id = ?`, messageID).Scan(&count))
	assert.Equal(1, count)
	var value string
	require.NoError(st.DB().QueryRow(`SELECT reaction_value FROM reactions WHERE message_id = ?`, messageID).Scan(&value))
	assert.Equal("reaction-0", value)
}

func TestReactionSnapshotUpdates(t *testing.T) {
	t.Run("replace reactions writes only changes", func(t *testing.T) {
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
	})

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
