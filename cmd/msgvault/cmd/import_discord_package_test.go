package cmd

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportDiscordPackageEndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
	command, _, err := rootCmd.Find([]string{"import-discord-package"})
	require.NoError(err)
	require.Equal("import-discord-package", command.Name())
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
	fixture, err := filepath.Abs("../../../internal/discord/testdata/package-csv")
	require.NoError(err)
	root := t.TempDir()
	require.NoError(os.CopyFS(root, os.DirFS(fixture)))
	require.NoError(os.Mkdir(filepath.Join(root, "messages/renamed"), 0o700))
	fixture = root
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"--home", home, "import-discord-package", fixture})
	require.NoError(rootCmd.ExecuteContext(t.Context()))
	assert.Contains(output.String(), "Coverage: exporting account's sent messages only")
	assert.Contains(output.String(), "Channels: 1 imported, 1 skipped")
	assert.Contains(output.String(), "1 missing local media")
	st, err := store.Open(filepath.Join(home, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages WHERE message_type='discord'`).Scan(&n))
	assert.Equal(5, n)
}

func TestImportDiscordPackageRejectsRemoteClientFiles(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	command := newImportDiscordPackageCmd()
	command.SetContext(ctx)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"package"})
	require.ErrorContains(t, command.Execute(), "run it on the daemon host with --local")
	assert.Zero(t, requests.Load())
}

func TestImportDiscordPackagePartialFailureReport(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	ctx := withStoreResolverConfig(t, cfg)
	root := t.TempDir()
	testutil.WriteFile(t, root, "account/user.json", []byte(`{"id":"100000000000000001"}`))
	testutil.WriteFile(t, root, "messages/200000000000000001/messages.json", []byte(`[{"ID":"1","Contents":"persisted before failure"},broken]`))
	command := &cobra.Command{Use: "import-discord-package"}
	command.SetContext(ctx)
	var output bytes.Buffer
	command.SetOut(&output)
	require.Error(runImportDiscordPackage(command, root, false))
	assert.Contains(output.String(), "Coverage: exporting account's sent messages only")
	assert.Contains(output.String(), "1 added")
	db, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { require.NoError(db.Close()) })
	var n int
	require.NoError(db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?,hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
	assert.Equal(1, n)
}
