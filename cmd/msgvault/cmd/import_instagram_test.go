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

func TestImportInstagramRejectsNegativeLimit(t *testing.T) {
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
	command := newImportInstagramCmd()
	command.SetContext(withStoreResolverConfig(t, lifecycleTestConfig(t.TempDir())))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--account", "example", "--self-name", "Nova Example", "--limit=-1", "/fictional/export"})
	require.ErrorContains(command.Execute(), "--limit must be non-negative")
}

func TestImportInstagramRefreshesCacheAfterPartialFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	testCtx := withStoreResolverConfig(t, cfg)
	root := t.TempDir()
	good := filepath.Join(root, "your_instagram_activity", "messages", "inbox", "a")
	require.NoError(os.MkdirAll(good, 0700))
	require.NoError(os.WriteFile(filepath.Join(good, "message_1.json"), []byte(`{"thread_path":"inbox/a","messages":[{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"retained"}]}`), 0600))
	dir := filepath.Join(root, "your_instagram_activity", "messages", "inbox", "fictional")
	require.NoError(os.MkdirAll(dir, 0700))
	require.NoError(os.WriteFile(filepath.Join(dir, "message_1.json"), []byte(`{"thread_path":"inbox/fictional","messages":[{"sender_name":"Mira Example","timestamp_ms":1730500000123,"content":"retained before conflict"}]}`), 0600))
	require.NoError(os.WriteFile(filepath.Join(dir, "message_2.json"), []byte(`{"thread_path":"inbox/other","messages":[{"timestamp_ms":1730500001123,"content":"conflicting key"}]}`), 0600))
	command := &cobra.Command{Use: "import-instagram"}
	command.SetContext(testCtx)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	err := runImportInstagram(command, root, instagramCLIOptions{Account: "example", SelfName: "Nova Example"})
	require.ErrorContains(err, "conflicting thread_path")
	db, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { require.NoError(db.Close()) })
	var n int
	require.NoError(db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?,hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
	assert.Equal(1, n)
}

func TestImportInstagramRetriesCacheAfterUnchangedReplay(t *testing.T) {
	cfg := lifecycleTestConfig(t.TempDir())
	ctx := withStoreResolverConfig(t, cfg)
	root := t.TempDir()
	dir := filepath.Join(root, "your_instagram_activity", "messages", "inbox", "fictional")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "message_1.json"), []byte(`{"thread_path":"inbox/fictional","messages":[{"timestamp_ms":1730500000123,"content":"retained"}]}`), 0600))
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg.AnalyticsDir()), 0700))
	require.NoError(t, os.WriteFile(cfg.AnalyticsDir(), []byte("obstruction"), 0600))
	command := &cobra.Command{Use: "import-instagram"}
	command.SetContext(ctx)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	opts := instagramCLIOptions{Account: "example", SelfName: "Nova Example"}
	require.Error(t, runImportInstagram(command, root, opts))
	require.NoError(t, os.Remove(cfg.AnalyticsDir()))
	require.NoError(t, runImportInstagram(command, root, opts))
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	var n int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?,hive_partitioning=true)`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
	assert.Equal(t, 1, n)
}
