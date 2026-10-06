package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/testutil/katatest"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestKataInputRejectedBeforeTransport(t *testing.T) {
	largest := `{"selectors":[]}`
	largest += strings.Repeat(" ", kataevidence.MaxRequestBytes-len(largest))
	for _, tc := range []struct {
		input    string
		rejected bool
	}{
		{`{"selectors":[],"unexpected":true}`, true},
		{largest + " ", true},
		{`{} {}`, true},
		{largest, false},
	} {
		command := newKataCmd()
		command.SetArgs([]string{"evidence", "prepare"})
		command.SetIn(strings.NewReader(tc.input))
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		err := command.ExecuteContext(t.Context())
		require.Error(t, err)
		// Without a daemon configured, input that passes the reader fails opening the client.
		assert.Equal(t, tc.rejected, strings.Contains(strings.ToLower(err.Error()), "kata input"), "%q: %v", tc.input[:min(len(tc.input), 40)], err)
	}
}

// The daemon adapter, not a bare Store, must carry evidence and the retry
// marker from the CLI through to Kata.
func TestKataCreateThroughDaemonAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("cli-evidence")
	_, err := f.Store.DB().Exec(f.Store.Rebind("INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)"), id, "é界🙂 send the revised budget")
	require.NoError(err)
	kataServer := httptest.NewServer(katatest.New(t).Service.Handler())
	t.Cleanup(kataServer.Close)
	cfg := &config.Config{}
	cfg.Integrations.Kata = config.TaskIntegrationConfig{Enabled: true, Endpoint: kataServer.URL, APIKey: katatest.Token, DefaultProject: "example"}
	daemon := httptest.NewServer(api.NewServer(cfg, &storeAPIAdapter{store: f.Store}, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(daemon.Close)
	ctx := configureRemoteDaemonForTest(t, daemon.URL)

	output := runKataCommand(ctx, t, fmt.Sprintf(`{"selectors":[{"kind":"message","message_id":%d,"max_chars":1000}]}`, id), "evidence", "prepare")
	var prepared generated.KataEvidencePrepareResponse
	require.NoError(json.Unmarshal(output, &prepared))
	require.Len(prepared.Evidence, 1)
	assert.Equal("é界🙂 send the revised budget", prepared.Evidence[0].Excerpt)

	request, err := json.Marshal(generated.KataIssueCreateRequest{Title: "Send the revised budget", Evidence: []generated.Reference{prepared.Evidence[0].Reference}})
	require.NoError(err)
	var created, retried generated.KataIssueResponse
	require.NoError(json.Unmarshal(runKataCommand(ctx, t, string(request), "create", "--idempotency-key", "cli-key", "--json"), &created))
	require.NoError(json.Unmarshal(runKataCommand(ctx, t, string(request), "create", "--idempotency-key", "cli-key", "--json"), &retried))

	// The caller names the issue with its key; the CLI never invents one.
	command := newKataCmd()
	command.SetArgs([]string{"create"})
	command.SetIn(strings.NewReader(string(request)))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	require.ErrorContains(command.ExecuteContext(ctx), "--idempotency-key is required")
	assert.False(created.Replayed)
	assert.True(retried.Replayed)
	assert.Equal(created.Issue.UID, retried.Issue.UID)
	assert.Equal(created.Issue.QualifiedRef+"\tSend the revised budget (already filed, "+created.Issue.Status+")\n", string(runKataCommand(ctx, t, string(request), "create", "--idempotency-key", "cli-key")))
	var citing generated.KataIssueListResponse
	require.NoError(json.Unmarshal(runKataCommand(ctx, t, "", "issues", "--message", strconv.FormatInt(id, 10), "--json"), &citing))
	require.Len(citing.Issues, 1)
	assert.Equal(created.Issue.UID, citing.Issues[0].UID)

	// The exported generated client decodes a replay like a create, and the
	// daemon client names the filed issue when a retry changes the request.
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true, HTTPClient: daemon.Client()})
	require.NoError(err)
	generatedClient, err := client.GeneratedClient()
	require.NoError(err)
	replayed, err := generatedClient.CreateKataIssue(ctx, &generated.CreateKataIssueRequestOptions{
		Body: &generated.KataIssueCreateRequest{Title: "Send the revised budget", Evidence: []generated.Reference{prepared.Evidence[0].Reference}}, Header: &generated.CreateKataIssueHeaders{IdempotencyKey: "cli-key"},
	})
	require.NoError(err)
	assert.True(replayed.Replayed)
	_, err = client.CreateKataIssue(ctx, "cli-key", generated.KataIssueCreateRequest{Title: "Send the final budget", Evidence: []generated.Reference{prepared.Evidence[0].Reference}})
	conflict, ok := errors.AsType[*daemonclient.KataIssueConflictError](err)
	require.True(ok, "%v", err)
	assert.Equal(created.Issue.QualifiedRef, conflict.Issue.QualifiedRef)

	// Truncation must not add a non-tabular row to stdout.
	for i := range 10 {
		runKataCommand(ctx, t, string(request), "create", "--idempotency-key", fmt.Sprintf("cli-extra-%d", i), "--json")
	}
	command = newKataCmd()
	command.SetArgs([]string{"issues", "--message", strconv.FormatInt(id, 10)})
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	require.NoError(command.ExecuteContext(ctx))
	rows := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	assert.Len(rows, 10)
	for _, row := range rows {
		assert.Len(strings.Split(row, "\t"), 3, row)
	}
	assert.Contains(stderr.String(), "More issues cite this source")
}

func TestKataIssuesRequiresMessage(t *testing.T) {
	command := newKataCmd()
	command.SetArgs([]string{"issues"})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	require.ErrorContains(t, command.ExecuteContext(t.Context()), `required flag(s) "message" not set`)
}

func runKataCommand(ctx context.Context, t *testing.T, input string, args ...string) []byte {
	t.Helper()
	command := newKataCmd()
	command.SetArgs(args)
	command.SetIn(strings.NewReader(input))
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(io.Discard)
	require.NoError(t, command.ExecuteContext(ctx))
	return output.Bytes()
}

func TestKataCommandsRefuseAnOlderDaemon(t *testing.T) {
	var version atomic.Pointer[string]
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			assert.Fail(t, "unexpected request", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"` + *version.Load() + `"}`))
	}))
	t.Cleanup(daemon.Close)
	ctx := configureRemoteDaemonForTest(t, daemon.URL)
	for _, args := range [][]string{{"evidence", "prepare"}, {"create", "--idempotency-key", "key-1"}, {"link", "example#abcd"}, {"issues", "--message", "1"}} {
		// The lookup needs a newer daemon than the other commands.
		version.Store(new("3.1.0"))
		if args[0] == "issues" {
			version.Store(new("3.2.0"))
		}
		command := newKataCmd()
		command.SetArgs(args)
		command.SetIn(strings.NewReader(kataInputFor(args[0])))
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		err := command.ExecuteContext(ctx)
		require.ErrorContains(t, err, "too old for Kata issues", args[0])
	}
}

func kataInputFor(command string) string {
	switch command {
	case "evidence":
		return `{"selectors":[{"kind":"message","message_id":1,"max_chars":1000}]}`
	case "create":
		return `{"title":"Send the budget","evidence":[]}`
	}
	return `{"evidence":[]}`
}
