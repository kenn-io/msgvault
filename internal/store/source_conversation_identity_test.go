package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReassignSourceConversationID(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*store.Store, int64) {
		t.Helper()
		st := testutil.NewTestStore(t)
		src, err := st.GetOrCreateSource("signal", "+15555550100")
		require.NoError(t, err)
		return st, src.ID
	}
	addMessage := func(t *testing.T, st *store.Store, sourceID, conversationID int64, id string) {
		t.Helper()
		_, err := st.UpsertMessage(&store.Message{
			ConversationID: conversationID, SourceID: sourceID, SourceMessageID: id,
			MessageType: "signal", ArchivedAt: time.Now(),
		})
		require.NoError(t, err)
	}
	conversationOf := func(t *testing.T, st *store.Store, id string) string {
		t.Helper()
		var conv string
		require.NoError(t, st.DB().QueryRow(st.Rebind(`
			SELECT c.source_conversation_id FROM messages m JOIN conversations c ON c.id = m.conversation_id
			WHERE m.source_message_id = ?`), id).Scan(&conv))
		return conv
	}

	t.Run("renames the legacy conversation in place", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		st, sourceID := setup(t)
		legacy, err := st.EnsureConversationWithType(sourceID, "legacy", "direct_chat", "")
		require.NoError(err)
		addMessage(t, st, sourceID, legacy, "m1")

		found, err := st.ReassignSourceConversationIDContext(ctx, sourceID, "legacy", "current")
		require.NoError(err)
		assert.True(found)
		current, err := st.EnsureConversationWithType(sourceID, "current", "direct_chat", "")
		require.NoError(err)
		assert.Equal(legacy, current)
		assert.Equal("current", conversationOf(t, st, "m1"))
	})

	t.Run("moves messages and members when both exist", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		st, sourceID := setup(t)
		legacy, err := st.EnsureConversationWithType(sourceID, "legacy", "direct_chat", "")
		require.NoError(err)
		current, err := st.EnsureConversationWithType(sourceID, "current", "direct_chat", "")
		require.NoError(err)
		addMessage(t, st, sourceID, legacy, "m1")
		pid, err := st.EnsureParticipantByPhone("+15555550101", "", "signal")
		require.NoError(err)
		require.NoError(st.EnsureConversationParticipant(legacy, pid, "member"))

		found, err := st.ReassignSourceConversationIDContext(ctx, sourceID, "legacy", "current")
		require.NoError(err)
		assert.True(found)
		assert.Equal("current", conversationOf(t, st, "m1"))
		var members int
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ? AND participant_id = ?`),
			current, pid).Scan(&members))
		assert.Equal(1, members)
	})

	t.Run("reports a missing legacy conversation", func(t *testing.T) {
		st, sourceID := setup(t)
		found, err := st.ReassignSourceConversationIDContext(ctx, sourceID, "legacy", "current")
		require.NoError(t, err)
		assert.False(t, found)
	})
}
