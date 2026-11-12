package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

type cancelOnDraftOutput context.CancelFunc

func (cancel cancelOnDraftOutput) Write(p []byte) (int, error) {
	cancel()
	return len(p), nil
}

func TestDraftInterruptionPreservesReceivedReconciliation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cli/run" {
			writeDaemonTestHealth(t, w)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, err := w.Write([]byte(`{"type":"stderr","data":"{\"draft_id\":\"fixture-draft\",\"receipt\":\"fixture-receipt\",\"manual_reconciliation\":true}\n"}` + "\n" +
			`{"type":"stdout","data":"receipt received\n"}` + "\n"))
		assert.NoError(err)
		assert.NoError(http.NewResponseController(w).Flush())
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{HomeDir: t.TempDir()}
	cfg.Data.DataDir = t.TempDir()
	cfg.Remote.URL, cfg.Remote.AllowInsecure = server.URL, true
	root := newRootCommand()
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		prepareInvocation(cmd).cfg = cfg
		return nil
	}
	group := &cobra.Command{Use: "draft"}
	leaf := &cobra.Command{Use: "delete <id>", Args: cobra.ExactArgs(1), RunE: runDaemonCLICommandHTTPFromCobra}
	leaf.Flags().Bool("json", false, "")
	group.AddCommand(leaf)
	root.AddCommand(group)
	var diagnostic bytes.Buffer
	root.SetOut(cancelOnDraftOutput(cancel))
	root.SetErr(&diagnostic)
	args := []string{"draft", "delete", "fixture-draft", "--json"}
	root.SetArgs(args)
	err := executeRootContext(ctx, root, args)
	require.ErrorIs(err, context.Canceled)
	assert.Equal(130, ExitCode(err))
	var got struct {
		Error struct {
			Code    string         `json:"code"`
			Hint    string         `json:"hint"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(json.Unmarshal(diagnostic.Bytes(), &got))
	assert.Equal("interrupted", got.Error.Code)
	assert.Equal("fixture-draft", got.Error.Details["draft_id"])
	assert.Equal("fixture-receipt", got.Error.Details["receipt"])
	assert.Equal(true, got.Error.Details["manual_reconciliation"])
	assert.Contains(got.Error.Hint, "msgvault draft delete --help")
	assert.NotContains(got.Error.Hint, "rerun")
}

func TestDraftFailureHasOneDiagnosticWithReconciliation(t *testing.T) {
	payload, err := json.Marshal(map[string]any{"type": "stderr", "data": "{\"status\":\"pending\",\"draft_id\":\"fixture-draft\",\"manual_reconciliation\":true}\n"})
	require := require.New(t)
	assert := assert.New(t)
	require.NoError(err)
	server, _ := newDaemonCLIRunnerTestServer(t, nil, string(payload), `{"type":"error","error":"revision_mismatch","code":"revision_mismatch"}`)
	cfg := &config.Config{HomeDir: t.TempDir()}
	cfg.Data.DataDir = t.TempDir()
	cfg.Remote.URL = server.URL
	cfg.Remote.AllowInsecure = true
	root := newRootCommand()
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error { inv := prepareInvocation(cmd); inv.cfg = cfg; return nil }
	group := &cobra.Command{Use: "draft"}
	leaf := &cobra.Command{Use: "get <id>", Args: cobra.ExactArgs(1), RunE: runDaemonCLICommandHTTPFromCobra}
	leaf.Flags().Bool("json", false, "")
	group.AddCommand(leaf)
	root.AddCommand(group)
	var out, diagnostic bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&diagnostic)
	args := []string{"draft", "get", "fixture-draft", "--json"}
	root.SetArgs(args)
	err = executeRootContext(t.Context(), root, args)
	require.Error(err)
	assert.Empty(out.String())
	var got struct {
		Error struct {
			Hint    string         `json:"hint"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(json.Unmarshal(diagnostic.Bytes(), &got))
	assert.NotEmpty(got.Error.Hint)
	assert.Equal("fixture-draft", got.Error.Details["draft_id"])
	assert.Equal(true, got.Error.Details["manual_reconciliation"])
}
