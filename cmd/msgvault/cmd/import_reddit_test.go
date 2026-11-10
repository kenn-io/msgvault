package cmd

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportRedditRefreshesCacheAfterPartialFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title,date\na,lantern,2026-05-24 03:19:35 UTC\nb,\"unterminated\n"), 0600))
	require.NoError(os.WriteFile(filepath.Join(root, "votes.csv"), []byte("id,vote\none,up\n"), 0600))
	cmd := &cobra.Command{Use: "import-reddit"}
	cmd.SetContext(withStoreResolverConfig(t, cfg))
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.ErrorContains(runImportReddit(cmd, root, redditCLIOptions{Me: "fictional_owner"}), "posts.csv")
	assert.Contains(out.String(), "Unsupported: votes.csv")
	db, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { _ = db.Close() })
	var n int
	require.NoError(db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?, hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
	assert.Equal(1, n)
	require.NoError(os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title,date\na,lantern,2026-05-24 03:19:35 UTC\nb,recovered,2026-05-24 03:20:35 UTC\n"), 0600))
	require.NoError(runImportReddit(cmd, root, redditCLIOptions{Me: "fictional_owner"}))
	require.NoError(db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?, hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
	assert.Equal(2, n)
}

func TestImportRedditRejectsClientFilesOnRemote(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	command := newImportRedditCmd()
	command.SetContext(configureRemoteDaemonForTest(t, server.URL))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"export"})
	require.ErrorContains(command.Execute(), "run it on the daemon host with --local")
	assert.Zero(requests.Load())
}
