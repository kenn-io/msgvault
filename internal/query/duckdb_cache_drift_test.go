package query

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

// TestDuckDBEngine_CacheRebuiltUnderneath reproduces the production crash where
// a long-running engine (the mcp-http server) probed the Parquet schema once at
// startup, then build-cache/sync rewrote the cache with a different column set.
// The stale "message_type present" verdict put the column into a SELECT *
// REPLACE list that the new Parquet lacked, yielding:
//
//	Binder Error: Column "message_type" in REPLACE list not found in FROM clause
//
// The engine must detect the cache change and re-probe instead of crashing.
func TestDuckDBEngine_CacheRebuiltUnderneath(t *testing.T) {
	// Pre-rebuild: current schema (message_type, sender_id, attachment_count present).
	const newMessagesCols = messagesCols
	// Post-rebuild: old schema written by a stale cache builder (no new columns).
	const oldMessagesCols = "id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments, deleted_from_source_at, year, month"

	pb := newParquetBuilder(t).
		addTable("messages", "messages/year=2024", "data.parquet", newMessagesCols, `
			(1::BIGINT, 1::BIGINT, 'm1', 100::BIGINT, 'Hello SOFRA', 'snip', TIMESTAMP '2024-01-15 10:00:00', 1000::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', 2024, 1)
		`).
		addTable("sources", "sources", "sources.parquet", sourcesCols, `(1::BIGINT, 'test@gmail.com', 'gmail')`).
		addTable("participants", "participants", "participants.parquet", participantsCols, `(1::BIGINT, 'alice@test.com', 'test.com', 'Alice', '')`).
		addTable("message_recipients", "message_recipients", "message_recipients.parquet", messageRecipientsCols, `(1::BIGINT, 1::BIGINT, 'from', 'Alice')`).
		addEmptyTable("labels", "labels", "labels.parquet", labelsCols, `(1::BIGINT, 'x')`).
		addEmptyTable("message_labels", "message_labels", "message_labels.parquet", messageLabelsCols, `(1::BIGINT, 1::BIGINT)`).
		addEmptyTable("attachments", "attachments", "attachments.parquet", attachmentsCols, `(1::BIGINT, 100::BIGINT, 'x')`).
		addTable("conversations", "conversations", "conversations.parquet", conversationsCols, `(100::BIGINT, 'thread100', '')`)

	analyticsDir, cleanup := pb.build()
	t.Cleanup(cleanup)

	engine, err := NewDuckDBEngine(analyticsDir, "", nil)
	require.NoError(t, err, "NewDuckDBEngine")
	t.Cleanup(func() { _ = engine.Close() })

	ctx := context.Background()

	// Startup probe sees the new schema.
	require.True(t, engine.hasCol("messages", "message_type"),
		"message_type should be detected as present in the initial schema")
	res, err := engine.SearchFast(ctx, search.Parse("SOFRA"), MessageFilter{}, 10, 0)
	require.NoError(t, err, "SearchFast before rebuild")
	require.Len(t, res, 1)

	// build-cache rewrites the messages Parquet with the OLD schema underneath
	// the running engine — message_type/sender_id/attachment_count disappear.
	msgPath := filepath.Join(analyticsDir, "messages", "year=2024", "data.parquet")
	rewriteParquetForTest(t, msgPath, oldMessagesCols, `
		(1::BIGINT, 1::BIGINT, 'm1', 100::BIGINT, 'Hello SOFRA', 'snip', TIMESTAMP '2024-01-15 10:00:00', 1000::BIGINT, false, NULL::TIMESTAMP, 2024, 1)
	`)

	// Must re-probe and succeed rather than fail with the REPLACE binder error.
	res, err = engine.SearchFast(ctx, search.Parse("SOFRA"), MessageFilter{}, 10, 0)
	require.NoError(t, err, "SearchFast after cache rebuilt underneath engine")
	require.Len(t, res, 1)
	assert.Equal(t, "Hello SOFRA", res[0].Subject)
	assert.False(t, engine.hasCol("messages", "message_type"),
		"message_type should be re-probed as absent after the rebuild")

	// Aggregate (the other reported-broken path) must also recover.
	agg, err := engine.Aggregate(ctx, ViewSenders, DefaultAggregateOptions())
	require.NoError(t, err, "Aggregate after cache rebuilt underneath engine")
	require.Len(t, agg, 1)
}

// rewriteParquetForTest overwrites an existing Parquet file with a new schema
// and rows, simulating an out-of-band cache rebuild.
func rewriteParquetForTest(t *testing.T, path, columns, values string) {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err, "open duckdb")
	defer func() { _ = db.Close() }()
	writeTableParquet(t, db, escapePath(path), columns, values, false)
}
