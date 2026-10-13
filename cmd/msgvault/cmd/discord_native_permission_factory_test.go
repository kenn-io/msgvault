package cmd

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/store"
	"net/http"
	"testing"
)

// Real native credential resolution and HTTP identity verification must pause
// stale authority on failure before an importer can be returned.
func TestDiscordImporterFactoryPausesUnavailableNativeAuthority(t *testing.T) {
	for _, mode := range []string{"missing_credential", "wrong_bot", "denied_bot"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := newDiscordCLIStore(t)
			source, err := st.GetOrCreateSource("discord", testDiscordGuildA)
			require.NoError(err)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "discord", NativeIdentity: source.Identifier, Known: true, MessageRead: true}))
			tokens := t.TempDir()
			api := newDiscordCLIServer(t)
			if mode != "missing_credential" {
				bot := testDiscordBotID
				if mode == "wrong_bot" {
					bot = "500000000000000002"
				}
				require.NoError(discord.NewTokenManager(tokens).Save(discord.NewTokenRecord(bot, "synthetic-bot", testDiscordBotToken, "")))
			}
			if mode == "denied_bot" {
				api.fail["/users/@me"] = http.StatusForbidden
			}
			importer, _, err := newDiscordImporterForSource(t.Context(), st, source, testDiscordCommandDeps(t, st, tokens, api.server.URL))
			require.Error(err)
			assert.Nil(importer)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed, "a failed selected credential must not retain a prior native grant")
			current, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			if mode == "missing_credential" {
				assert.Equal(resource.Reference, current.Reference, "an unknown credential failure pauses without asserting source removal")
			}
		})
	}
}
