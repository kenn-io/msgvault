package cmd

import (
	"bytes"
	"database/sql"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/discord"
)

func TestSyncDiscordNoArgumentContinuesAfterFailureInSourceIDOrder(t *testing.T) {
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(t, discord.NewTokenManager(tokensDir).Save(discord.TokenRecord{
		BotUserID: testDiscordBotID, BotUsername: "archive-bot", AccessToken: testDiscordBotToken,
	}))
	first, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(t, err)
	second, err := st.GetOrCreateSource("discord", testDiscordGuildB)
	require.NoError(t, err)
	require.Less(t, first.ID, second.ID)
	api := newDiscordCLIServer(t)
	api.fail["/guilds/"+testDiscordGuildA] = http.StatusForbidden

	cmd := newSyncDiscordLocalCmd(testDiscordCommandDeps(t, st, tokensDir, api.server.URL))
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	err = cmd.Execute()
	require.ErrorContains(t, err, testDiscordGuildA)
	assert.Contains(t, output.String(), testDiscordGuildA)
	assert.Contains(t, output.String(), testDiscordGuildB)
	assert.NotContains(t, output.String(), testDiscordBotToken)

	requests := api.requestURIs()
	var guildRequests []string
	for _, request := range requests {
		if request == "/guilds/"+testDiscordGuildA || request == "/guilds/"+testDiscordGuildB {
			guildRequests = append(guildRequests, request)
		}
	}
	assert.Equal(t, []string{"/guilds/" + testDiscordGuildA, "/guilds/" + testDiscordGuildB}, guildRequests)
}

func TestSyncDiscordResolvesUnambiguousDisplayNameAndForwardsBounds(t *testing.T) {
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(t, discord.NewTokenManager(tokensDir).Save(discord.TokenRecord{
		BotUserID: testDiscordBotID, BotUsername: "archive-bot", AccessToken: testDiscordBotToken,
	}))
	source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceDisplayName(source.ID, "Alpha Guild"))
	require.NoError(t, st.UpdateSourceOAuthApp(source.ID, sql.NullString{}))
	runID, err := st.StartSync(source.ID, "discord")
	require.NoError(t, err)
	require.NoError(t, st.CompleteSync(runID, "not-json"), "--full must ignore malformed stored state")
	api := newDiscordCLIServer(t)
	api.messages[testDiscordChannel] = []discord.Message{{
		ID: "400000000000000001", ChannelID: testDiscordChannel, GuildID: testDiscordGuildA,
		Author:  discord.User{ID: "500000000000000001", Username: "synthetic-user"},
		Content: "older than lower bound", Timestamp: time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC),
	}}

	cmd := newSyncDiscordLocalCmd(testDiscordCommandDeps(t, st, tokensDir, api.server.URL))
	var output bytes.Buffer
	cmd.SetArgs([]string{"Alpha Guild", "--full", "--after", "2026-01-01"})
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	require.NoError(t, cmd.Execute())
	assert.Contains(t, output.String(), "Alpha Guild")
	assert.NotContains(t, output.String(), testDiscordBotToken)
	var messageCount int
	require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE source_id = ?", source.ID).Scan(&messageCount))
	assert.Zero(t, messageCount, "--after must exclude earlier Discord messages")
}

func TestResolveDiscordSourcesRejectsAmbiguousDisplayName(t *testing.T) {
	st := newDiscordCLIStore(t)
	for _, guildID := range []string{testDiscordGuildA, testDiscordGuildB} {
		source, err := st.GetOrCreateSource("discord", guildID)
		require.NoError(t, err)
		require.NoError(t, st.UpdateSourceDisplayName(source.ID, "Shared Name"))
	}

	_, err := resolveDiscordSources(st, "Shared Name")
	require.ErrorContains(t, err, "ambiguous")

	all, err := resolveDiscordSources(st, "")
	require.NoError(t, err)
	assert.True(t, sort.SliceIsSorted(all, func(i, j int) bool { return all[i].ID < all[j].ID }))
}
