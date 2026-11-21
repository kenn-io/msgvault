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
	"go.kenn.io/msgvault/internal/tiktokexport"
)

func TestImportTikTokRejectsRemoteClientFiles(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	command := newImportTikTokCmd()
	command.SetContext(ctx)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"example-account", "export.zip"})
	require.ErrorContains(t, command.Execute(), "run it on the daemon host with --local")
	assert.Zero(t, requests.Load())
}

func TestImportTikTokEndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
	command, _, err := rootCmd.Find([]string{"import-tiktok"})
	require.NoError(err)
	require.Equal("import-tiktok", command.Name())
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
	fixture, err := filepath.Abs("../../../internal/tiktokexport/testdata/json")
	require.NoError(err)
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs([]string{"--home", home, "import-tiktok", "example-account", fixture, "--json"})
	captured := captureStdout(t)
	err = rootCmd.ExecuteContext(t.Context())
	processOutput := captured()
	require.NoError(err, stderr.String())
	assert.Empty(processOutput, "JSON imports must not print cache progress on process stdout")
	var summary tiktokexport.Summary
	require.NoError(json.Unmarshal(stdout.Bytes(), &summary))
	assert.Equal(int64(4), summary.MessagesAdded)
	assert.Contains(stderr.String(), "1 unknown")
}
