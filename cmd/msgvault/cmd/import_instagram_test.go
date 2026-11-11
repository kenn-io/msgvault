package cmd

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportInstagramRejectsClientFilesOnRemote(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	command := newImportInstagramCmd()
	command.SetContext(ctx)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--account", "example", "--self-name", "Nova Example", "/fictional/export"})
	require.ErrorContains(command.Execute(), "run it on the daemon host with --local")
	assert.Zero(requests.Load())
}

func TestImportInstagramRefreshesCache(t *testing.T) {
	for _, scenario := range []string{"partial failure", "unchanged replay"} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			cfg := lifecycleTestConfig(t.TempDir())
			root := t.TempDir()
			dir := filepath.Join(root, "your_instagram_activity", "messages", "inbox", "a")
			require.NoError(os.MkdirAll(dir, 0700))
			require.NoError(os.WriteFile(filepath.Join(dir, "message_1.json"), []byte(`{"thread_path":"inbox/a","messages":[{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"retained"}]}`), 0600))
			command := &cobra.Command{Use: "import-instagram"}
			command.SetContext(withStoreResolverConfig(t, cfg))
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			opts := instagramCLIOptions{Account: "example", SelfName: "Nova Example"}
			if scenario == "partial failure" {
				bad := filepath.Join(root, "your_instagram_activity", "messages", "inbox", "fictional")
				require.NoError(os.MkdirAll(bad, 0700))
				require.NoError(os.WriteFile(filepath.Join(bad, "message_1.json"), []byte(`{"thread_path":"inbox/fictional","messages":[{"timestamp_ms":1730500000123,"content":"first conflicting shard"}]}`), 0600))
				require.NoError(os.WriteFile(filepath.Join(bad, "message_2.json"), []byte(`{"thread_path":"inbox/other","messages":[{"timestamp_ms":1730500001123,"content":"conflicting key"}]}`), 0600))
				require.ErrorContains(runImportInstagram(command, root, opts), "conversation paths conflict between message files")
			} else {
				require.NoError(os.MkdirAll(filepath.Dir(cfg.AnalyticsDir()), 0700))
				require.NoError(os.WriteFile(cfg.AnalyticsDir(), []byte("obstruction"), 0600))
				require.Error(runImportInstagram(command, root, opts))
				require.NoError(os.Remove(cfg.AnalyticsDir()))
				require.NoError(runImportInstagram(command, root, opts))
			}
			db, err := sql.Open("duckdb", "")
			require.NoError(err)
			t.Cleanup(func() { require.NoError(db.Close()) })
			var n int
			require.NoError(db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?,hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
			assert.Equal(1, n)
		})
	}
}
