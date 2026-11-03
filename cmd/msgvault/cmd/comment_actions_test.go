package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/meta"
)

type cliCommentBackend struct {
	calls, checks int
	request       meta.ActionRequest
	filter        api.CommentQueueRequest
	expect        string
	fail          bool
	failure       error
}

func (b *cliCommentBackend) SupportsAPISchemaVersion(context.Context, string) (bool, error) {
	b.checks++
	return true, nil
}
func (b *cliCommentBackend) RunCommentAction(_ context.Context, id int64, r meta.ActionRequest) (*meta.ActionResult, error) {
	b.calls++
	b.request = r
	result := &meta.ActionResult{MessageID: id, DryRun: r.DryRun, Confirm: "binding", Disclosure: meta.Disclosure{CommentID: "100_200_300"}, Receipt: &meta.Receipt{ID: 3, State: "unknown"}}
	if b.failure != nil {
		return result, b.failure
	}
	if b.fail {
		return result, &meta.ActionError{Code: "outcome_unknown", Message: "Inspect before resolving", Result: result}
	}
	return result, nil
}
func (b *cliCommentBackend) ListCommentActions(context.Context, int64) ([]meta.Receipt, error) {
	b.calls++
	return []meta.Receipt{{ID: 3, State: "succeeded"}}, nil
}
func (b *cliCommentBackend) UpdateCommentTriage(_ context.Context, _ int64, state, expect string) (string, error) {
	b.calls++
	b.expect = expect
	return state, nil
}
func (b *cliCommentBackend) ListCommentQueue(_ context.Context, f api.CommentQueueRequest) (*api.CommentQueueResponse, error) {
	b.calls++
	b.filter = f
	return &api.CommentQueueResponse{Items: []api.CommentQueueItem{{MessageID: 7, Triage: "open", Snippet: "Example\x1b[31m comment\nSpoof"}}, NextBeforeID: 7}, nil
}
func runCommentCLI(t *testing.T, b commentActionBackend, name string, args ...string) (string, error) {
	t.Helper()
	cmd := newCommentCommand(name, b)
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := executeRootContext(t.Context(), cmd, args)
	return out.String(), err
}
func TestCommentModerateFlagsExclusive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b := &cliCommentBackend{}
	for _, args := range [][]string{{"7"}, {"7", "--hide", "--delete"}, {"0", "--hide"}, {"7", "--hide", "--dry-run", "--resolve-unknown", "sent"}, {"7", "--hide", "--resolve-unknown", "invalid"}} {
		_, err := runCommentCLI(t, b, "moderate", args...)
		require.Error(err)
	}
	for _, args := range [][]string{{"--limit", "0"}, {"--limit", "201"}, {"--state", "bad"}, {"--before-id", "-1"}} {
		_, err := runCommentCLI(t, b, "queue", args...)
		require.Error(err)
	}
	assert.Zero(b.calls)
	assert.Zero(b.checks)
}
func TestCommentModerateDryRunPrintsConfirm(t *testing.T) {
	assert := assert.New(t)
	b := &cliCommentBackend{}
	out, err := runCommentCLI(t, b, "moderate", "7", "--delete", "--dry-run")
	require.NoError(t, err)
	assert.Contains(out, "binding")
	assert.Contains(out, "100_200_300")
	assert.True(b.request.DryRun)
	assert.Equal(meta.ActionDelete, b.request.Action)
}

func TestCommentTriageExpect(t *testing.T) {
	b := &cliCommentBackend{}
	out, err := runCommentCLI(t, b, "triage", "7", "--state", "handled", "--expect", "open")
	require.NoError(t, err)
	assert.Contains(t, out, "handled")
	assert.Equal(t, "open", b.expect)
}
func TestCommentQueueTable(t *testing.T) {
	assert := assert.New(t)
	b := &cliCommentBackend{}
	out, err := runCommentCLI(t, b, "queue", "--source", "app:9:page:100", "--limit", "1", "--before-id", "3")
	require.NoError(t, err)
	assert.Contains(out, "7")
	assert.Contains(out, "Example")
	assert.Contains(out, "\n7\topen\t")
	assert.NotContains(out, "\nSpoof")
	assert.NotContains(out, "\x1b")
	assert.Equal(int64(3), b.filter.BeforeID)
	assert.Equal(1, b.filter.Limit)
}
func TestCommentActionsOldDaemon(t *testing.T) {
	requests := 0
	client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.35.0"}`))
			return
		}
		requests++
	})
	for name, args := range map[string][]string{"moderate": {"7", "--hide"}, "triage": {"7", "--state", "open"}, "queue": {}, "actions": {"7"}} {
		_, err := runCommentCLI(t, client, name, args...)
		require.ErrorContains(t, err, "this daemon is too old for comment actions; upgrade it to API schema "+commentActionsMinAPISchemaVersion+" or newer")
	}
	assert.Zero(t, requests)
}

func TestMCPCommentToolsSchemaGate(t *testing.T) {
	for _, tc := range []struct {
		version string
		enabled bool
	}{{"3.35.0", false}, {"3.36.0", false}, {"3.37.0", false}, {commentActionsMinAPISchemaVersion, true}} {
		t.Run(tc.version, func(t *testing.T) {
			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					assert.NoError(t, json.MarshalWrite(w, map[string]any{"status": "ok", "api_schema_version": tc.version}))
					return
				}
				http.NotFound(w, r)
			})
			opts := daemonMCPServeOptions(t.Context(), client, invocationFromContext(t.Context()))
			assert.Equal(t, tc.enabled, opts.CommentActions != nil)
		})
	}
}
