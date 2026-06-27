package query

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	require := require.New(t)
	assert := assert.New(t)

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
	require.NoError(err, "NewDuckDBEngine")
	t.Cleanup(func() { _ = engine.Close() })

	ctx := context.Background()

	// Startup probe sees the new schema.
	require.True(engine.hasCol("messages", "message_type"),
		"message_type should be detected as present in the initial schema")
	res, err := engine.SearchFast(ctx, search.Parse("SOFRA"), MessageFilter{}, 10, 0)
	require.NoError(err, "SearchFast before rebuild")
	require.Len(res, 1)

	// build-cache rewrites the messages Parquet with the OLD schema underneath
	// the running engine — message_type/sender_id/attachment_count disappear.
	msgPath := filepath.Join(analyticsDir, "messages", "year=2024", "data.parquet")
	rewriteParquetForTest(t, msgPath, oldMessagesCols, `
		(1::BIGINT, 1::BIGINT, 'm1', 100::BIGINT, 'Hello SOFRA', 'snip', TIMESTAMP '2024-01-15 10:00:00', 1000::BIGINT, false, NULL::TIMESTAMP, 2024, 1)
	`)

	// Must re-probe and succeed rather than fail with the REPLACE binder error.
	res, err = engine.SearchFast(ctx, search.Parse("SOFRA"), MessageFilter{}, 10, 0)
	require.NoError(err, "SearchFast after cache rebuilt underneath engine")
	require.Len(res, 1)
	assert.Equal("Hello SOFRA", res[0].Subject)
	assert.False(engine.hasCol("messages", "message_type"),
		"message_type should be re-probed as absent after the rebuild")

	// Aggregate (the other reported-broken path) must also recover.
	agg, err := engine.Aggregate(ctx, ViewSenders, DefaultAggregateOptions())
	require.NoError(err, "Aggregate after cache rebuilt underneath engine")
	require.Len(agg, 1)
}

func TestDuckDBEngine_SearchFastWithStatsRebuildsCacheWhenParquetChanges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	pb := newParquetBuilder(t).
		addTable("messages", "messages/year=2024", "data.parquet", messagesCols, `
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
	require.NoError(err, "NewDuckDBEngine")
	t.Cleanup(func() { _ = engine.Close() })

	ctx := context.Background()
	q := search.Parse("SOFRA")

	first, err := engine.SearchFastWithStats(ctx, q, "SOFRA", MessageFilter{}, ViewSenders, 10, 0)
	require.NoError(err, "SearchFastWithStats before rebuild")
	require.Len(first.Messages, 1)
	require.Equal(int64(1), first.TotalCount)
	require.NotNil(first.Stats)
	require.Equal(int64(1), first.Stats.MessageCount)

	msgPath := filepath.Join(analyticsDir, "messages", "year=2024", "data.parquet")
	rewriteParquetForTest(t, msgPath, messagesCols, `
		(1::BIGINT, 1::BIGINT, 'm1', 100::BIGINT, 'Hello SOFRA', 'snip', TIMESTAMP '2024-01-15 10:00:00', 1000::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', 2024, 1),
		(2::BIGINT, 1::BIGINT, 'm2', 101::BIGINT, 'Another SOFRA', 'snip', TIMESTAMP '2024-01-16 10:00:00', 2000::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', 2024, 1)
	`)

	second, err := engine.SearchFastWithStats(ctx, q, "SOFRA", MessageFilter{}, ViewSenders, 10, 0)
	require.NoError(err, "SearchFastWithStats after rebuild")
	require.Len(second.Messages, 2)
	assert.Equal(int64(2), second.TotalCount)
	require.NotNil(second.Stats)
	assert.Equal(int64(2), second.Stats.MessageCount)
	assert.Equal("Another SOFRA", second.Messages[0].Subject)
	assert.Equal("Hello SOFRA", second.Messages[1].Subject)
}

func TestDuckDBEngine_SearchFastWithStatsRebuildsStatsWhenAttachmentsChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	pb := newParquetBuilder(t).
		addTable("messages", "messages/year=2024", "data.parquet", messagesCols, `
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
	require.NoError(err, "NewDuckDBEngine")
	t.Cleanup(func() { _ = engine.Close() })

	ctx := context.Background()
	q := search.Parse("SOFRA")

	first, err := engine.SearchFastWithStats(ctx, q, "SOFRA", MessageFilter{}, ViewSenders, 10, 0)
	require.NoError(err, "SearchFastWithStats before attachments rebuild")
	require.NotNil(first.Stats)
	require.Len(first.Messages, 1)
	require.Equal(int64(0), first.Stats.AttachmentCount)
	require.Equal(0, first.Messages[0].AttachmentCount)

	attPath := filepath.Join(analyticsDir, "attachments", "attachments.parquet")
	rewriteParquetForTest(t, attPath, attachmentsCols, `(1::BIGINT, 123::BIGINT, 'file.pdf')`)

	second, err := engine.SearchFastWithStats(ctx, q, "SOFRA", MessageFilter{}, ViewSenders, 10, 0)
	require.NoError(err, "SearchFastWithStats after attachments rebuild")
	require.NotNil(second.Stats)
	require.Len(second.Messages, 1)
	assert.Equal(int64(1), second.Stats.AttachmentCount)
	assert.Equal(int64(123), second.Stats.AttachmentSize)
	assert.Equal(1, second.Messages[0].AttachmentCount)
}

func TestDuckDBEngine_CacheFingerprintCoversRequiredParquetDirs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	analyticsDir, cleanup := buildStandardTestData(t).Build()
	t.Cleanup(cleanup)

	engine, err := NewDuckDBEngine(analyticsDir, "", nil)
	require.NoError(err, "NewDuckDBEngine")
	t.Cleanup(func() { _ = engine.Close() })

	for _, dir := range RequiredParquetDirs {
		t.Run(dir, func(t *testing.T) {
			before := engine.cacheFingerprint()
			touchParquetForTest(t, firstRequiredParquetForTest(t, analyticsDir, dir))
			after := engine.cacheFingerprint()
			assert.NotEqual(before, after, "fingerprint should include %s", dir)
		})
	}
}

func TestStableOptionalColumnsRetriesWhenFingerprintChanges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	staleCols := map[string]map[string]bool{datasetMessages: map[string]bool{"message_type": true}}
	freshCols := map[string]map[string]bool{datasetMessages: map[string]bool{"message_type": false}}
	fingerprints := []string{"before", "after", "after", "after"}
	probeCalls := 0

	cols, fp := stableOptionalColumns(func() string {
		require.NotEmpty(fingerprints, "unexpected fingerprint call")
		fp := fingerprints[0]
		fingerprints = fingerprints[1:]
		return fp
	}, func() map[string]map[string]bool {
		probeCalls++
		if probeCalls == 1 {
			return staleCols
		}
		return freshCols
	})

	assert.Equal(2, probeCalls)
	assert.Equal(freshCols, cols)
	assert.Equal("after", fp)
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

func firstRequiredParquetForTest(t *testing.T, analyticsDir, dir string) string {
	t.Helper()
	patterns := []string{filepath.Join(analyticsDir, dir, "*.parquet")}
	if dir == datasetMessages {
		patterns = append([]string{filepath.Join(analyticsDir, dir, "*", "*.parquet")}, patterns...)
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		require.NoError(t, err, "glob parquet files")
		if len(matches) > 0 {
			return matches[0]
		}
	}
	require.FailNow(t, "required parquet file not found", "dir %s", dir)
	return ""
}

func touchParquetForTest(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err, "stat parquet file")
	modTime := info.ModTime().Add(time.Second)
	require.NoError(t, os.Chtimes(path, modTime, modTime), "touch parquet file")
}
