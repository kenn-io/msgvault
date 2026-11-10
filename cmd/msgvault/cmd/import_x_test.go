package cmd

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportXRefreshesCacheAfterPartialFailure(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	cfg := lifecycleTestConfig(t.TempDir())
	testCtx := withStoreResolverConfig(t, cfg)
	root := t.TempDir()
	body := `window.YTD.direct_messages.part0 = [{"dmConversation":{"conversationId":"1-2","messages":[{"messageCreate":{"id":"1","senderId":"1","createdAt":"2020-01-02T03:04:05.000Z","text":"committed"}}, invalid]}}]`
	require.NoError(t, os.WriteFile(filepath.Join(root, "direct-messages.js"), []byte(body), 0600))
	var output bytes.Buffer
	invalid := newImportXCmd()
	invalid.SetContext(testCtx)
	invalid.SetOut(&output)
	invalid.SetErr(io.Discard)
	invalid.SetArgs([]string{root})
	require.ErrorContains(t, invalid.Execute(), "--me")
	assert.NotContains(t, output.String(), "X DM import:")
	assert.NotContains(t, output.String(), "Re-run to safely retry")
	cmd := newImportXCmd()
	cmd.SetContext(testCtx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--me", "1", root})
	require.Error(t, cmd.Execute())
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
