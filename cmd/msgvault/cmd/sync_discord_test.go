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
	"go.kenn.io/msgvault/internal/store"
)

func TestSyncDiscordNoArgumentContinuesAfterFailureInSourceIDOrder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
	first, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(err)
	second, err := st.GetOrCreateSource("discord", testDiscordGuildB)
	require.NoError(err)
	require.Less(first.ID, second.ID)
	api := newDiscordCLIServer(t)
	api.fail["/guilds/"+testDiscordGuildA] = http.StatusForbidden

	cmd := newSyncDiscordLocalCmd(testDiscordCommandDeps(t, st, tokensDir, api.server.URL))
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	err = cmd.Execute()
	require.ErrorContains(err, testDiscordGuildA)
	assert.Contains(output.String(), testDiscordGuildA)
	assert.Contains(output.String(), testDiscordGuildB)
	assert.NotContains(output.String(), testDiscordBotToken)

	requests := api.requestURIs()
	var guildRequests []string
	for _, request := range requests {
		if request == "/guilds/"+testDiscordGuildA || request == "/guilds/"+testDiscordGuildB {
			guildRequests = append(guildRequests, request)
		}
	}
	assert.Equal([]string{"/guilds/" + testDiscordGuildA, "/guilds/" + testDiscordGuildB}, guildRequests)
}

func TestSyncDiscordResolvesUnambiguousDisplayNameAndForwardsBounds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
	source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(err)
	require.NoError(st.UpdateSourceDisplayName(source.ID, "Alpha Guild"))
	require.NoError(st.UpdateSourceOAuthApp(source.ID, sql.NullString{}))
	runID, err := st.StartSync(source.ID, "discord")
	require.NoError(err)
	require.NoError(st.CompleteSync(runID, "not-json"), "--full must ignore malformed stored state")
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
	require.NoError(cmd.Execute())
	assert.Contains(output.String(), "Alpha Guild")
	assert.NotContains(output.String(), testDiscordBotToken)
	var messageCount int
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID).Scan(&messageCount))
	assert.Zero(messageCount, "--after must exclude earlier Discord messages")
}

func TestSyncDiscordReportsSanitizedCatalogAndContainerAccessIssues(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
	_, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(err)
	api := newDiscordCLIServer(t)
	api.fail["/channels/"+testDiscordChannel+"/users/@me/threads/archived/private"] = http.StatusForbidden
	api.fail["/channels/"+testDiscordChannel+"/messages"] = http.StatusForbidden

	cmd := newSyncDiscordLocalCmd(testDiscordCommandDeps(t, st, tokensDir, api.server.URL))
	var output bytes.Buffer
	cmd.SetArgs([]string{testDiscordGuildA})
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	require.NoError(cmd.Execute())
	assert.Contains(output.String(), "Private archived threads unavailable")
	assert.Contains(output.String(), "Container inaccessible")
	assert.Contains(output.String(), testDiscordChannel)
	assert.Contains(output.String(), "HTTP 403")
	assert.NotContains(output.String(), "synthetic failure")
	assert.NotContains(output.String(), testDiscordBotToken)
}

func TestSyncDiscordRebuildsCacheAfterPartialDurableImportFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "archive-bot", testDiscordBotToken, "")))
	source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(err)
	require.NoError(st.UpdateSourceDisplayName(source.ID, "Alpha Guild"))
	require.NoError(st.UpdateSourceOAuthApp(source.ID, sql.NullString{}))
	installFailingDiscordParticipantTrigger(t, st)
	api := newDiscordCLIServer(t)
	api.messages[testDiscordChannel] = []discord.Message{{
		ID: "400000000000000001", ChannelID: testDiscordChannel, GuildID: testDiscordGuildA,
		Author:    discord.User{ID: "500000000000000001", Username: "synthetic-user"},
		Content:   "durable before the later failure",
		Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}}
	deps := testDiscordCommandDeps(t, st, tokensDir, api.server.URL)
	rebuilds := 0
	deps.rebuildCache = func(string) error {
		rebuilds++
		return nil
	}

	cmd := newSyncDiscordLocalCmd(deps)
	cmd.SetArgs([]string{testDiscordGuildA})
	err = cmd.Execute()
	require.ErrorContains(err, "synthetic participant persistence failure")
	var messageCount int
	require.NoError(st.DB().QueryRow(
		st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id = ?"), source.ID,
	).Scan(&messageCount))
	assert.Equal(1, messageCount, "core message persistence must precede the injected failure")
	assert.Equal(1, rebuilds, "a failed importer attempt with durable writes must refresh analytics")
}

func installFailingDiscordParticipantTrigger(t *testing.T, st *store.Store) {
	t.Helper()
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`
			CREATE OR REPLACE FUNCTION fail_discord_conversation_participant()
			RETURNS trigger AS $$
			BEGIN
				RAISE EXCEPTION 'synthetic participant persistence failure';
				RETURN NEW;
			END;
			$$ LANGUAGE plpgsql;

			CREATE TRIGGER fail_discord_conversation_participant
			BEFORE INSERT ON conversation_participants
			FOR EACH ROW
			EXECUTE FUNCTION fail_discord_conversation_participant();
		`)
	} else {
		_, err = st.DB().Exec(`
			CREATE TRIGGER fail_discord_conversation_participant
			BEFORE INSERT ON conversation_participants
			BEGIN
				SELECT RAISE(ABORT, 'synthetic participant persistence failure');
			END
		`)
	}
	require.NoError(t, err)
}

func TestResolveDiscordSourcesRejectsAmbiguousDisplayName(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	for _, guildID := range []string{testDiscordGuildA, testDiscordGuildB} {
		source, err := st.GetOrCreateSource("discord", guildID)
		require.NoError(err)
		require.NoError(st.UpdateSourceDisplayName(source.ID, "Shared Name"))
	}

	_, err := resolveDiscordSources(st, "Shared Name")
	require.ErrorContains(err, "ambiguous")

	all, err := resolveDiscordSources(st, "")
	require.NoError(err)
	assert.True(sort.SliceIsSorted(all, func(i, j int) bool { return all[i].ID < all[j].ID }))
}
