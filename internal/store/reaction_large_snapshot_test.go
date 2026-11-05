package store_test

import (
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersistLargeReactionSnapshot(t *testing.T) {
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
	})

	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	pid := f.EnsureParticipant("reactor@example.com", "Reactor", "example.com")
	reactions := make([]store.ReactionRef, 1000)
	for i := range reactions {
		reactions[i] = store.ReactionRef{ParticipantID: pid, Type: "emoji", Value: strconv.Itoa(i), CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	data := &store.MessagePersistData{
		Message:  &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "large-reactions", MessageType: "signal"},
		BodyText: sql.NullString{String: "Group message", Valid: true}, Reactions: &reactions,
	}
	id, err := f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	assert.Equal(int64(1000), data.ReactionsAdded)
	var firstID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM reactions WHERE message_id = ? AND reaction_value = '0'`), id).Scan(&firstID))
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	assert.Zero(data.ReactionsAdded)
	// Replace one reaction and change another's timestamp without changing its identity.
	reactions[0].CreatedAt = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	reactions[999].Value = "replacement"
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	assert.Equal(int64(1), data.ReactionsAdded)
	var count int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM reactions WHERE message_id = ?`), id).Scan(&count))
	assert.Equal(1000, count)
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM reactions WHERE message_id = ? AND reaction_value = '999'`), id).Scan(&count))
	assert.Zero(count)
	var keptID int64
	var createdAt time.Time
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id, created_at FROM reactions WHERE message_id = ? AND reaction_value = '0'`), id).Scan(&keptID, &createdAt))
	assert.Equal(firstID, keptID)
	assert.True(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).Equal(createdAt))
	body, err := f.Store.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("Group message", body)
	reactions = nil
	_, err = f.Store.PersistMessageContext(t.Context(), data)
	require.NoError(err)
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM reactions WHERE message_id = ?`), id).Scan(&count))
	assert.Zero(count)
}
