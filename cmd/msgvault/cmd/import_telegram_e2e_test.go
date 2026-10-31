package cmd

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportTelegramEndToEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
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
	assert.Contains(stdout.String(), "Import complete")
	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	var n int
	require.NoError(st.DB().QueryRow("SELECT count(*) FROM messages WHERE message_type = 'telegram'").Scan(&n))
	assert.Equal(3, n)
	require.NoError(st.DB().QueryRow("SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'hello'").Scan(&n))
	assert.Equal(1, n)
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
	require.NoError(db.QueryRow(`SELECT count(*) FROM read_parquet(?, hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
	assert.Equal(1, n)
	require.NoError(db.QueryRow(`SELECT count(*) FROM read_parquet(?)`, filepath.Join(cfg.AnalyticsDir(), "owner_participants", "*.parquet")).Scan(&n))
	assert.Equal(1, n)
}

func TestImportTelegramRejectsClientFilesOnRemote(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	command := newImportTelegramCmd()
	command.SetContext(ctx)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"export"})
	err := command.Execute()
	require.ErrorContains(t, err, "run it on the daemon host with --local")
	assert.Zero(t, requests.Load())
}

func TestImportTelegramRejectsNegativeLimit(t *testing.T) {
	command := newImportTelegramCmd()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--limit=-1", "export"})
	require.ErrorContains(t, command.Execute(), "--limit must be non-negative")
}

func TestImportTelegramConfirmsOwnerAfterWrites(t *testing.T) {
	for _, partial := range []bool{false, true} {
		for _, optOut := range []string{"none", "flag", "source"} {
			t.Run(fmt.Sprintf("partial=%t/opt-out=%s", partial, optOut), func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				cfg := lifecycleTestConfig(t.TempDir())
				ctx := withStoreResolverConfig(t, cfg)
				root := t.TempDir()
				chats := `{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1","from_id":"user1","text":"owner message"}]}`
				if partial {
					chats += `,{"id":3,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"invalid"}]}`
				}
				require.NoError(os.WriteFile(filepath.Join(root, "result.json"), []byte(`{"personal_information":{"user_id":1},"chats":{"list":[`+chats+`]}}`), 0600))
				st := testutil.NewTestStore(t)
				if optOut == "source" {
					source, err := st.GetOrCreateSource("telegram", "user1")
					require.NoError(err)
					_, err = st.DB().Exec(st.Rebind(`UPDATE sources SET sync_config = ? WHERE id = ?`), `{"no_default_identity":true}`, source.ID)
					require.NoError(err)
				}
				summary, err := telegram.Import(t.Context(), st, telegram.ImportOptions{RootDir: root})
				if partial {
					require.ErrorContains(err, "date_unixtime")
				} else {
					require.NoError(err)
				}
				require.EqualValues(1, summary.MessagesAdded)
				require.NoError(runTelegramPostImportMigrations(io.Discard, st, summary, optOut == "flag", invocationFromContext(ctx)))
				var n int
				require.NoError(st.DB().QueryRow("SELECT count(*) FROM account_identities WHERE address = 'user1'").Scan(&n))
				want := 1
				if optOut != "none" {
					want = 0
				}
				assert.Equal(want, n)
			})
		}
	}
}
