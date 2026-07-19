package cmd

import (
	"bytes"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/discord"
)

func TestAddDiscordReadsTokenFromStdinAndSelectsSoleGuild(t *testing.T) {
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	api := newDiscordCLIServer(t, discord.Guild{ID: testDiscordGuildA, Name: "Alpha Guild"})
	cmd := newAddDiscordLocalCmd(testDiscordCommandDeps(t, st, tokensDir, api.server.URL))
	var output bytes.Buffer
	cmd.SetIn(bytes.NewBufferString(testDiscordBotToken + "\n"))
	cmd.SetOut(&output)
	cmd.SetErr(&output)

	require.NoError(t, cmd.Execute())
	assert.Contains(t, output.String(), "Alpha Guild")
	assert.Contains(t, output.String(), testDiscordGuildA)
	assert.NotContains(t, output.String(), testDiscordBotToken)

	sources, err := st.ListSources("discord")
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, testDiscordGuildA, sources[0].Identifier)
	assert.Equal(t, sql.NullString{String: "Alpha Guild", Valid: true}, sources[0].DisplayName)
	assert.False(t, sources[0].OAuthApp.Valid)

	record, err := discord.NewTokenManager(tokensDir).Resolve("")
	require.NoError(t, err)
	assert.Equal(t, testDiscordBotID, record.BotUserID)
	assert.Equal(t, testDiscordBotToken, record.AccessToken)
}

func TestAddDiscordRequiresExplicitGuildWhenSeveralAreAccessible(t *testing.T) {
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	api := newDiscordCLIServer(t,
		discord.Guild{ID: testDiscordGuildA, Name: "Alpha Guild"},
		discord.Guild{ID: testDiscordGuildB, Name: "Beta Guild"},
	)
	cmd := newAddDiscordLocalCmd(testDiscordCommandDeps(t, st, tokensDir, api.server.URL))
	var output bytes.Buffer
	cmd.SetIn(bytes.NewBufferString(testDiscordBotToken + "\n"))
	cmd.SetOut(&output)
	cmd.SetErr(&output)

	err := cmd.Execute()
	require.ErrorContains(t, err, "--guild")
	assert.Contains(t, output.String(), "Alpha Guild")
	assert.Contains(t, output.String(), "Beta Guild")
	assert.NotContains(t, err.Error(), testDiscordBotToken)
	assert.NotContains(t, output.String(), testDiscordBotToken)
	sources, listErr := st.ListSources("discord")
	require.NoError(t, listErr)
	assert.Empty(t, sources)
}

func TestAddDiscordPromotesSoleBindingAndExistingNullSources(t *testing.T) {
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	manager := discord.NewTokenManager(tokensDir)
	require.NoError(t, manager.Save(discord.TokenRecord{
		BotUserID: testDiscordBotID, BotUsername: "archive-bot", AccessToken: testDiscordBotToken,
	}))
	existing, err := st.GetOrCreateSource("discord", testDiscordGuildB)
	require.NoError(t, err)
	assert.False(t, existing.OAuthApp.Valid)

	api := newDiscordCLIServer(t, discord.Guild{ID: testDiscordGuildA, Name: "Alpha Guild"})
	cmd := newAddDiscordLocalCmd(testDiscordCommandDeps(t, st, tokensDir, api.server.URL))
	cmd.SetIn(bytes.NewBufferString(testDiscordBotToken + "\n"))
	cmd.SetArgs([]string{"--oauth-app", "archive-bot"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	require.NoError(t, cmd.Execute())

	record, err := manager.Resolve("archive-bot")
	require.NoError(t, err)
	assert.Equal(t, testDiscordBotID, record.BotUserID)
	sources, err := st.ListSources("discord")
	require.NoError(t, err)
	require.Len(t, sources, 2)
	for _, source := range sources {
		assert.Equal(t, sql.NullString{String: "archive-bot", Valid: true}, source.OAuthApp)
	}
}

func TestAddDiscordReportsPermissionDiagnosticsWithoutExposingToken(t *testing.T) {
	st := newDiscordCLIStore(t)
	api := newDiscordCLIServer(t, discord.Guild{ID: testDiscordGuildA, Name: "Alpha Guild"})
	api.fail["/guilds/"+testDiscordGuildA+"/members"] = 403
	api.fail["/channels/"+testDiscordChannel+"/users/@me/threads/archived/private"] = 403
	cmd := newAddDiscordLocalCmd(testDiscordCommandDeps(t, st, t.TempDir(), api.server.URL))
	var output bytes.Buffer
	cmd.SetIn(bytes.NewBufferString(testDiscordBotToken + "\n"))
	cmd.SetOut(&output)
	cmd.SetErr(&output)

	require.NoError(t, cmd.Execute())
	assert.Contains(t, output.String(), "Member enrichment unavailable")
	assert.Contains(t, output.String(), "Private archived threads unavailable")
	assert.NotContains(t, output.String(), testDiscordBotToken)
}
