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

func TestDiscordExportReturnsBoundedStableHistory(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("discord", "guild-1")
	require.NoError(t, err)
	other, err := st.GetOrCreateSource("discord", "guild-2")
	require.NoError(t, err)

	insertDiscordExportConversation(t, st, source.ID, "200", "later", `{"guild_id":"guild-1","discord_channel_type":11,"parent_channel_id":"20","thread":{"archived":true}}`)
	firstConversation := insertDiscordExportConversation(t, st, source.ID, "100", "general", `{"guild_id":"guild-1","discord_channel_type":0}`)
	otherConversation := insertDiscordExportConversation(t, st, other.ID, "100", "other", `{"guild_id":"guild-2","discord_channel_type":0}`)

	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	insertDiscordExportMessage(t, st, source.ID, firstConversation, "1001", start, "Second body", "bob", true, false)
	insertDiscordExportMessage(t, st, source.ID, firstConversation, "1000", start, "Full rendered body https://example.test/a", "alice", false, false)
	insertDiscordExportMessage(t, st, source.ID, firstConversation, "0999", start.Add(-time.Nanosecond), "before", "alice", false, false)
	insertDiscordExportMessage(t, st, source.ID, firstConversation, "1002", end, "at end", "alice", false, false)
	insertDiscordExportMessage(t, st, source.ID, firstConversation, "1003", start.Add(time.Hour), "dedup hidden", "alice", false, true)
	insertDiscordExportMessage(t, st, other.ID, otherConversation, "1000", start, "other source", "mallory", false, false)

	got, err := st.DiscordExport(context.Background(), source.ID, start, end)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []string{"100", "200"}, []string{got[0].ID, got[1].ID})
	require.Len(t, got[0].Messages, 2)
	assert.Equal(t, []string{"1000", "1001"}, []string{got[0].Messages[0].ID, got[0].Messages[1].ID})
	assert.Equal(t, "Full rendered body https://example.test/a", got[0].Messages[0].Content)
	assert.Equal(t, "alice", got[0].Messages[0].AuthorHandle)
	assert.Equal(t, []string{"https://example.test/a"}, got[0].Messages[0].URLs)
	assert.True(t, got[0].Messages[1].Deleted)
	assert.Equal(t, "channel", got[0].Kind)
	assert.False(t, got[0].Archived)
	assert.Equal(t, "thread", got[1].Kind)
	assert.True(t, got[1].Archived)
	assert.Equal(t, "20", got[1].ParentID)
}

func insertDiscordExportConversation(
	t *testing.T, st *store.Store, sourceID int64, id, name, metadata string,
) int64 {
	t.Helper()
	result, err := st.DB().Exec(st.Rebind(`
		INSERT INTO conversations (
			source_id, source_conversation_id, conversation_type, title, metadata
		) VALUES (?, ?, 'channel', ?, ?)
	`), sourceID, id, name, metadata)
	require.NoError(t, err)
	conversationID, err := result.LastInsertId()
	require.NoError(t, err)
	return conversationID
}

func insertDiscordExportMessage(
	t *testing.T,
	st *store.Store,
	sourceID, conversationID int64,
	id string,
	sentAt time.Time,
	body, author string,
	sourceDeleted, hidden bool,
) {
	t.Helper()
	var deletedFromSource any
	if sourceDeleted {
		deletedFromSource = sentAt.Add(time.Hour)
	}
	var deletedAt any
	if hidden {
		deletedAt = sentAt.Add(time.Hour)
	}
	result, err := st.DB().Exec(st.Rebind(`
		INSERT INTO messages (
			conversation_id, source_id, source_message_id, message_type,
			sent_at, received_at, metadata, deleted_from_source_at, deleted_at
		) VALUES (?, ?, ?, 'discord', ?, ?, ?, ?, ?)
	`), conversationID, sourceID, id, sentAt, sentAt,
		`{"author_display_name":"`+author+`"}`, deletedFromSource, deletedAt)
	require.NoError(t, err)
	messageID, err := result.LastInsertId()
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)
	`), messageID, body)
	require.NoError(t, err)
}
