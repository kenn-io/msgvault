package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/introops"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestIntroThroughDaemonCLI(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	src, err := st.GetOrCreateSource("beeper", "synthetic-account")
	require.NoError(t, err)
	pa, err := st.EnsureParticipant("a@example.com", "A", "example.com")
	require.NoError(t, err)
	pb, err := st.EnsureParticipant("b@example.com", "B", "example.com")
	require.NoError(t, err)
	a, _, err := st.CreatePersonFromParticipantContext(ctx, pa)
	require.NoError(t, err)
	b, _, err := st.CreatePersonFromParticipantContext(ctx, pb)
	require.NoError(t, err)
	var providerCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(provider.Close)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = t.TempDir()
	cfg.Beeper.URL = provider.URL
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st, config: cfg}, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	remote := configureRemoteDaemonForTest(t, server.URL)
	run := func(args ...string) (string, error) {
		root := &cobra.Command{Use: "msgvault"}
		root.AddCommand(newIntroCommand(runDaemonCLICommandHTTPFromCobra))
		silenceUsageInRunE(root)
		var out, stderr bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&stderr)
		root.SetArgs(args)
		err := root.ExecuteContext(remote)
		return out.String() + stderr.String(), err
	}
	data, err := run("intro", "prepare", "--source-id", strconv.FormatInt(src.ID, 10), "--person", strconv.FormatInt(a.ID, 10), "--person", strconv.FormatInt(b.ID, 10), "--title", "Hello", "--body", "Private", "--json")
	require.NoError(t, err)
	var row introops.Record
	require.NoError(t, json.Unmarshal([]byte(data), &row))
	assert.Equal(t, "Private", row.Body)
	assert.Equal(t, int64(1), row.Revision)
	data, err = run("intro", "show", row.IntroID)
	require.NoError(t, err)
	assert.Contains(t, data, "no_identity_on_account")
	data, err = run("intro", "list", "--json")
	require.NoError(t, err)
	var page introops.Page
	require.NoError(t, json.Unmarshal([]byte(data), &page))
	require.Len(t, page.Items, 1)
	_, err = run("intro", "edit", row.IntroID, "--revision", "1", "--title", "Changed")
	require.NoError(t, err)
	_, err = run("intro", "cancel", row.IntroID, "--revision", "1")
	assert.Error(t, err)
	data, err = run("intro", "cancel", row.IntroID, "--revision", "2", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(data), &row))
	assert.Equal(t, "cancelled", row.Status)
	assert.Empty(t, row.Body)
	assert.Zero(t, providerCalls.Load())
}

func TestIntroDelegatedThroughRunner(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	src, err := st.GetOrCreateSource("beeper", "grant-account")
	require.NoError(t, err)
	other, err := st.GetOrCreateSource("beeper", "other-account")
	require.NoError(t, err)
	pa, err := st.EnsureParticipant("grant-a@example.com", "A", "example.com")
	require.NoError(t, err)
	pb, err := st.EnsureParticipant("grant-b@example.com", "B", "example.com")
	require.NoError(t, err)
	a, _, err := st.CreatePersonFromParticipantContext(ctx, pa)
	require.NoError(t, err)
	b, _, err := st.CreatePersonFromParticipantContext(ctx, pb)
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "owner-test-key"
	cfg.Server.AgentAccess = true
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close() })
	issue := func(perms []string, sourceID int64) string {
		grant, err := owner.IssueAgentToken(ctx, "test", perms, []int64{sourceID}, nil, time.Time{})
		require.NoError(t, err)
		return grant.Secret
	}
	run := func(secret string, args ...string) []api.CLIRunEvent {
		body, err := json.Marshal(api.CLIRunRequest{Args: args})
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/cli/run", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Msgvault-Agent-Token", secret)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var events []api.CLIRunEvent
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			var event api.CLIRunEvent
			require.NoError(t, json.Unmarshal(scanner.Bytes(), &event))
			events = append(events, event)
		}
		require.NoError(t, scanner.Err())
		return events
	}
	create := issue([]string{"draft.create"}, src.ID)
	readBody := issue([]string{"draft.create", "draft.edit"}, src.ID)
	outsider := issue([]string{"draft.create"}, other.ID)
	events := run(create, "intro", "prepare", "--source-id", strconv.FormatInt(src.ID, 10), "--person", strconv.FormatInt(a.ID, 10), "--person", strconv.FormatInt(b.ID, 10), "--title", "title", "--body", "secret", "--json")
	require.Len(t, events, 2)
	assert.NotContains(t, events[0].Data, "secret")
	var row introops.Record
	require.NoError(t, json.Unmarshal([]byte(events[0].Data), &row))
	assert.NotEqual(t, "owner", row.CreatedBy)
	events = run(readBody, "intro", "show", row.IntroID, "--json")
	require.Len(t, events, 2)
	assert.Contains(t, events[0].Data, "secret")
	denied := run(outsider, "intro", "show", row.IntroID, "--json")
	missing := run(outsider, "intro", "show", "missing", "--json")
	require.Len(t, denied, 1)
	require.Len(t, missing, 1)
	assert.Equal(t, "not_permitted", denied[0].Error)
	assert.Equal(t, denied[0].Error, missing[0].Error)
	events = run(create, "intro", "cancel", row.IntroID, "--revision", "1", "--json")
	require.Len(t, events, 2)
	assert.NotContains(t, events[0].Data, "secret")
}

func TestIntroRunnerRejectsEnvironmentAndSanitizesText(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	src, err := st.GetOrCreateSource("beeper", "safe-account")
	require.NoError(t, err)
	pa, err := st.EnsureParticipant("safe-a@example.com", "A", "example.com")
	require.NoError(t, err)
	pb, err := st.EnsureParticipant("safe-b@example.com", "B", "example.com")
	require.NoError(t, err)
	a, _, err := st.CreatePersonFromParticipantContext(ctx, pa)
	require.NoError(t, err)
	b, _, err := st.CreatePersonFromParticipantContext(ctx, pb)
	require.NoError(t, err)
	adapter := &storeAPIAdapter{store: st}
	var output bytes.Buffer
	emit := func(e api.CLIRunEvent) error { output.WriteString(e.Data); return nil }
	args := []string{"intro", "prepare", "--source-id", strconv.FormatInt(src.ID, 10), "--person", strconv.FormatInt(a.ID, 10), "--person", strconv.FormatInt(b.ID, 10), "--title", "x\x1b[31mred", "--body", "text\x1b[31mred"}
	err = adapter.runCLIIntro(ctx, api.CLIRunRequest{Args: args, Env: map[string]string{"HOME": "forbidden"}}, emit)
	require.Error(t, err)
	assert.Equal(t, "invalid_args", err.Error())
	assert.Empty(t, output.String())
	err = adapter.runCLIIntro(ctx, api.CLIRunRequest{Args: args}, emit)
	require.NoError(t, err)
	assert.NotContains(t, output.String(), "\x1b")
	err = adapter.runCLIIntro(ctx, api.CLIRunRequest{Args: []string{"intro", "unknown", "private"}}, emit)
	require.Error(t, err)
	assert.Equal(t, "invalid_args", err.Error())
}

func TestIntroStoreFailureDoesNotLogText(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	src, err := st.GetOrCreateSource("beeper", "log-account")
	require.NoError(t, err)
	pa, err := st.EnsureParticipant("log-a@example.com", "A", "example.com")
	require.NoError(t, err)
	pb, err := st.EnsureParticipant("log-b@example.com", "B", "example.com")
	require.NoError(t, err)
	a, _, err := st.CreatePersonFromParticipantContext(ctx, pa)
	require.NoError(t, err)
	b, _, err := st.CreatePersonFromParticipantContext(ctx, pb)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(ctx, `DROP TABLE person_introductions`)
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = t.TempDir()
	var logs bytes.Buffer
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	args := []string{"intro", "prepare", "--source-id", strconv.FormatInt(src.ID, 10), "--person", strconv.FormatInt(a.ID, 10), "--person", strconv.FormatInt(b.ID, 10), "--title", "secret-title", "--body", "secret-body"}
	payload, err := json.Marshal(api.CLIRunRequest{Args: args})
	require.NoError(t, err)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/cli/run", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	assert.Contains(t, resp.Body.String(), "local_store_failed")
	assert.NotContains(t, logs.String(), "secret-title")
	assert.NotContains(t, logs.String(), "secret-body")
}
