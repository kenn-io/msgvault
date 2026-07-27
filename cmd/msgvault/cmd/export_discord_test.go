package cmd

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestExportDiscordWritesVersionedBoundedJSON(t *testing.T) {
	st := newDiscordCLIStore(t)
	source, err := st.GetOrCreateSource(sourceTypeDiscord, testDiscordGuildA)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceDisplayName(source.ID, "Alpha Guild"))
	conversationID := insertDiscordCLIExportConversation(t, st, source.ID)
	start := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	insertDiscordCLIExportMessage(t, st, source.ID, conversationID, start)
	runID, err := st.StartSync(source.ID, sourceTypeDiscord)
	require.NoError(t, err)
	require.NoError(t, st.CompleteSync(runID, "complete"))

	cmd := newExportDiscordLocalCmd(testDiscordCommandDeps(t, st, "", ""))
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{
		"Alpha Guild",
		"--start", "2026-07-19T20:00:00-04:00",
		"--end", "2026-07-21T00:00:00Z",
		"--format", "json",
	})
	require.NoError(t, cmd.Execute())

	type testGuild struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var got struct {
		Schema                string                         `json:"schema"`
		MsgvaultVersion       string                         `json:"msgvault_version"`
		SourceSyncCompletedAt time.Time                      `json:"source_sync_completed_at"`
		Guild                 testGuild                      `json:"guild"`
		Start                 time.Time                      `json:"start"`
		End                   time.Time                      `json:"end"`
		Containers            []store.DiscordExportContainer `json:"containers"`
	}
	require.NoError(t, json.Unmarshal(output.Bytes(), &got))
	assert.Equal(t, "msgvault-discord-export/1", got.Schema)
	assert.Equal(t, Version, got.MsgvaultVersion)
	assert.False(t, got.SourceSyncCompletedAt.IsZero())
	assert.Equal(t, testDiscordGuildA, got.Guild.ID)
	assert.Equal(t, "Alpha Guild", got.Guild.Name)
	assert.Equal(t, start, got.Start)
	assert.Equal(t, start.Add(24*time.Hour), got.End)
	require.Len(t, got.Containers, 1)
	require.Len(t, got.Containers[0].Messages, 1)
	assert.Equal(t, "400000000000000001", got.Containers[0].Messages[0].ID)
}

func TestExportDiscordRequiresSuccessfulSync(t *testing.T) {
	st := newDiscordCLIStore(t)
	_, err := st.GetOrCreateSource(sourceTypeDiscord, testDiscordGuildA)
	require.NoError(t, err)

	cmd := newExportDiscordLocalCmd(testDiscordCommandDeps(t, st, "", ""))
	cmd.SetArgs([]string{
		testDiscordGuildA,
		"--start", "2026-07-20T00:00:00Z",
		"--end", "2026-07-21T00:00:00Z",
	})
	err = cmd.Execute()
	require.ErrorContains(t, err, "successful Discord sync")
}

func TestExportDiscordRejectsInvalidBounds(t *testing.T) {
	cmd := newExportDiscordLocalCmd(discordCommandDeps{})
	cmd.SetArgs([]string{
		testDiscordGuildA,
		"--start", "2026-07-21T00:00:00Z",
		"--end", "2026-07-20T00:00:00Z",
	})
	err := cmd.Execute()
	require.ErrorContains(t, err, "--start must be before --end")
}

func insertDiscordCLIExportConversation(
	t *testing.T, st *store.Store, sourceID int64,
) int64 {
	t.Helper()
	var conversationID int64
	err := st.DB().QueryRow(st.Rebind(`
		INSERT INTO conversations (
			source_id, source_conversation_id, conversation_type, title, metadata
		) VALUES (?, ?, 'channel', 'general', ?)
		RETURNING id
	`), sourceID, testDiscordChannel,
		`{"guild_id":"`+testDiscordGuildA+`","discord_channel_type":0}`,
	).Scan(&conversationID)
	require.NoError(t, err)
	return conversationID
}

func insertDiscordCLIExportMessage(
	t *testing.T,
	st *store.Store,
	sourceID, conversationID int64,
	sentAt time.Time,
) {
	t.Helper()
	var messageID int64
	err := st.DB().QueryRow(st.Rebind(`
		INSERT INTO messages (
			conversation_id, source_id, source_message_id, message_type,
			sent_at, received_at, metadata
		) VALUES (?, ?, '400000000000000001', 'discord', ?, ?, ?)
		RETURNING id
	`), conversationID, sourceID, sentAt, sentAt,
		`{"author_display_name":"synthetic-user"}`,
	).Scan(&messageID)
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)
	`), messageID, "A complete message body https://example.test/item")
	require.NoError(t, err)
}
