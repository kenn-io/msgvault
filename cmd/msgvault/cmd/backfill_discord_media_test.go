package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/store"
)

type cancelAfterFirstErrContext struct {
	context.Context

	checks int
	cancel context.CancelFunc
}

func (c *cancelAfterFirstErrContext) Err() error {
	c.checks++
	if c.checks > 1 {
		c.cancel()
	}
	return c.Context.Err()
}

func newDiscordPendingMediaMessage(t *testing.T, st *store.Store, sourceID int64) int64 {
	t.Helper()
	conversationID, err := st.EnsureConversationWithType(sourceID, testDiscordChannel, "channel", "general")
	require.NoError(t, err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: sourceID, ConversationID: conversationID,
		SourceMessageID: "400000000000000001", MessageType: "discord",
	})
	require.NoError(t, err)
	require.NoError(t, st.ReplaceMessageDiscordAttachments(messageID, []store.AttachmentRef{{
		Filename: "file.bin", Size: 5, StoragePath: "discord:pending:500000000000000001",
		SourceAttachmentID: "discord:500000000000000001", MediaType: "document",
		State: attachmentpolicy.StatePending,
	}}))
	return messageID
}

func discordAttachmentOutcome(t *testing.T, st *store.Store, messageID int64) (state, reason string) {
	t.Helper()
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT attachment_state, attachment_skip_reason FROM attachments WHERE message_id = ?
	`), messageID).Scan(&state, &reason))
	return state, reason
}

func TestBackfillDiscordMediaReportsSizePolicySkipWithoutSignedURL(t *testing.T) {
	req := require.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	req.NoError(discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
	source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	req.NoError(err)
	req.NoError(st.UpdateSourceDisplayName(source.ID, "Alpha Guild"))
	conversationID, err := st.EnsureConversationWithType(source.ID, testDiscordChannel, "channel", "general")
	req.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "400000000000000001", MessageType: "discord",
		SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
	})
	req.NoError(err)
	signedURL := "https://cdn.discordapp.com/attachments/300000000000000001/500000000000000001/file.bin?hm=secret-signature"
	req.NoError(st.ReplaceMessageDiscordAttachments(messageID, []store.AttachmentRef{{
		Filename: "file.bin", Size: 100, StoragePath: signedURL,
		SourceAttachmentID: "discord:500000000000000001", MediaType: "document",
	}}))
	completedMessageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "400000000000000002", MessageType: "discord",
		SentAt: sql.NullTime{Time: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Valid: true},
	})
	req.NoError(err)
	completedHash := strings.Repeat("ab", 32)
	req.NoError(st.ReplaceMessageDiscordAttachments(completedMessageID, []store.AttachmentRef{{
		Filename: "complete.bin", Size: 100,
		StoragePath: completedHash[:2] + "/" + completedHash, ContentHash: completedHash,
		SourceAttachmentID: "discord:500000000000000002", MediaType: "document",
	}}))

	api := newDiscordCLIServer(t)
	api.messages[testDiscordChannel] = []discord.Message{{
		ID: "400000000000000001", ChannelID: testDiscordChannel,
		Attachments: []discord.Attachment{{
			ID: "500000000000000001", Filename: "file.bin", Size: 100,
			URL: signedURL,
		}},
	}}
	deps := testDiscordCommandDeps(t, st, tokensDir, api.server.URL)
	deps.providerConfig = func() config.DiscordConfig { return config.DiscordConfig{MaxMediaBytes: 1} }
	for _, tt := range []struct {
		name          string
		args          []string
		wantProcessed string
	}{
		{name: "only incomplete", args: []string{"Alpha Guild", "--only-incomplete"}, wantProcessed: "Messages processed: 1"},
		{name: "all attachment messages", args: []string{"Alpha Guild"}, wantProcessed: "Messages processed: 2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			cmd := newBackfillDiscordMediaLocalCmd(deps)
			var output bytes.Buffer
			cmd.SetArgs(tt.args)
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			require.NoError(cmd.Execute())
			assert.Contains(output.String(), tt.wantProcessed)
			assert.Contains(output.String(), "Skipped: 1")
			assert.Contains(output.String(), "Attachment warnings: 1")
			assert.Contains(output.String(), "Size cap exceeded: 1")
			assert.NotContains(output.String(), "hm=secret-signature")
			assert.NotContains(output.String(), testDiscordBotToken)
		})
	}
}

func TestBackfillDiscordMediaReportsSanitizedRefreshAndUnrecoverableWarnings(t *testing.T) {
	for _, tt := range []struct {
		name        string
		status      int
		discordCode int
		wantOutcome string
		wantWarning string
	}{
		{
			name: "refresh forbidden", status: http.StatusForbidden, discordCode: 50013,
			wantOutcome: "Pending: 1", wantWarning: "Refresh unavailable: 1",
		},
		{
			name: "message gone", status: http.StatusNotFound, discordCode: 10003,
			wantOutcome: "Unrecoverable: 1", wantWarning: "Attachment unrecoverable: 1",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := newDiscordCLIStore(t)
			tokensDir := t.TempDir()
			require.NoError(discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
			source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
			require.NoError(err)
			conversationID, err := st.EnsureConversationWithType(source.ID, testDiscordChannel, "channel", "general")
			require.NoError(err)
			messageID, err := st.UpsertMessage(&store.Message{
				SourceID: source.ID, ConversationID: conversationID,
				SourceMessageID: "400000000000000001", MessageType: "discord",
			})
			require.NoError(err)
			signedURL := "https://cdn.discordapp.com/attachments/300000000000000001/500000000000000001/file.bin?hm=private-signature"
			require.NoError(st.ReplaceMessageDiscordAttachments(messageID, []store.AttachmentRef{{
				StoragePath: signedURL, SourceAttachmentID: "discord:500000000000000001",
			}}))
			api := newDiscordCLIServer(t)
			path := "/channels/" + testDiscordChannel + "/messages/400000000000000001"
			api.fail[path] = tt.status
			api.failCode[path] = tt.discordCode
			cmd := newBackfillDiscordMediaLocalCmd(testDiscordCommandDeps(t, st, tokensDir, api.server.URL))
			var output bytes.Buffer
			cmd.SetArgs([]string{testDiscordGuildA, "--only-incomplete"})
			cmd.SetOut(&output)
			cmd.SetErr(&output)

			require.NoError(cmd.Execute())
			assert.Contains(output.String(), tt.wantOutcome)
			assert.Contains(output.String(), "Attachment warnings: 1")
			assert.Contains(output.String(), tt.wantWarning)
			assert.NotContains(output.String(), "private-signature")
			assert.NotContains(output.String(), testDiscordBotToken)
		})
	}
}

func TestBackfillDiscordMediaUsesArchivedParticipantCount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
	source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, testDiscordChannel, "channel", "general")
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE conversations SET participant_count = ? WHERE id = ?`), 20, conversationID)
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "400000000000000001", MessageType: "discord",
	})
	require.NoError(err)
	require.NoError(st.ReplaceMessageDiscordAttachments(messageID, []store.AttachmentRef{{
		Filename: "file.bin", Size: 5, StoragePath: "discord:pending:500000000000000001",
		SourceAttachmentID: "discord:500000000000000001", MediaType: "document",
		State: attachmentpolicy.StatePending,
	}}))
	api := newDiscordCLIServer(t)
	api.messages[testDiscordChannel] = []discord.Message{{
		ID: "400000000000000001", ChannelID: testDiscordChannel,
		Attachments: []discord.Attachment{{ID: "500000000000000001", Filename: "file.bin", Size: 5}},
	}}
	deps := testDiscordCommandDeps(t, st, tokensDir, api.server.URL)
	deps.providerConfig = func() config.DiscordConfig {
		return config.DiscordConfig{MediaMaxParticipants: 8}
	}

	summary, err := backfillDiscordSourceMedia(t.Context(), st, source, deps, false)
	require.NoError(err)
	assert.Equal(int64(1), summary.Skipped)
	var state, reason string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT attachment_state, attachment_skip_reason FROM attachments WHERE message_id = ?
	`), messageID).Scan(&state, &reason))
	assert.Equal(string(attachmentpolicy.StateSkipped), state)
	assert.Equal(string(attachmentpolicy.SkipParticipantThreshold), reason)
}

func TestBackfillDiscordMediaUsesGuildMemberCountWhenArchivedCountIsUnderIt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
	source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(err)
	messageID := newDiscordPendingMediaMessage(t, st, source.ID)
	api := newDiscordCLIServer(t)
	// An ordinary guild channel archives no membership of its own, so only the
	// guild's own count can exclude its media.
	api.guildMemberCount = 12
	api.messages[testDiscordChannel] = []discord.Message{{
		ID: "400000000000000001", ChannelID: testDiscordChannel,
		Attachments: []discord.Attachment{{ID: "500000000000000001", Filename: "file.bin", Size: 5}},
	}}
	deps := testDiscordCommandDeps(t, st, tokensDir, api.server.URL)
	deps.providerConfig = func() config.DiscordConfig {
		return config.DiscordConfig{MediaMaxParticipants: 8}
	}

	summary, err := backfillDiscordSourceMedia(t.Context(), st, source, deps, false)
	require.NoError(err)
	assert.Equal(int64(1), summary.Skipped)
	assert.False(summary.MembershipUnavailable)
	state, reason := discordAttachmentOutcome(t, st, messageID)
	assert.Equal(string(attachmentpolicy.StateSkipped), state)
	assert.Equal(string(attachmentpolicy.SkipParticipantThreshold), reason)
}

func TestBackfillDiscordMediaFailsClosedWhenGuildMembershipIsUnreadable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
	source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(err)
	messageID := newDiscordPendingMediaMessage(t, st, source.ID)
	api := newDiscordCLIServer(t)
	api.fail["/guilds/"+testDiscordGuildA] = http.StatusForbidden
	api.failCode["/guilds/"+testDiscordGuildA] = 50001
	api.messages[testDiscordChannel] = []discord.Message{{
		ID: "400000000000000001", ChannelID: testDiscordChannel,
		Attachments: []discord.Attachment{{ID: "500000000000000001", Filename: "file.bin", Size: 5}},
	}}
	deps := testDiscordCommandDeps(t, st, tokensDir, api.server.URL)
	deps.providerConfig = func() config.DiscordConfig {
		return config.DiscordConfig{MediaMaxParticipants: 8}
	}
	cmd := newBackfillDiscordMediaLocalCmd(deps)
	var output bytes.Buffer
	cmd.SetArgs([]string{testDiscordGuildA})
	cmd.SetOut(&output)
	cmd.SetErr(&output)

	require.NoError(cmd.Execute())
	assert.Contains(output.String(), "Skipped: 1")
	assert.Contains(output.String(), "Guild membership unavailable")
	state, reason := discordAttachmentOutcome(t, st, messageID)
	assert.Equal(string(attachmentpolicy.StateSkipped), state)
	assert.Equal(string(attachmentpolicy.SkipParticipantThreshold), reason)
}

// TestBackfillDiscordMediaArchivesGuildMembershipOutcome pins that the
// backfill's own guild lookup lands in the archive: an unreadable lookup marks
// the guild's rosters unresolved so purge retains rather than trusting a stale
// count, and a readable one clears the marker and reconciles the floor before
// the retry selection runs, so a guild that shrank releases what it excluded.
func TestBackfillDiscordMediaArchivesGuildMembershipOutcome(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
	source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(err)
	messageID := newDiscordPendingMediaMessage(t, st, source.ID)
	conversationID, err := st.EnsureConversationWithType(source.ID, testDiscordChannel, "channel", "general")
	require.NoError(err)
	// A floor archived while the guild was large.
	require.NoError(st.SetConversationMetadata(conversationID, sql.NullString{
		String: `{"guild_id":"` + testDiscordGuildA + `","discord_channel_type":0,"member_count":40}`, Valid: true,
	}))
	api := newDiscordCLIServer(t)
	api.fail["/guilds/"+testDiscordGuildA] = http.StatusForbidden
	api.failCode["/guilds/"+testDiscordGuildA] = 50001
	api.messages[testDiscordChannel] = []discord.Message{{
		ID: "400000000000000001", ChannelID: testDiscordChannel,
		Attachments: []discord.Attachment{{ID: "500000000000000001", Filename: "file.bin", Size: 5}},
	}}
	deps := testDiscordCommandDeps(t, st, tokensDir, api.server.URL)
	deps.providerConfig = func() config.DiscordConfig {
		return config.DiscordConfig{MediaMaxParticipants: 8}
	}
	metadataOf := func() string {
		metadata, err := st.GetConversationMetadata(conversationID)
		require.NoError(err)
		return metadata.String
	}

	summary, err := backfillDiscordSourceMedia(t.Context(), st, source, deps, false)
	require.NoError(err)
	assert.True(summary.MembershipUnavailable)
	assert.Equal(int64(1), summary.Skipped)
	state, reason := discordAttachmentOutcome(t, st, messageID)
	assert.Equal(string(attachmentpolicy.StateSkipped), state)
	assert.Equal(string(attachmentpolicy.SkipParticipantThreshold), reason)
	assert.JSONEq(`{"guild_id":"`+testDiscordGuildA+`","discord_channel_type":0,"member_count":40,"member_count_unknown":true}`,
		metadataOf(), "an unreadable lookup marks the roster unresolved and keeps the count for reference")
	candidates, err := st.ListAttachmentPolicyCandidates(t.Context())
	require.NoError(err)
	assert.Empty(candidates, "a skipped marker holds no stored media")

	delete(api.fail, "/guilds/"+testDiscordGuildA)
	delete(api.failCode, "/guilds/"+testDiscordGuildA)
	api.guildMemberCount = 5
	summary, err = backfillDiscordSourceMedia(t.Context(), st, source, deps, true)
	require.NoError(err)
	assert.False(summary.MembershipUnavailable)
	assert.Equal(int64(1), summary.MessagesProcessed,
		"the readable lookup is archived before retry selection, so the shrunken guild's skip is revisited")
	assert.Zero(summary.Skipped)
	assert.JSONEq(`{"guild_id":"`+testDiscordGuildA+`","discord_channel_type":0,"member_count":5}`,
		metadataOf(), "a readable lookup clears the marker and lowers the archived floor")
}

func TestBackfillDiscordMediaReturnsCancellationAfterFinalMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
	source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, testDiscordChannel, "channel", "general")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "400000000000000001", MessageType: "discord",
	})
	require.NoError(err)
	completedHash := strings.Repeat("ab", 32)
	require.NoError(st.ReplaceMessageDiscordAttachments(messageID, []store.AttachmentRef{{
		Filename: "complete.bin", Size: 100,
		StoragePath: completedHash[:2] + "/" + completedHash, ContentHash: completedHash,
		SourceAttachmentID: "discord:500000000000000001", MediaType: "document",
	}}))
	baseContext, cancel := context.WithCancel(context.Background())
	ctx := &cancelAfterFirstErrContext{Context: baseContext, cancel: cancel}
	deps := testDiscordCommandDeps(t, st, tokensDir, newDiscordCLIServer(t).server.URL)

	summary, err := backfillDiscordSourceMedia(ctx, st, source, deps, false)
	require.ErrorIs(err, context.Canceled)
	assert.Equal(int64(1), summary.MessagesProcessed)
	assert.GreaterOrEqual(ctx.checks, 2, "cancellation must be checked after the final message")
}
