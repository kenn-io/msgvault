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

func TestImportRedditEndToEnd(t *testing.T) {
	cfg := lifecycleTestConfig(t.TempDir())
	cmd := &cobra.Command{Use: "import-reddit"}
	cmd.SetContext(withStoreResolverConfig(t, cfg))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	fixture, err := filepath.Abs("../../../internal/reddit/testdata/export")
	require.NoError(t, err)
	require.NoError(t, runImportReddit(cmd, fixture, redditCLIOptions{}))
	assert.Contains(t, out.String(), "6 added, 2 already archived")
	assert.Contains(t, out.String(), "votes.csv")
}

func TestImportRedditRefreshesCacheAfterPartialFailure(t *testing.T) {
	cfg := lifecycleTestConfig(t.TempDir())
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title,date\na,lantern,2026-05-24 03:19:35 UTC\nb,\"unterminated\n"), 0600))
	cmd := &cobra.Command{Use: "import-reddit"}
	cmd.SetContext(withStoreResolverConfig(t, cfg))
	cmd.SetOut(io.Discard)
	require.ErrorContains(t, runImportReddit(cmd, root, redditCLIOptions{Me: "fictional_owner"}), "posts.csv")
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	var n int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?, hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestImportRedditRejectsClientFilesOnRemote(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	command := newImportRedditCmd()
	command.SetContext(configureRemoteDaemonForTest(t, server.URL))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"export"})
	require.ErrorContains(t, command.Execute(), "run it on the daemon host with --local")
	assert.Zero(t, requests.Load())
}
