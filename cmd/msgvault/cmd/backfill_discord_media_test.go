package cmd

import (
	"bytes"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/store"
)

func TestBackfillDiscordMediaReportsPendingWithoutSignedURL(t *testing.T) {
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(t, discord.NewTokenManager(tokensDir).Save(discord.TokenRecord{
		BotUserID: testDiscordBotID, BotUsername: "archive-bot", AccessToken: testDiscordBotToken,
	}))
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
	cmd := newBackfillDiscordMediaLocalCmd(deps)
	var output bytes.Buffer
	cmd.SetArgs([]string{"Alpha Guild", "--only-incomplete"})
	cmd.SetOut(&output)
	cmd.SetErr(&output)

	require.NoError(t, cmd.Execute())
	assert.Contains(t, output.String(), "Messages processed: 1")
	assert.Contains(t, output.String(), "Pending: 1")
	assert.NotContains(t, output.String(), "hm=secret-signature")
	assert.NotContains(t, output.String(), testDiscordBotToken)
}
