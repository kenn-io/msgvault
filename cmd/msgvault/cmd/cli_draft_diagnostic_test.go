package cmd

import (
	"bytes"
	"encoding/json/v2"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

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
