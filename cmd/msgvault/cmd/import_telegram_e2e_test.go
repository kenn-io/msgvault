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
)

func TestImportTelegramEndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
	command, _, err := rootCmd.Find([]string{"import-telegram"})
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
	fixture, err := filepath.Abs("../../../internal/telegram/testdata/full")
	require.NoError(err)
	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"--home", tmp, "import-telegram", "--limit", "3", fixture})
	require.NoError(rootCmd.ExecuteContext(t.Context()))
	assert.Contains(stdout.String(), "Import paused at --limit. More messages remain; re-run to continue.")
	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE message_type = 'telegram'").Scan(&n))
	assert.Equal(3, n)
}

func TestImportTelegramRefreshesCacheAfterPartialFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	testCtx := withStoreResolverConfig(t, cfg)
	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "result.json"), []byte(`{"personal_information":{"user_id":1},"chats":{"list":[{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1610909872","from_id":"user1","text":"committed before failure"}]},{"id":3,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"invalid"}]}]}}`), 0600))
	cmd := &cobra.Command{Use: "import-telegram"}
	cmd.SetContext(testCtx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := runImportTelegram(cmd, root, telegramCLIOptions{})
	require.ErrorContains(err, "date_unixtime")
	db, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { _ = db.Close() })
	var n int
	require.NoError(db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?, hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
	assert.Equal(1, n)
	require.NoError(db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?)`, filepath.Join(cfg.AnalyticsDir(), "owner_participants", "*.parquet")).Scan(&n))
	assert.Equal(1, n)
}

func TestImportTelegramRejectsNegativeLimit(t *testing.T) {
	command := newImportTelegramCmd()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--limit=-1", "export"})
	require.ErrorContains(t, command.Execute(), "--limit must be non-negative")
}
