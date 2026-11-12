package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	check := assert.New(t)
	req := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	src, err := st.GetOrCreateSource("beeper", "synthetic-account")
	req.NoError(err)
	pa, err := st.EnsureParticipant("a@example.com", "A", "example.com")
	req.NoError(err)
	pb, err := st.EnsureParticipant("b@example.com", "B", "example.com")
	req.NoError(err)
	a, _, err := st.CreatePersonFromParticipantContext(ctx, pa)
	req.NoError(err)
	b, _, err := st.CreatePersonFromParticipantContext(ctx, pb)
	req.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = t.TempDir()
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
		if err != nil {
			return out.String() + stderr.String(), fmt.Errorf("execute intro: %w", err)
		}
		return out.String() + stderr.String(), nil
	}
	data, err := run("intro", "prepare", "--source-id", strconv.FormatInt(src.ID, 10), "--person", strconv.FormatInt(a.ID, 10), "--person", strconv.FormatInt(b.ID, 10), "--title", "Hello", "--body", "Private", "--json")
	req.NoError(err)
	var row introops.Record
	req.NoError(json.Unmarshal([]byte(data), &row))
	check.Equal("Private", row.Body)
	check.Equal(int64(1), row.Revision)
	data, err = run("intro", "show", row.IntroID)
	req.NoError(err)
	check.Contains(data, "no_identity_on_account")
	data, err = run("intro", "list", "--json")
	req.NoError(err)
	var page introops.Page
	req.NoError(json.Unmarshal([]byte(data), &page))
	req.Len(page.Items, 1)
	check.Equal(row.IntroID, page.Items[0].IntroID)
	stored, err := st.GetPersonIntroductionContext(ctx, row.IntroID)
	req.NoError(err)
	check.Equal("Private", stored.Body)
	_, err = run("intro", "cancel", row.IntroID, "--revision", "99")
	req.Error(err)
}

func TestIntroDelegatedThroughRunner(t *testing.T) {
	check := assert.New(t)
	req := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	src, err := st.GetOrCreateSource("beeper", "grant-account")
	req.NoError(err)
	other, err := st.GetOrCreateSource("beeper", "other-account")
	req.NoError(err)
	pa, err := st.EnsureParticipant("grant-a@example.com", "A", "example.com")
	req.NoError(err)
	pb, err := st.EnsureParticipant("grant-b@example.com", "B", "example.com")
	req.NoError(err)
	a, _, err := st.CreatePersonFromParticipantContext(ctx, pa)
	req.NoError(err)
	b, _, err := st.CreatePersonFromParticipantContext(ctx, pb)
	req.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "owner-test-key"
	cfg.Server.AgentAccess = true
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
	req.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	issue := func(perms []string, sourceID int64) string {
		grant, err := owner.IssueAgentToken(ctx, "test", perms, []int64{sourceID}, nil, time.Time{})
		req.NoError(err)
		return grant.Secret
	}
	run := func(secret string, args ...string) []api.CLIRunEvent {
		body, err := json.Marshal(api.CLIRunRequest{Args: args})
		req.NoError(err)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/cli/run", bytes.NewReader(body))
		req.NoError(err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Msgvault-Agent-Token", secret)
		resp, err := http.DefaultClient.Do(request)
		req.NoError(err)
		defer func() { _ = resp.Body.Close() }()
		req.Equal(http.StatusOK, resp.StatusCode)
		var events []api.CLIRunEvent
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			var event api.CLIRunEvent
			req.NoError(json.Unmarshal(scanner.Bytes(), &event))
			events = append(events, event)
		}
		req.NoError(scanner.Err())
		return events
	}
	create := issue([]string{"draft.create"}, src.ID)
	outsider := issue([]string{"draft.create"}, other.ID)
	events := run(create, "intro", "prepare", "--source-id", strconv.FormatInt(src.ID, 10), "--person", strconv.FormatInt(a.ID, 10), "--person", strconv.FormatInt(b.ID, 10), "--title", "title", "--body", "secret", "--json")
	req.Len(events, 2)
	check.Equal("stdout", events[0].Type)
	check.Equal("complete", events[1].Type)
	var row introops.Record
	req.NoError(json.Unmarshal([]byte(events[0].Data), &row))
	check.NotEqual("owner", row.CreatedBy)
	denied := run(outsider, "intro", "show", row.IntroID, "--json")
	missing := run(outsider, "intro", "show", "missing", "--json")
	req.Len(denied, 1)
	req.Len(missing, 1)
	check.Equal("not_permitted", denied[0].Error)
	check.Equal(denied[0].Error, missing[0].Error)
}

func TestIntroRunnerSanitizesText(t *testing.T) {
	check := assert.New(t)
	req := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	src, err := st.GetOrCreateSource("beeper", "safe-account")
	req.NoError(err)
	pa, err := st.EnsureParticipant("safe-a@example.com", "A", "example.com")
	req.NoError(err)
	pb, err := st.EnsureParticipant("safe-b@example.com", "B", "example.com")
	req.NoError(err)
	a, _, err := st.CreatePersonFromParticipantContext(ctx, pa)
	req.NoError(err)
	b, _, err := st.CreatePersonFromParticipantContext(ctx, pb)
	req.NoError(err)
	adapter := &storeAPIAdapter{store: st}
	var output bytes.Buffer
	emit := func(e api.CLIRunEvent) error { output.WriteString(e.Data); return nil }
	args := []string{"intro", "prepare", "--source-id", strconv.FormatInt(src.ID, 10), "--person", strconv.FormatInt(a.ID, 10), "--person", strconv.FormatInt(b.ID, 10), "--title", "x\x1b[31mred", "--body", "text\x1b[31mred"}
	err = adapter.runCLIIntro(ctx, api.CLIRunRequest{Args: args}, emit)
	req.NoError(err)
	check.NotContains(output.String(), "\x1b")
	err = adapter.runCLIIntro(ctx, api.CLIRunRequest{Args: []string{"intro", "unknown", "private"}}, emit)
	req.Error(err)
	check.Equal("invalid_args", err.Error())
}

func TestIntroStoreFailureDoesNotLogText(t *testing.T) {
	check := assert.New(t)
	req := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	src, err := st.GetOrCreateSource("beeper", "log-account")
	req.NoError(err)
	pa, err := st.EnsureParticipant("log-a@example.com", "A", "example.com")
	req.NoError(err)
	pb, err := st.EnsureParticipant("log-b@example.com", "B", "example.com")
	req.NoError(err)
	a, _, err := st.CreatePersonFromParticipantContext(ctx, pa)
	req.NoError(err)
	b, _, err := st.CreatePersonFromParticipantContext(ctx, pb)
	req.NoError(err)
	req.NoError(st.Close())
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = t.TempDir()
	var logs bytes.Buffer
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	args := []string{"intro", "prepare", "--source-id", strconv.FormatInt(src.ID, 10), "--person", strconv.FormatInt(a.ID, 10), "--person", strconv.FormatInt(b.ID, 10), "--title", "secret-title", "--body", "secret-body"}
	payload, err := json.Marshal(api.CLIRunRequest{Args: args})
	req.NoError(err)
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/cli/run", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, request)
	check.Contains(resp.Body.String(), "local_store_failed")
	check.NotContains(logs.String(), "secret-title")
	check.NotContains(logs.String(), "secret-body")
}
