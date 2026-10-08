package archive_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/archive"
)

func exerciseDiscordCollection(t *testing.T, runtime *archive.Archive) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body any
		switch r.URL.Path {
		case "/users/@me":
			body = map[string]any{"id": "101", "username": "archive-bot", "bot": true}
		case "/guilds/200":
			body = map[string]any{"id": "200", "name": "Example guild"}
		case "/guilds/200/channels":
			body = []any{
				map[string]any{"id": "301", "guild_id": "200", "type": 0, "name": "selected", "last_message_id": "1200000000000000301", "permission_overwrites": []any{map[string]any{"id": "200", "type": 0, "allow": "1024", "deny": "0"}}},
				map[string]any{"id": "302", "guild_id": "200", "type": 0, "name": "unselected", "last_message_id": "1200000000000000302"},
			}
		case "/guilds/200/threads/active":
			body = map[string]any{"threads": []any{
				map[string]any{"id": "401", "guild_id": "200", "parent_id": "301", "type": 11, "name": "public thread", "last_message_id": "1200000000000000401"},
				map[string]any{"id": "402", "guild_id": "200", "parent_id": "301", "type": 12, "name": "private thread", "last_message_id": "1200000000000000402"},
			}}
		case "/channels/301/threads/archived/public":
			body = map[string]any{"threads": []any{}, "has_more": false}
		case "/channels/301/messages", "/channels/401/messages":
			channel := strings.Split(r.URL.Path, "/")[2]
			id := "1200000000000000" + channel
			body = []any{}
			if r.URL.Query().Get("before") != id && r.URL.Query().Get("after") != id {
				body = []any{map[string]any{
					"id": id, "channel_id": channel, "guild_id": "200", "type": 0,
					"timestamp": "2024-01-01T12:00:00Z", "content": "Discord nebula discussion",
					"author": map[string]any{"id": "102", "username": "example-author"},
				}}
			}
		default:
			assert.Fail("unexpected Discord fetch", "%s", r.URL.Path)
			http.Error(w, "unexpected fetch", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.MarshalWrite(w, body))
	}))
	defer server.Close()
	credential := archive.DiscordCredential{Token: "test-bot", BaseURL: server.URL}
	channels, err := archive.DiscordChannels(t.Context(), credential, "200")
	require.NoError(err)
	require.Len(channels, 2)
	require.Len(channels[0].PermissionOverwrites, 1)
	assert.Equal("1024", channels[0].PermissionOverwrites[0].Allow)
	source, err := runtime.BindDiscord(t.Context(), credential, "200")
	require.NoError(err)
	opts := archive.DiscordSync{Credential: credential, Options: archive.DiscordOptions{GuildID: "200", SourceID: source, PublicChannels: []string{"301"}}}
	first, err := runtime.SyncDiscord(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(2), first.MessagesAdded)
	second, err := runtime.SyncDiscord(t.Context(), opts)
	require.NoError(err)
	assert.Zero(second.MessagesAdded)
	searchIDs(t, runtime, "nebula", 2)
	require.NoError(runtime.PurgeChannel(t.Context(), source, "301"))
	searchIDs(t, runtime, "nebula", 0)
	searchIDs(t, runtime, "observatory", 1)
}
