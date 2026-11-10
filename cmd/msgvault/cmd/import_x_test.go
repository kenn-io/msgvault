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
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportXRefreshesCacheAfterPartialFailure(t *testing.T) {
	cfg := lifecycleTestConfig(t.TempDir())
	testCtx := withStoreResolverConfig(t, cfg)
	root := t.TempDir()
	body := `window.YTD.direct_messages.part0 = [{"dmConversation":{"conversationId":"1-2","messages":[{"messageCreate":{"id":"1","senderId":"1","createdAt":"2020-01-02T03:04:05.000Z","text":"committed"}}, invalid]}}]`
	require.NoError(t, os.WriteFile(filepath.Join(root, "direct-messages.js"), []byte(body), 0600))
	cmd := &cobra.Command{Use: "import-x"}
	cmd.SetContext(testCtx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	require.Error(t, runImportX(cmd, root, xCLIOptions{Me: "1"}))
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	var n int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?, hive_partitioning=true) WHERE message_type='x_dm'`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
	assert.Equal(t, 1, n)
	archive, err := store.Open(cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, archive.Close()) })
	engine, err := query.NewDuckDBEngine(t.Context(), cfg.AnalyticsDir(), "", archive.DB())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, engine.Close()) })
	conversations, err := engine.ListConversations(t.Context(), query.TextFilter{})
	require.NoError(t, err)
	assert.Len(t, conversations, 1)
	hits, err := engine.TextSearch(t.Context(), "committed", nil, 10, 0)
	require.NoError(t, err)
	assert.Len(t, hits, 1)

}
func TestImportXRejectsClientFilesOnRemote(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	cmd := newImportXCmd()
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"export"})
	require.ErrorContains(t, cmd.Execute(), "run it on the daemon host with --local")
	assert.Zero(t, requests.Load())
}

func TestImportXCommandEndToEnd(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	cfg := lifecycleTestConfig(t.TempDir())
	ctx := withStoreResolverConfig(t, cfg)
	root, err := filepath.Abs("../../../internal/xarchive/testdata")
	require.NoError(t, err)
	command := newImportXCmd()
	command.SetContext(ctx)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"--me", "100001", root})
	require.NoError(t, command.Execute())
	archive, err := store.Open(cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, archive.Close()) })
	engine, err := query.NewDuckDBEngine(t.Context(), cfg.AnalyticsDir(), "", archive.DB())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, engine.Close()) })
	conversations, err := engine.ListConversations(t.Context(), query.TextFilter{})
	require.NoError(t, err)
	assert.Len(t, conversations, 2)
	hits, err := engine.TextSearch(t.Context(), "Synthetic", nil, 10, 0)
	require.NoError(t, err)
	assert.Len(t, hits, 3)
}
