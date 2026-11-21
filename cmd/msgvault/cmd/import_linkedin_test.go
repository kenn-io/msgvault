package cmd

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportLinkedInValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing archive", []string{"example"}, "accepts 2 arg(s)"},
		{"missing --me", []string{"example", "export.zip"}, "LinkedIn member profile URL"},
		{"not a profile URL", []string{"example", "export.zip", "--me", "https://www.linkedin.com/company/example"}, "LinkedIn member profile URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := newImportLinkedInCmd()
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			command.SetArgs(tc.args)
			require.ErrorContains(t, command.ExecuteContext(t.Context()), tc.want)
		})
	}
}

func TestImportLinkedInRejectsRemoteClientFiles(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	command := newImportLinkedInCmd()
	command.SetContext(ctx)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"example", "export.zip", "--me", "https://www.linkedin.com/in/alex-example/"})
	require.ErrorContains(t, command.Execute(), "run it on the daemon host with --local")
	assert.Zero(t, requests.Load())
}

func TestImportLinkedInEndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
	command, _, err := rootCmd.Find([]string{"import-linkedin"})
	require.NoError(err)
	require.Equal("import-linkedin", command.Name())
	homeFlag := rootCmd.PersistentFlags().Lookup("home")
	previousHome, previousChanged := homeFlag.Value.String(), homeFlag.Changed
	prevOut, prevErr := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr()
	t.Cleanup(func() {
		command.Flags().VisitAll(func(flag *pflag.Flag) { require.NoError(flag.Value.Set(flag.DefValue)); flag.Changed = false })
		require.NoError(homeFlag.Value.Set(previousHome))
		homeFlag.Changed = previousChanged
		rootCmd.SetOut(prevOut)
		rootCmd.SetErr(prevErr)
		rootCmd.SetArgs(nil)
	})
	home := t.TempDir()
	fixture, err := filepath.Abs("../../../internal/linkedin/testdata/export")
	require.NoError(err)
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs([]string{"--home", home, "import-linkedin", "example-account", fixture, "--me", "https://www.linkedin.com/in/alex-example/", "--json"})
	captured := captureStdout(t)
	err = rootCmd.ExecuteContext(t.Context())
	processOutput := captured()
	require.NoError(err, stderr.String())
	assert.Empty(processOutput, "JSON imports must not print cache progress on process stdout")
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
