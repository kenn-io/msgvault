package cmd

import (
	"bytes"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/store"
)

func TestAddDiscordReadsTokenFromStdinAndSelectsSoleGuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	api := newDiscordCLIServer(t, discord.Guild{ID: testDiscordGuildA, Name: "Alpha Guild"})
	cmd := newAddDiscordLocalCmd(testDiscordCommandDeps(t, st, tokensDir, api.server.URL))
	var output bytes.Buffer
	cmd.SetIn(bytes.NewBufferString(testDiscordBotToken + "\n"))
	cmd.SetOut(&output)
	cmd.SetErr(&output)

	require.NoError(cmd.Execute())
	assert.Contains(output.String(), "Alpha Guild")
	assert.Contains(output.String(), testDiscordGuildA)
	assert.NotContains(output.String(), testDiscordBotToken)

	sources, err := st.ListSources("discord")
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(testDiscordGuildA, sources[0].Identifier)
	assert.Equal(sql.NullString{String: "Alpha Guild", Valid: true}, sources[0].DisplayName)
	assert.False(sources[0].OAuthApp.Valid)

	record, err := discord.NewTokenManager(tokensDir).Resolve("")
	require.NoError(err)
	assert.Equal(testDiscordBotID, record.BotUserID)
	assert.Equal(testDiscordBotToken, record.AccessToken())
}

func TestAddDiscordRequiresExplicitGuildWhenSeveralAreAccessible(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	require.ErrorContains(err, "--guild")
	assert.Contains(output.String(), "Alpha Guild")
	assert.Contains(output.String(), "Beta Guild")
	assert.NotContains(err.Error(), testDiscordBotToken)
	assert.NotContains(output.String(), testDiscordBotToken)
	sources, listErr := st.ListSources("discord")
	require.NoError(listErr)
	assert.Empty(sources)
}

func TestAddDiscordPromotesSoleBindingAndExistingNullSources(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	manager := discord.NewTokenManager(tokensDir)
	require.NoError(manager.Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
	existing, err := st.GetOrCreateSource("discord", testDiscordGuildB)
	require.NoError(err)
	assert.False(existing.OAuthApp.Valid)

	api := newDiscordCLIServer(t, discord.Guild{ID: testDiscordGuildA, Name: "Alpha Guild"})
	cmd := newAddDiscordLocalCmd(testDiscordCommandDeps(t, st, tokensDir, api.server.URL))
	cmd.SetIn(bytes.NewBufferString(testDiscordBotToken + "\n"))
	cmd.SetArgs([]string{"--oauth-app", "archive-bot"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	require.NoError(cmd.Execute())

	record, err := manager.Resolve("archive-bot")
	require.NoError(err)
	assert.Equal(testDiscordBotID, record.BotUserID)
	sources, err := st.ListSources("discord")
	require.NoError(err)
	require.Len(sources, 2)
	for _, source := range sources {
		assert.Equal(sql.NullString{String: "archive-bot", Valid: true}, source.OAuthApp)
	}
}

func TestAddDiscordReportsPermissionDiagnosticsWithoutExposingToken(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	api := newDiscordCLIServer(t, discord.Guild{ID: testDiscordGuildA, Name: "Alpha Guild"})
	api.fail["/channels/"+testDiscordChannel+"/users/@me/threads/archived/private"] = 403
	cmd := newAddDiscordLocalCmd(testDiscordCommandDeps(t, st, t.TempDir(), api.server.URL))
	var output bytes.Buffer
	cmd.SetIn(bytes.NewBufferString(testDiscordBotToken + "\n"))
	cmd.SetOut(&output)
	cmd.SetErr(&output)

	require.NoError(cmd.Execute())
	assert.Contains(output.String(), "Private archived threads unavailable")
	assert.NotContains(output.String(), "Member enrichment unavailable")
	assert.NotContains(output.String(), testDiscordBotToken)
	for _, requestURI := range api.requestURIs() {
		assert.NotContains(requestURI, "/members", "setup must not request the unused guild roster")
	}
}

func TestAddDiscordCredentialFirstRegistrationFailureIsIdempotentlyRecoverable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	api := newDiscordCLIServer(t, discord.Guild{ID: testDiscordGuildA, Name: "Alpha Guild"})
	deps := testDiscordCommandDeps(t, st, tokensDir, api.server.URL)
	deps.registerGuild = func(st *store.Store, guild discord.Guild, _ string) error {
		if _, err := st.GetOrCreateSource("discord", guild.ID); err != nil {
			return err
		}
		return errors.New("synthetic source registration failure")
	}
	first := newAddDiscordLocalCmd(deps)
	var firstOutput bytes.Buffer
	first.SetIn(bytes.NewBufferString(testDiscordBotToken + "\n"))
	first.SetArgs([]string{"--oauth-app", "archive-bot"})
	first.SetOut(&firstOutput)
	first.SetErr(&firstOutput)
	err := first.Execute()
	require.ErrorContains(err, "synthetic source registration failure")
	assert.NotContains(err.Error(), testDiscordBotToken)
	assert.NotContains(firstOutput.String(), testDiscordBotToken)

	record, err := discord.NewTokenManager(tokensDir).Resolve("archive-bot")
	require.NoError(err, "validated credential is the durable first phase")
	assert.Equal(testDiscordBotID, record.BotUserID)
	sources, err := st.ListSources("discord")
	require.NoError(err)
	require.Len(sources, 1)
	assert.False(sources[0].DisplayName.Valid, "interrupted registration may leave a resumable source shell")
	assert.False(sources[0].OAuthApp.Valid)

	second := newAddDiscordLocalCmd(testDiscordCommandDeps(t, st, tokensDir, api.server.URL))
	var secondOutput bytes.Buffer
	second.SetIn(bytes.NewBufferString(testDiscordBotToken + "\n"))
	second.SetArgs([]string{"--oauth-app", "archive-bot"})
	second.SetOut(&secondOutput)
	second.SetErr(&secondOutput)
	require.NoError(second.Execute())
	sources, err = st.ListSources("discord")
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(sql.NullString{String: "archive-bot", Valid: true}, sources[0].OAuthApp)
	assert.NotContains(secondOutput.String(), testDiscordBotToken)
}
