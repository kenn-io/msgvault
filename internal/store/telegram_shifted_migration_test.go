package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTelegramShiftedHistoryRekeysMessagesAndReferences(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	channel, err := st.EnsureConversationWithType(source.ID, "-1000000000123", "group_chat", "Synthetic")
	require.NoError(err)
	former, err := st.EnsureConversationWithType(source.ID, "-789", "group_chat", "Synthetic former")
	require.NoError(err)
	other, err := st.EnsureConversationWithType(source.ID, "456", "direct_chat", "Synthetic")
	require.NoError(err)
	sender, err := st.EnsureParticipant("tg-sender@example.com", "Synthetic sender", "telegram")
	require.NoError(err)
	require.NoError(st.EnsureConversationParticipant(channel, sender, "member"))
	for _, row := range []struct {
		conversation  int64
		key, metadata string
	}{
		{channel, "-1000000000123:-999999999", `{"attachment_part_keys":["legacy-key"]}`},
		{channel, "-1000000000123:-999999998", `{"referenced_message_id":"-1000000000123:-999999999","reply_peer_verified":true}`},
		{other, "456:3", `{"referenced_message_id":"-1000000000123:-999999999","reply_peer_verified":true}`},
	} {
		message := &store.Message{SourceID: source.ID, ConversationID: row.conversation, SourceMessageID: row.key, MessageType: "telegram"}
		if row.conversation == channel {
			message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		}
		_, err := st.PersistMessage(&store.MessagePersistData{Message: message, BodyText: sql.NullString{String: "synthetic", Valid: true}, Metadata: &sql.NullString{String: row.metadata, Valid: true}})
		require.NoError(err)
	}
	var originalMessageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-1000000000123:-999999999").Scan(&originalMessageID))
	revisionBefore, err := st.DerivedDataRevisionContext(context.Background())
	require.NoError(err)
	require.NoError(st.ReconcileTelegramShiftedHistoryContext(context.Background(), source.ID, -1000000000123, 789, 100))
	revisionAfter, err := st.DerivedDataRevisionContext(context.Background())
	require.NoError(err)
	assert.Equal(revisionBefore+1, revisionAfter, "rekeyed message shards are invalidated")
	for _, key := range []string{"-789:1", "-789:2", "456:3"} {
		var metadata string
		var conversationID int64
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT metadata, conversation_id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, key).Scan(&metadata, &conversationID))
		var fields map[string]any
		require.NoError(json.Unmarshal([]byte(metadata), &fields))
		if key == "-789:1" {
			assert.Equal([]any{"legacy-key"}, fields["attachment_part_keys"])
			assert.Equal(former, conversationID)
		} else {
			assert.Equal("-789:1", fields["referenced_message_id"])
			if key == "-789:2" {
				assert.Equal(former, conversationID)
			} else {
				assert.Equal(other, conversationID)
			}
		}
	}
	var movedMessageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-789:1").Scan(&movedMessageID))
	assert.Equal(originalMessageID, movedMessageID)
	var channelCount, formerCount, formerParticipants int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT message_count FROM conversations WHERE id = ?`), channel).Scan(&channelCount))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT message_count, participant_count FROM conversations WHERE id = ?`), former).Scan(&formerCount, &formerParticipants))
	assert.Zero(channelCount)
	assert.Equal(2, formerCount)
	assert.Equal(1, formerParticipants)
	var senderMembership int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ? AND participant_id = ?`), former, sender).Scan(&senderMembership))
	assert.Equal(1, senderMembership)
	shifted, err := st.HasTelegramShiftedHistoryContext(context.Background(), source.ID)
	require.NoError(err)
	assert.False(shifted)
}

func TestTelegramShiftedHistoryIgnoresArchiveOnlyMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	conversation, err := st.EnsureConversationWithType(source.ID, "-1000000000123", "group_chat", "Synthetic")
	require.NoError(err)
	messageID, err := st.PersistMessage(&store.MessagePersistData{
		Message:  &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "-1000000000123:-999999999", MessageType: "telegram"},
		Metadata: &sql.NullString{String: `{"referenced_message_id":"-1000000000123:-999999999"}`, Valid: true},
	})
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO source_merge_archive_only_messages(message_id) VALUES (?)`), messageID)
	require.NoError(err)

	shifted, err := st.TelegramShiftedHistoryChannelsContext(context.Background(), source.ID)
	require.NoError(err)
	assert.Empty(shifted)
	revisionBefore, err := st.DerivedDataRevisionContext(context.Background())
	require.NoError(err)
	require.NoError(st.ReconcileTelegramShiftedHistoryContext(context.Background(), source.ID, -1000000000123, 789, 100))
	revisionAfter, err := st.DerivedDataRevisionContext(context.Background())
	require.NoError(err)
	assert.Equal(revisionBefore, revisionAfter, "archive-only messages do not change cache-visible facts")
	var key string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_message_id FROM messages WHERE id = ?`), messageID).Scan(&key))
	assert.Equal("-1000000000123:-999999999", key)
}

func TestTelegramShiftedHistoryCollisionRollsBackEntireRepair(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	channel, err := st.EnsureConversationWithType(source.ID, "-1000000000123", "group_chat", "Synthetic")
	require.NoError(err)
	former, err := st.EnsureConversationWithType(source.ID, "-789", "group_chat", "Synthetic former")
	require.NoError(err)
	for _, row := range []struct {
		conversation int64
		key          string
	}{{channel, "-1000000000123:-999999999"}, {channel, "-1000000000123:-999999998"}, {former, "-789:2"}} {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: row.conversation, SourceMessageID: row.key, MessageType: "telegram"}})
		require.NoError(err)
	}
	err = st.ReconcileTelegramShiftedHistoryContext(context.Background(), source.ID, -1000000000123, 789, 100)
	require.ErrorContains(err, "collision")
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id IN (?, ?)`), source.ID, "-1000000000123:-999999999", "-1000000000123:-999999998").Scan(&count))
	assert.Equal(2, count)
	link, err := st.TelegramHistoryMigrationContext(context.Background(), source.ID, -1000000000123)
	require.NoError(err)
	assert.Nil(link)
}
