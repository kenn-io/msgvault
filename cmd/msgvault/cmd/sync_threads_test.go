package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/threads"
)

type threadsSyncDaemon struct {
	*storeAPIAdapter

	deps threadsCommandDeps
}

func (d *threadsSyncDaemon) RunCLICommand(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	return d.runCLICommandWithRunner(ctx, req, emit, func(ctx context.Context, args []string, env map[string]string, _ string, output func(string, string) error) error {
		deps := d.deps
		deps.oauthPayload = func() string { return env[clirun.EnvThreadsOAuth] }
		cmd := newAddThreadsLocalCmd(deps)
		if args[0] == "sync-threads" {
			cmd = newSyncThreadsLocalCmd(deps)
		}
		cmd.SetArgs(args[1:])
		var out, stderr bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&stderr)
		err := cmd.ExecuteContext(ctx)
		if out.Len() > 0 {
			if e := output("stdout", out.String()); e != nil {
				return e
			}
		}
		if stderr.Len() > 0 {
			if e := output("stderr", stderr.String()); e != nil {
				return e
			}
		}
		return err
	})
}
func TestSyncThreadsEndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		require.True(st.IsPostgreSQL())
	}
	provider := newThreadsConnectFixture(t, "20")
	dir := t.TempDir()
	cfg := &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}, Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}
	deps := provider.deps(st, cfg.TokensDir())
	deps.newAPI = func(record threads.TokenRecord, budget *threads.RequestBudget) (threads.API, error) {
		return threads.NewClient(threads.ClientOptions{HTTPClient: provider.server.Client(), BaseURL: provider.server.URL + "/v1.0", AccessToken: record.AccessToken(), Budget: budget})
	}
	cacheRuns := 0
	deps.databaseDSN = cfg.DatabaseDSN
	deps.rebuildCache = func(string) error {
		cacheRuns++
		source, err := st.GetSourceByTypeAndIdentifier(sourceTypeThreads, "threads:123:20")
		require.NoError(err)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		// Cache publication must be outside both credential and execution ownership.
		return deps.tokenManager().WithSourceLock(ctx, source.Identifier, func() error {
			execution, err := st.AcquireSyncExecutionContext(ctx, source.ID)
			if err != nil {
				return err
			}
			return execution.Release()
		})
	}
	adapter := &threadsSyncDaemon{storeAPIAdapter: &storeAPIAdapter{store: st, config: cfg}, deps: deps}
	engine := query.NewSQLiteEngine(st.DB())
	if st.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(st.DB(), query.PostgreSQLQueryDialect{})
	}
	srv := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Engine: engine, Logger: slog.New(slog.DiscardHandler)}).Router())
	defer srv.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: srv.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true})
	require.NoError(err)
	payload, err := json.Marshal(threadsOAuthPayload{AppID: "123", RedirectURI: "https://example.com/threads-callback", Code: "synthetic-code", AppSecret: "synthetic-app-secret"})
	require.NoError(err)
	require.NoError(client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: []string{"add-threads", "--app-id", "123", "--redirect-uri", "https://example.com/threads-callback"}, Env: map[string]string{clirun.EnvThreadsOAuth: string(payload)}}, nil))
	pause := true
	readAt := provider.clock().Add(-30 * time.Minute).Format(time.RFC3339Nano)
	provider.mu.Lock()
	provider.readHandler = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/v1.0/") {
			return false
		}
		if pause {
			w.Header().Set("X-App-Usage", `{"call_count":80}`)
		}
		if r.URL.Query().Get("after") != "" {
			fmt.Fprint(w, `{"data":[]}`)
			return true
		}
		data := []map[string]any{}
		row := func(id, text, root, parent string) map[string]any {
			m := map[string]any{"id": id, "text": text, "username": "synthetic_author", "timestamp": readAt, "has_replies": false}
			if root != "" {
				m["root_post"] = map[string]string{"id": root}
				m["replied_to"] = map[string]string{"id": parent}
				m["is_reply"] = true
			}
			return m
		}
		switch r.URL.Path {
		case "/v1.0/me/threads":
			data = append(data, row("100", "Root phrase for source search", "", ""))
		case "/v1.0/me/replies":
			data = append(data, row("800", "Own external reply", "900", "900"))
		case "/v1.0/100/conversation":
			data = append(data, row("101", "First nested reply", "100", "100"), row("102", "Deep nested reply", "100", "101"))
		default:
			http.NotFound(w, r)
			return true
		}
		body, err := json.Marshal(map[string]any{"data": data, "paging": map[string]any{"cursors": map[string]string{"after": "next"}}})
		require.NoError(err)
		_, _ = w.Write(body)
		return true
	}
	provider.mu.Unlock()
	var out bytes.Buffer
	run := func() {
		out.Reset()
		require.NoError(client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: []string{"sync-threads", "threads:123:20"}}, func(stream, data string) error {
			if stream == "stdout" {
				out.WriteString(data)
			}
			return nil
		}))
	}
	run()
	assert.Contains(out.String(), "paused")
	assert.Equal(1, cacheRuns)
	provider.mu.Lock()
	pause = false
	provider.mu.Unlock()
	run()
	run() // Finish source-wide walks, then sweep roots discovered there.
	assert.Contains(out.String(), "history_complete")
	assert.Contains(out.String(), "oldest_observed")
	source, err := st.GetSourceByTypeAndIdentifier(sourceTypeThreads, "threads:123:20")
	require.NoError(err)
	records, err := st.SourceMessageMetadata(source.ID)
	require.NoError(err)
	assert.Len(records, 4)
	var linked, observed int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND reply_to_message_id IS NOT NULL`), source.ID).Scan(&linked))
	assert.Equal(2, linked)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM participant_contact_observations WHERE source_id=?`), source.ID).Scan(&observed))
	assert.Positive(observed)
	// The existing daemon read paths must expose the archived conversation.
	require.Error(client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: []string{"sync-threads", "20"}}, nil))
	remote := daemonclient.NewEngineAdapter(client)
	matches, err := remote.SearchFast(t.Context(), search.ParseOR("Root phrase"), query.MessageFilter{SourceID: &source.ID, MessageType: "threads"}, 20, 0)
	require.NoError(err)
	require.Len(matches, 1)
	page, err := remote.ListThread(t.Context(), query.ThreadQuery{MessageRef: query.MessageRef{SourceMessageID: "100", Account: source.Identifier}, All: true})
	require.NoError(err)
	require.Len(page.Messages, 3)
}

func TestSyncThreadsJSONCoverage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	provider := newThreadsConnectFixture(t, "20")
	deps := provider.deps(st, t.TempDir())
	_, err := executeAddThreadsFixture(t, deps, "123", false)
	require.NoError(err)
	provider.mu.Lock()
	provider.readHandler = func(w http.ResponseWriter, _ *http.Request) bool { fmt.Fprint(w, `{"data":[]}`); return true }
	provider.mu.Unlock()
	deps.newAPI = func(record threads.TokenRecord, budget *threads.RequestBudget) (threads.API, error) {
		return threads.NewClient(threads.ClientOptions{HTTPClient: provider.server.Client(), BaseURL: provider.server.URL + "/v1.0", AccessToken: record.AccessToken(), Budget: budget})
	}
	command := newSyncThreadsLocalCmd(deps)
	command.SetArgs([]string{"threads:123:20", "--json"})
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	require.NoError(command.ExecuteContext(t.Context()))
	var result struct {
		Status          string    `json:"status"`
		HistoryComplete bool      `json:"history_complete"`
		PendingWalks    int       `json:"pending_walks"`
		RequestedFloor  time.Time `json:"requested_floor"`
	}
	require.NoError(json.Unmarshal(out.Bytes(), &result))
	assert.Equal("completed", result.Status)
	assert.True(result.HistoryComplete)
	assert.Zero(result.PendingWalks)
	assert.Equal(time.Unix(1688540400, 0).UTC(), result.RequestedFloor)
}
