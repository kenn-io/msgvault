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

func TestBackfillDiscordMediaReportsPendingWithoutSignedURL(t *testing.T) {
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(t, discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
	source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceDisplayName(source.ID, "Alpha Guild"))
	conversationID, err := st.EnsureConversationWithType(source.ID, testDiscordChannel, "channel", "general")
	require.NoError(t, err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "400000000000000001", MessageType: "discord",
		SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(t, err)
	signedURL := "https://cdn.discordapp.com/attachments/300000000000000001/500000000000000001/file.bin?hm=secret-signature"
	require.NoError(t, st.ReplaceMessageDiscordAttachments(messageID, []store.AttachmentRef{{
		Filename: "file.bin", Size: 100, StoragePath: signedURL,
		SourceAttachmentID: "discord:500000000000000001", MediaType: "document",
	}}))
	completedMessageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "400000000000000002", MessageType: "discord",
		SentAt: sql.NullTime{Time: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(t, err)
	completedHash := strings.Repeat("ab", 32)
	require.NoError(t, st.ReplaceMessageDiscordAttachments(completedMessageID, []store.AttachmentRef{{
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
		{name: "all attachment messages", args: []string{"Alpha Guild"}, wantProcessed: "Messages processed: 2"},
		{name: "only incomplete", args: []string{"Alpha Guild", "--only-incomplete"}, wantProcessed: "Messages processed: 1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newBackfillDiscordMediaLocalCmd(deps)
			var output bytes.Buffer
			cmd.SetArgs(tt.args)
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			require.NoError(t, cmd.Execute())
			assert.Contains(t, output.String(), tt.wantProcessed)
			assert.Contains(t, output.String(), "Pending: 1")
			assert.Contains(t, output.String(), "Attachment warnings: 1")
			assert.Contains(t, output.String(), "Size cap exceeded: 1")
			assert.NotContains(t, output.String(), "hm=secret-signature")
			assert.NotContains(t, output.String(), testDiscordBotToken)
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
			st := newDiscordCLIStore(t)
			tokensDir := t.TempDir()
			require.NoError(t, discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
			source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
			require.NoError(t, err)
			conversationID, err := st.EnsureConversationWithType(source.ID, testDiscordChannel, "channel", "general")
			require.NoError(t, err)
			messageID, err := st.UpsertMessage(&store.Message{
				SourceID: source.ID, ConversationID: conversationID,
				SourceMessageID: "400000000000000001", MessageType: "discord",
			})
			require.NoError(t, err)
			signedURL := "https://cdn.discordapp.com/attachments/300000000000000001/500000000000000001/file.bin?hm=private-signature"
			require.NoError(t, st.ReplaceMessageDiscordAttachments(messageID, []store.AttachmentRef{{
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

			require.NoError(t, cmd.Execute())
			assert.Contains(t, output.String(), tt.wantOutcome)
			assert.Contains(t, output.String(), "Attachment warnings: 1")
			assert.Contains(t, output.String(), tt.wantWarning)
			assert.NotContains(t, output.String(), "private-signature")
			assert.NotContains(t, output.String(), testDiscordBotToken)
		})
	}
}

func TestBackfillDiscordMediaReturnsCancellationAfterFinalMessage(t *testing.T) {
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(t, discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
	source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(t, err)
	conversationID, err := st.EnsureConversationWithType(source.ID, testDiscordChannel, "channel", "general")
	require.NoError(t, err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "400000000000000001", MessageType: "discord",
	})
	require.NoError(t, err)
	completedHash := strings.Repeat("ab", 32)
	require.NoError(t, st.ReplaceMessageDiscordAttachments(messageID, []store.AttachmentRef{{
		Filename: "complete.bin", Size: 100,
		StoragePath: completedHash[:2] + "/" + completedHash, ContentHash: completedHash,
		SourceAttachmentID: "discord:500000000000000001", MediaType: "document",
	}}))
	baseContext, cancel := context.WithCancel(context.Background())
	ctx := &cancelAfterFirstErrContext{Context: baseContext, cancel: cancel}
	deps := testDiscordCommandDeps(t, st, tokensDir, newDiscordCLIServer(t).server.URL)

	summary, err := backfillDiscordSourceMedia(ctx, st, source, deps, false)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int64(1), summary.MessagesProcessed)
	assert.GreaterOrEqual(t, ctx.checks, 2, "cancellation must be checked after the final message")
}
