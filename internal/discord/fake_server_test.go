package discord

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeDiscord struct {
	t *testing.T

	mu       sync.Mutex
	requests []*http.Request
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	return &fakeDiscord{t: t}
}

func (f *fakeDiscord) server() *httptest.Server {
	f.t.Helper()
	return httptest.NewServer(http.HandlerFunc(f.serveHTTP))
}

func (f *fakeDiscord) requestPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	paths := make([]string, 0, len(f.requests))
	for _, request := range f.requests {
		path := request.URL.Path
		if request.URL.RawQuery != "" {
			path += "?" + request.URL.RawQuery
		}
		paths = append(paths, path)
	}
	return paths
}

func (f *fakeDiscord) firstRequest() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[0].Clone(f.requests[0].Context())
}

func (f *fakeDiscord) serveHTTP(w http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, request.Clone(request.Context()))
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch request.URL.Path {
	case "/api/v10/users/@me":
		writeDiscordJSON(w, http.StatusOK, map[string]any{
			"id": "101", "username": "archive-bot", "global_name": "Archive Bot", "bot": true,
		})
	case "/api/v10/users/@me/guilds":
		f.writeGuilds(w, request)
	case "/api/v10/guilds/201":
		writeDiscordJSON(w, http.StatusOK, map[string]any{"id": "201", "name": "Test Guild", "icon": "guild-icon"})
	case "/api/v10/guilds/201/channels":
		writeDiscordJSON(w, http.StatusOK, []map[string]any{{
			"id": "301", "guild_id": "201", "type": 0, "name": "general", "topic": "Synthetic discussion", "nsfw": false,
		}})
	case "/api/v10/guilds/201/threads/active":
		writeDiscordJSON(w, http.StatusOK, map[string]any{"threads": []map[string]any{{
			"id": "401", "guild_id": "201", "parent_id": "301", "type": 11, "name": "active-thread",
			"thread_metadata": map[string]any{"archived": false, "auto_archive_duration": 1440, "archive_timestamp": "2026-07-18T12:00:00Z", "locked": false},
		}}, "members": []any{}})
	case "/api/v10/channels/301/threads/archived/public":
		f.writeArchivedThreads(w, "public-thread", "402")
	case "/api/v10/channels/301/users/@me/threads/archived/private":
		f.writeArchivedThreads(w, "private-thread", "403")
	case "/api/v10/channels/301/messages":
		writeDiscordJSON(w, http.StatusOK, []json.RawMessage{
			json.RawMessage(`{"id":"501","channel_id":"301","guild_id":"201","author":{"id":"102","username":"alice","global_name":"Alice"},"content":"hello","timestamp":"2026-07-18T12:01:00Z","type":0,"future_field":{"retained":true}}`),
		})
	case "/api/v10/channels/301/messages/501":
		writeDiscordJSON(w, http.StatusOK, json.RawMessage(`{"id":"501","channel_id":"301","author":{"id":"102","username":"alice"},"content":"hello, edited","timestamp":"2026-07-18T12:01:00Z","edited_timestamp":"2026-07-18T12:02:00Z","type":0}`))
	default:
		writeDiscordJSON(w, http.StatusNotFound, map[string]any{"code": 0, "message": "not found"})
	}
}

func (f *fakeDiscord) writeGuilds(w http.ResponseWriter, request *http.Request) {
	if request.URL.Query().Get("after") != "" {
		writeDiscordJSON(w, http.StatusOK, []map[string]any{{"id": "1200", "name": "Last Guild"}})
		return
	}

	guilds := make([]map[string]any, 200)
	for i := range guilds {
		guilds[i] = map[string]any{"id": strconv.Itoa(1000 + i), "name": fmt.Sprintf("Guild %03d", i)}
	}
	writeDiscordJSON(w, http.StatusOK, guilds)
}

func (f *fakeDiscord) writeArchivedThreads(w http.ResponseWriter, name, id string) {
	archiveTimestamp := "2026-07-17T12:00:00Z"
	writeDiscordJSON(w, http.StatusOK, map[string]any{"threads": []map[string]any{{
		"id": id, "guild_id": "201", "parent_id": "301", "type": 11, "name": name,
		"thread_metadata": map[string]any{"archived": true, "archive_timestamp": archiveTimestamp, "auto_archive_duration": 1440, "locked": false},
	}}, "members": []any{}, "has_more": true})
}

func writeDiscordJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		panic(fmt.Errorf("encode fake Discord response: %w", err))
	}
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return parsed
}
