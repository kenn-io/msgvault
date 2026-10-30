package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTelegramCommonDeletionDoesNotDeleteChannelCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	for _, row := range []struct{ peer, kind, key string }{{"456", "direct_chat", "456:7"}, {"-1000000000123", "group_chat", "-1000000000123:7"}} {
		conv, err := st.EnsureConversationWithType(source.ID, row.peer, row.kind, "Synthetic")
		require.NoError(err)
		_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: row.key, MessageType: "telegram"}, BodyText: sql.NullString{String: "synthetic", Valid: true}})
		require.NoError(err)
	}
	require.NoError(st.MarkTelegramCommonDeletedContext(ctx, source.ID, 7))
	var userDeleted, channelDeleted bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "456:7").Scan(&userDeleted))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-1000000000123:7").Scan(&channelDeleted))
	assert.True(userDeleted)
	assert.False(channelDeleted)
	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "456:7").Scan(&messageID))
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), messageID).Scan(&body))
	assert.Equal("synthetic", body)
}

func TestTelegramCommonDeletionRejectsAmbiguousMatches(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	for _, row := range []struct{ peer, kind, key string }{{"456", "direct_chat", "456:7"}, {"-123", "group_chat", "-123:7"}} {
		conv, err := st.EnsureConversationWithType(source.ID, row.peer, row.kind, "Synthetic")
		require.NoError(err)
		_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: row.key, MessageType: "telegram"}})
		require.NoError(err)
	}
	require.ErrorContains(st.MarkTelegramCommonDeletedContext(ctx, source.ID, 7), "ambiguous")
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND deleted_from_source_at IS NOT NULL`), source.ID).Scan(&count))
	assert.Zero(t, count)
}

func TestTelegramCommonDeletionIgnoresMergedArchiveOnlyKeyCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("telegram", "user998")
	require.NoError(err)
	into, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	archivedConversation, err := st.EnsureConversationWithType(from.ID, "456", "direct_chat", "Synthetic archived chat")
	require.NoError(err)
	archivedID, err := st.PersistMessage(&store.MessagePersistData{
		Message:  &store.Message{SourceID: from.ID, ConversationID: archivedConversation, SourceMessageID: "456:1", MessageType: "telegram"},
		BodyText: sql.NullString{String: "archived only", Valid: true},
	})
	require.NoError(err)
	_, err = st.MergeSourcesContext(ctx, store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(err)
	var archivedKey string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_message_id FROM messages WHERE id = ?`), archivedID).Scan(&archivedKey))
	assert.Equal(fmt.Sprintf("msgvault-archive:%d:%d", from.ID, archivedID), archivedKey)

	currentConversation, err := st.EnsureConversationWithType(into.ID, "789", "direct_chat", "Synthetic current chat")
	require.NoError(err)
	currentKey := fmt.Sprintf("789:%d", archivedID)
	currentID, err := st.PersistMessage(&store.MessagePersistData{
		Message:  &store.Message{SourceID: into.ID, ConversationID: currentConversation, SourceMessageID: currentKey, MessageType: "telegram"},
		BodyText: sql.NullString{String: "current message", Valid: true},
	})
	require.NoError(err)

	require.NoError(st.MarkTelegramCommonDeletedContext(ctx, into.ID, int(archivedID)))
	var archivedDeleted, currentDeleted bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), archivedID).Scan(&archivedDeleted))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), currentID).Scan(&currentDeleted))
	assert.False(archivedDeleted, "source-merge history is not a live common-box message")
	assert.True(currentDeleted)
}
