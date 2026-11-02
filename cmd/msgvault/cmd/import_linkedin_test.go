package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportLinkedInValidation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{}, "identifier"},
		{[]string{"example", "export.zip"}, "--me"},
		{[]string{"example", "export.zip", "--me", "not-a-profile"}, "profile URL"},
	} {
		cmd := newImportLinkedInCmd()
		var stderr bytes.Buffer
		cmd.SetOut(io.Discard)
		cmd.SetErr(&stderr)
		cmd.SetArgs(append(tc.args, "--json"))
		err := cmd.ExecuteContext(context.Background())
		require.Error(t, err)
		assert.Contains(t, stderr.String(), tc.want)
		var failure map[string]string
		require.NoError(t, json.Unmarshal(stderr.Bytes(), &failure))
		assert.Contains(t, failure["error"], tc.want)
		assert.NotEmpty(t, failure["hint"])
		assert.True(t, cmd.SilenceErrors)
	}
}

func TestImportLinkedInForwarding(t *testing.T) {
	root := &cobra.Command{Use: "msgvault"}
	parent := &cobra.Command{Use: "import"}
	root.AddCommand(parent)
	command := newImportLinkedInCmd()
	parent.AddCommand(command)
	require.NoError(t, command.ParseFlags([]string{"example", "export.zip", "--me", "https://www.linkedin.com/in/alex-example/", "--include-drafts", "--json"}))
	args, err := daemonCLIArgsFromCobra(command, command.Flags().Args())
	require.NoError(t, err)
	assert.Equal(t, []string{"import", "linkedin", "--include-drafts", "--json", "--me=https://www.linkedin.com/in/alex-example/", "example", "export.zip"}, args)
	assert.True(t, manualSyncCLICommand(args), "the daemon must queue cache refresh after import")
}

func TestImportLinkedInEndToEnd(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	t.Cleanup(saveMessengerState(t))
	home := t.TempDir()
	fixture, err := filepath.Abs("../../../internal/linkedin/testdata/export")
	require.NoError(t, err)
	// Use a fresh child to avoid leaking its parsed flag state into other tests.
	child := newImportLinkedInCmd()
	existing, _, err := rootCmd.Find([]string{"import", "linkedin"})
	require.NoError(t, err)
	importCmd.RemoveCommand(existing)
	importCmd.AddCommand(child)
	t.Cleanup(func() { importCmd.RemoveCommand(child); importCmd.AddCommand(existing) })
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs([]string{"--home", home, "import", "linkedin", "example-account", fixture, "--me", "https://www.linkedin.com/in/alex-example/", "--json"})
	require.NoError(t, executeRootContext(t.Context(), rootCmd), stderr.String())
	var summary map[string]int64
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &summary))
	assert.Equal(t, int64(4), summary["messages_added"])
	st, err := store.Open(filepath.Join(home, "msgvault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	sources, err := st.GetSourcesByIdentifier("example-account")
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, "linkedin_export", sources[0].SourceType)
	results, total, err := st.SearchMessages("quoted", 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, results, 1)
	assert.Contains(t, results[0].SourceMessageID, "comment:")
}
