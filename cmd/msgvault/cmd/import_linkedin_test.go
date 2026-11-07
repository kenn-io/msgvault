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
	assert := assert.New(t)
	require := require.New(t)
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
		require.Error(err)
		assert.Contains(stderr.String(), tc.want)
		var failure map[string]string
		require.NoError(json.Unmarshal(stderr.Bytes(), &failure))
		assert.Contains(failure["error"], tc.want)
		assert.NotEmpty(failure["hint"])
		assert.True(cmd.SilenceErrors)
	}
}

func TestImportLinkedInForwarding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := &cobra.Command{Use: "msgvault"}
	parent := &cobra.Command{Use: "import"}
	root.AddCommand(parent)
	command := newImportLinkedInCmd()
	parent.AddCommand(command)
	require.NoError(command.ParseFlags([]string{"example", "export.zip", "--me", "https://www.linkedin.com/in/alex-example/", "--include-drafts", "--json"}))
	args, err := daemonCLIArgsFromCobra(command, command.Flags().Args())
	require.NoError(err)
	assert.Equal([]string{"import", "linkedin", "--include-drafts", "--json", "--me=https://www.linkedin.com/in/alex-example/", "example", "export.zip"}, args)
	assert.True(manualSyncCLICommand(args), "the daemon must queue cache refresh after import")
}

func TestImportLinkedInEndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
	t.Cleanup(saveMessengerState(t))
	home := t.TempDir()
	fixture, err := filepath.Abs("../../../internal/linkedin/testdata/export")
	require.NoError(err)
	// Use a fresh child to avoid leaking its parsed flag state into other tests.
	child := newImportLinkedInCmd()
	existing, _, err := rootCmd.Find([]string{"import", "linkedin"})
	require.NoError(err)
	importCmd.RemoveCommand(existing)
	importCmd.AddCommand(child)
	t.Cleanup(func() { importCmd.RemoveCommand(child); importCmd.AddCommand(existing) })
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs([]string{"--home", home, "import", "linkedin", "example-account", fixture, "--me", "https://www.linkedin.com/in/alex-example/", "--json"})
	require.NoError(executeRootContext(t.Context(), rootCmd), stderr.String())
	var summary map[string]int64
	require.NoError(json.Unmarshal(stdout.Bytes(), &summary))
	assert.Equal(int64(4), summary["messages_added"])
	st, err := store.Open(filepath.Join(home, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	sources, err := st.GetSourcesByIdentifier("example-account")
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal("linkedin_export", sources[0].SourceType)
	results, total, err := st.SearchMessages("quoted", 0, 10)
	require.NoError(err)
	assert.EqualValues(1, total)
	require.Len(results, 1)
	assert.Contains(results[0].SourceMessageID, "comment:")
}
