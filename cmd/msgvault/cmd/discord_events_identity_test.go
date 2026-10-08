package cmd

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/discord"
)

func TestDiscordEventsRejectsCredentialIdentityMismatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord("500000000000000002", "synthetic-bot", testDiscordBotToken, "")))
	source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(err)
	api := newDiscordCLIServer(t)
	_, err = importDiscordSource(t.Context(), st, source, testDiscordCommandDeps(t, st, tokensDir, api.server.URL), false, time.Time{}, nil)
	require.Error(err, "the credential-bound current user must match its recorded bot identity")
	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	assert.Empty(identities, "mismatched credentials must not confirm ownership")
}

func TestDiscordEventsConfirmsOnlyCredentialBot(t *testing.T) {
	for _, tc := range []struct {
		name, author string
		bot, own     bool
	}{
		{"credential bot", testDiscordBotID, true, true},
		{"foreign bot", "500000000000000002", true, false},
		{"ordinary user", "500000000000000003", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := newDiscordCLIStore(t)
			tokensDir := t.TempDir()
			require.NoError(discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "synthetic-bot", testDiscordBotToken, "synthetic-binding")))
			source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
			require.NoError(err)
			require.NoError(st.UpdateSourceOAuthApp(source.ID, sql.NullString{String: "synthetic-binding", Valid: true}))
			other, err := st.GetOrCreateSource("discord", testDiscordGuildB)
			require.NoError(err)
			require.NoError(st.AddAccountIdentity(other.ID, tc.author, "manual"))
			api := newDiscordCLIServer(t)
			api.messages[testDiscordChannel] = []discord.Message{{ID: "400000000000000001", ChannelID: testDiscordChannel, GuildID: testDiscordGuildA, Author: discord.User{ID: tc.author, Username: "synthetic-sender", Bot: tc.bot}, Content: "Synthetic message", Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
			_, err = importDiscordSource(t.Context(), st, source, testDiscordCommandDeps(t, st, tokensDir, api.server.URL), false, time.Time{}, nil)
			require.NoError(err)
			var own bool
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_from_me FROM messages WHERE source_id=? AND source_message_id=?`), source.ID, "400000000000000001").Scan(&own))
			assert.Equal(tc.own, own)
			identities, err := st.ListAccountIdentities(source.ID)
			require.NoError(err)
			require.Len(identities, 1)
			assert.Equal(testDiscordBotID, identities[0].Address)
		})
	}
}

func TestDiscordEventsRejectsNonBotCurrentUser(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newDiscordCLIStore(t)
	tokensDir := t.TempDir()
	require.NoError(discord.NewTokenManager(tokensDir).Save(discord.NewTokenRecord(testDiscordBotID, "synthetic-bot", testDiscordBotToken, "")))
	source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
	require.NoError(err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/@me" {
			http.NotFound(w, r)
			return
		}
		writeDiscordCLIJSON(t, w, discord.User{ID: testDiscordBotID, Username: "synthetic-human", Bot: false})
	}))
	t.Cleanup(srv.Close)
	_, err = importDiscordSource(t.Context(), st, source, testDiscordCommandDeps(t, st, tokensDir, srv.URL), false, time.Time{}, nil)
	require.ErrorContains(err, "bound bot identity")
	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	assert.Empty(identities)
}
