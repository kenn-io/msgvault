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
	assert.Equal(t, testDiscordBotToken, record.AccessToken())
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
	require.NoError(t, manager.Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
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
	api.fail["/channels/"+testDiscordChannel+"/users/@me/threads/archived/private"] = 403
	cmd := newAddDiscordLocalCmd(testDiscordCommandDeps(t, st, t.TempDir(), api.server.URL))
	var output bytes.Buffer
	cmd.SetIn(bytes.NewBufferString(testDiscordBotToken + "\n"))
	cmd.SetOut(&output)
	cmd.SetErr(&output)

	require.NoError(t, cmd.Execute())
	assert.Contains(t, output.String(), "Private archived threads unavailable")
	assert.NotContains(t, output.String(), "Member enrichment unavailable")
	assert.NotContains(t, output.String(), testDiscordBotToken)
	for _, requestURI := range api.requestURIs() {
		assert.NotContains(t, requestURI, "/members", "setup must not request the unused guild roster")
	}
}

func TestAddDiscordCredentialFirstRegistrationFailureIsIdempotentlyRecoverable(t *testing.T) {
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
	require.ErrorContains(t, err, "synthetic source registration failure")
	assert.NotContains(t, err.Error(), testDiscordBotToken)
	assert.NotContains(t, firstOutput.String(), testDiscordBotToken)

	record, err := discord.NewTokenManager(tokensDir).Resolve("archive-bot")
	require.NoError(t, err, "validated credential is the durable first phase")
	assert.Equal(t, testDiscordBotID, record.BotUserID)
	sources, err := st.ListSources("discord")
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.False(t, sources[0].DisplayName.Valid, "interrupted registration may leave a resumable source shell")
	assert.False(t, sources[0].OAuthApp.Valid)

	second := newAddDiscordLocalCmd(testDiscordCommandDeps(t, st, tokensDir, api.server.URL))
	var secondOutput bytes.Buffer
	second.SetIn(bytes.NewBufferString(testDiscordBotToken + "\n"))
	second.SetArgs([]string{"--oauth-app", "archive-bot"})
	second.SetOut(&secondOutput)
	second.SetErr(&secondOutput)
	require.NoError(t, second.Execute())
	sources, err = st.ListSources("discord")
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, sql.NullString{String: "archive-bot", Valid: true}, sources[0].OAuthApp)
	assert.NotContains(t, secondOutput.String(), testDiscordBotToken)
}
