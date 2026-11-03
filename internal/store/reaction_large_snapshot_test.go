package store_test

import (
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersistLargeReactionSnapshot(t *testing.T) {
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
