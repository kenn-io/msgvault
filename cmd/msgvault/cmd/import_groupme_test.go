package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestImportGroupmeEndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
	command, _, err := rootCmd.Find([]string{"import-groupme"})
	require.NoError(err)
	homeFlag := rootCmd.PersistentFlags().Lookup("home")
	previousHome, previousHomeChanged := homeFlag.Value.String(), homeFlag.Changed
	t.Cleanup(func() {
		command.Flags().VisitAll(func(flag *pflag.Flag) {
			require.NoError(flag.Value.Set(flag.DefValue))
			flag.Changed = false
		})
		require.NoError(homeFlag.Value.Set(previousHome))
		homeFlag.Changed = previousHomeChanged
	})
	tmp := t.TempDir()
	prevOut, prevErr := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr()
	t.Cleanup(func() { rootCmd.SetOut(prevOut); rootCmd.SetErr(prevErr); rootCmd.SetArgs(nil) })
	fixture, err := filepath.Abs("../../../internal/groupme/testdata")
	require.NoError(err)
	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"--home", tmp, "import-groupme", "--me", "fictional-owner", fixture})
	require.NoError(rootCmd.ExecuteContext(t.Context()))
	assert.Contains(stdout.String(), "Import complete.")
	assert.Contains(stdout.String(), "Year 2024 (UTC): 2")
	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE message_type = 'groupme'").Scan(&n))
	assert.Equal(4, n)
}

func TestImportGroupmeRejectsClientFilesOnRemote(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	command := newImportGroupmeCmd()
	command.SetContext(configureRemoteDaemonForTest(t, server.URL))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--me", "fictional-owner", "export"})
	require.ErrorContains(t, command.Execute(), "run it on the daemon host with --local")
	assert.Zero(t, requests.Load())
}
func TestImportGroupmeRequiresOwnerBeforeDaemonRequest(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	command := newImportGroupmeCmd()
	command.SetContext(configureRemoteDaemonForTest(t, server.URL))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"export"})
	require.ErrorContains(t, command.Execute(), "--me")
	assert.Zero(t, requests.Load())
}

// A cancellation after a real commit still runs identity migrations and refreshes
// the Parquet cache from the partial archive, using an uncancelled context.
func TestImportGroupmePartialImportRefreshesIdentityAndCache(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	testCtx := withStoreResolverConfig(t, cfg)
	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "message.json"), []byte(`[{"id":"first","group_id":"fictional-group","sender_id":"fictional-owner","created_at":1704067200,"text":"first"},{"id":"second","group_id":"fictional-group","sender_id":"fictional-peer","created_at":1704067201,"text":"second"}]`), 0600))
	probe, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(probe.Close()) })
	ctx, cancel := context.WithCancel(testCtx)
	defer cancel()
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var n int
		require.NoError(probe.DB().QueryRowContext(context.WithoutCancel(t.Context()), "SELECT count(*) FROM messages").Scan(&n))
		return n > 0
	})
	command := &cobra.Command{Use: "import-groupme"}
	command.SetContext(ctx)
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(io.Discard)
	require.ErrorIs(runImportGroupme(command, root, groupmeCLIOptions{Me: "fictional-owner"}), context.Canceled)
	require.True(canceled())
	assert.Contains(output.String(), "Partial import")
	var sourceID int64
	require.NoError(probe.DB().QueryRowContext(t.Context(), "SELECT id FROM sources WHERE source_type='groupme'").Scan(&sourceID))
	identities, err := probe.ListAccountIdentitiesContext(t.Context(), sourceID)
	require.NoError(err)
	assert.NotEmpty(identities)
	db, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(db.Close()) })
	var n int
	require.NoError(db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?, hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
	assert.Equal(1, n)
}

func TestImportGroupmeInvalidInputHasNoPartialSummary(t *testing.T) {
	cfg := lifecycleTestConfig(t.TempDir())
	ctx := withStoreResolverConfig(t, cfg)
	for _, path := range []string{filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		command := &cobra.Command{Use: "import-groupme"}
		command.SetContext(ctx)
		var out bytes.Buffer
		command.SetOut(&out)
		command.SetErr(io.Discard)
		require.Error(t, runImportGroupme(command, path, groupmeCLIOptions{Me: "owner"}))
		assert.NotContains(t, out.String(), "Partial import")
		assert.NotContains(t, out.String(), "Known dates")
	}
}
