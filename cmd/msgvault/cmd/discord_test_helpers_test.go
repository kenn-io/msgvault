package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const (
	testDiscordBotToken = "synthetic.bot.token"
	testDiscordBotID    = "100000000000000001"
	testDiscordGuildA   = "200000000000000001"
	testDiscordGuildB   = "200000000000000002"
	testDiscordChannel  = "300000000000000001"
)

type discordCLIServer struct {
	testing *testing.T
	server  *httptest.Server

	mu       sync.Mutex
	requests []string
	guilds   []discord.Guild
	fail     map[string]int
	failCode map[string]int
	messages map[string][]discord.Message
}

func newDiscordCLIServer(t *testing.T, guilds ...discord.Guild) *discordCLIServer {
	t.Helper()
	fake := &discordCLIServer{
		testing:  t,
		guilds:   guilds,
		fail:     make(map[string]int),
		failCode: make(map[string]int),
		messages: map[string][]discord.Message{
			testDiscordChannel: {},
		},
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *discordCLIServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.RequestURI())
	status := f.fail[r.URL.Path]
	code := f.failCode[r.URL.Path]
	f.mu.Unlock()

	if got := r.Header.Get("Authorization"); got != "Bot "+testDiscordBotToken {
		http.Error(w, `{"message":"unauthorized","code":0}`, http.StatusUnauthorized)
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		if code == 0 {
			code = 50001
		}
		_, _ = fmt.Fprintf(w, `{"message":"synthetic failure","code":%d}`, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/users/@me":
		writeDiscordCLIJSON(f.testing, w, discord.User{ID: testDiscordBotID, Username: "archive-bot", Bot: true})
	case "/users/@me/guilds":
		writeDiscordCLIJSON(f.testing, w, f.guilds)
	case "/guilds/" + testDiscordGuildA:
		writeDiscordCLIJSON(f.testing, w, discord.Guild{ID: testDiscordGuildA, Name: "Alpha Guild"})
	case "/guilds/" + testDiscordGuildB:
		writeDiscordCLIJSON(f.testing, w, discord.Guild{ID: testDiscordGuildB, Name: "Beta Guild"})
	case "/guilds/" + testDiscordGuildA + "/channels", "/guilds/" + testDiscordGuildB + "/channels":
		writeDiscordCLIJSON(f.testing, w, []discord.Channel{{ID: testDiscordChannel, Type: 0, Name: "general"}})
	case "/guilds/" + testDiscordGuildA + "/threads/active", "/guilds/" + testDiscordGuildB + "/threads/active":
		writeDiscordCLIJSON(f.testing, w, map[string]any{"threads": []discord.Channel{}})
	case "/guilds/" + testDiscordGuildA + "/members", "/guilds/" + testDiscordGuildB + "/members":
		writeDiscordCLIJSON(f.testing, w, []discord.GuildMember{})
	case "/channels/" + testDiscordChannel + "/threads/archived/public",
		"/channels/" + testDiscordChannel + "/users/@me/threads/archived/private":
		writeDiscordCLIJSON(f.testing, w, map[string]any{"threads": []discord.Channel{}, "has_more": false})
	case "/channels/" + testDiscordChannel + "/messages":
		writeDiscordCLIJSON(f.testing, w, filterDiscordCLIMessages(f.messages[testDiscordChannel], r))
	case "/channels/" + testDiscordChannel + "/messages/400000000000000001":
		messages := f.messages[testDiscordChannel]
		if len(messages) == 0 {
			http.NotFound(w, r)
			return
		}
		writeDiscordCLIJSON(f.testing, w, messages[0])
	default:
		http.NotFound(w, r)
	}
}

func filterDiscordCLIMessages(messages []discord.Message, request *http.Request) []discord.Message {
	after, _ := strconv.ParseUint(request.URL.Query().Get("after"), 10, 64)
	before, _ := strconv.ParseUint(request.URL.Query().Get("before"), 10, 64)
	filtered := make([]discord.Message, 0, len(messages))
	for _, message := range messages {
		id, err := strconv.ParseUint(message.ID, 10, 64)
		if err != nil || after != 0 && id <= after || before != 0 && id >= before {
			continue
		}
		filtered = append(filtered, message)
	}
	return filtered
}

func writeDiscordCLIJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	require.NoError(t, json.NewEncoder(w).Encode(value))
}

func (f *discordCLIServer) requestURIs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func testDiscordCommandDeps(t *testing.T, st *store.Store, tokensDir, baseURL string) discordCommandDeps {
	t.Helper()
	return discordCommandDeps{
		openStore:    func() (*store.Store, func(), error) { return st, func() {}, nil },
		tokenManager: func() *discord.TokenManager { return discord.NewTokenManager(tokensDir) },
		apiBaseURL:   func() string { return baseURL },
		providerConfig: func() config.DiscordConfig {
			provider := config.DiscordConfig{}
			provider.ApplyDefaults()
			return provider
		},
		attachmentsDir:       func() string { return t.TempDir() },
		databaseDSN:          func() string { return "" },
		rebuildCache:         func(string) error { return nil },
		postSourceMigrations: func(*store.Store) error { return nil },
		registerGuild:        registerDiscordGuild,
	}
}

func newDiscordCLIStore(t *testing.T) *store.Store {
	t.Helper()
	return testutil.NewTestStore(t)
}
