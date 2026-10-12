package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
)

// Native Messenger reports hard per-thread failures in its summary while
// returning nil. Command JSON must retain the run ID and report it incomplete.
func TestImportCommandJSONKeepsPartialRunIdentityWithoutClaimingCompletion(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	cfg, parent := importCommandCaptureFixture(t, true)
	_, err := parent.DB().Exec(`CREATE TRIGGER synthetic_import_body_failure BEFORE INSERT ON message_bodies BEGIN SELECT RAISE(ABORT,'synthetic body failure'); END`)
	require.NoError(err)
	root := t.TempDir()
	thread := filepath.Join(root, "messages", "inbox", "synthetic_thread")
	require.NoError(os.MkdirAll(thread, 0700))
	require.NoError(os.WriteFile(filepath.Join(thread, "message_1.json"), []byte(`{"participants":[{"name":"Synthetic Owner"},{"name":"Synthetic Sender"}],"messages":[{"sender_name":"Synthetic Sender","timestamp_ms":978350400000,"content":"Synthetic body","type":"Generic"}],"title":"Synthetic thread","is_still_participant":true,"thread_path":"inbox/synthetic_thread"}`), 0600))
	command := cloneImportCommandForTest(t, importMessengerCmd)
	command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	command.SetErr(io.Discard)
	command.SetArgs([]string{root, "--me", "synthetic.owner@facebook.messenger", "--format", "json", "--no-resume", "--notify-imports", "--json"})
	finish := captureStdout(t)
	runErr := command.Execute()
	output := finish()
	require.NoError(runErr, "preserve the native command's existing summary-error exit behavior")
	var result struct {
		ImportRunIDs []string `json:"import_run_ids"`
		Completed    bool     `json:"completed"`
	}
	require.NoError(json.Unmarshal([]byte(output), &result))
	require.Len(result.ImportRunIDs, 1)
	var persistedComplete bool
	require.NoError(parent.DB().QueryRow("SELECT completed FROM import_runs WHERE id=?", result.ImportRunIDs[0]).Scan(&persistedComplete))
	assert.False(persistedComplete, "the real parser must have failed its body write")
	assert.False(result.Completed, "partial native work must not be reported complete merely because RunE returned nil")
	for _, table := range []string{"messages", "import_message_decisions", "mcp_event_log"} {
		var count int
		require.NoError(parent.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
		assert.Zero(count, table)
	}
}
