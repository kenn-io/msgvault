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
	assert := assert.New(t)
	require := require.New(t)
	markDaemonCLISubprocessForTest(t)
	cfg := lifecycleTestConfig(t.TempDir())
	testCtx := withStoreResolverConfig(t, cfg)
	root := t.TempDir()
	body := `window.YTD.direct_messages.part0 = [{"dmConversation":{"conversationId":"1-2","messages":[{"messageCreate":{"id":"1","senderId":"1","createdAt":"2020-01-02T03:04:05.000Z","text":"committed"}}, invalid]}}]`
	require.NoError(os.WriteFile(filepath.Join(root, "direct-messages.js"), []byte(body), 0600))
	var output bytes.Buffer
	invalid := newImportXCmd()
	invalid.SetContext(testCtx)
	invalid.SetOut(&output)
	invalid.SetErr(io.Discard)
	invalid.SetArgs([]string{root})
	require.ErrorContains(invalid.Execute(), "--me")
	assert.NotContains(output.String(), "X DM import:")
	assert.NotContains(output.String(), "Re-run to safely retry")
	cmd := newImportXCmd()
	cmd.SetContext(testCtx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--me", "1", root})
	require.Error(cmd.Execute())
	db, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(db.Close()) })
	var n int
	require.NoError(db.QueryRowContext(t.Context(), `SELECT count(*) FROM read_parquet(?, hive_partitioning=true) WHERE message_type='x_dm'`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet")).Scan(&n))
	assert.Equal(1, n)
	archive, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(archive.Close()) })
	engine, err := query.NewDuckDBEngine(t.Context(), cfg.AnalyticsDir(), "", archive.DB())
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(engine.Close()) })
	conversations, err := engine.ListConversations(t.Context(), query.TextFilter{})
	require.NoError(err)
	assert.Len(conversations, 1)
	hits, err := engine.TextSearch(t.Context(), "committed", nil, 10, 0)
	require.NoError(err)
	assert.Len(hits, 1)
}
