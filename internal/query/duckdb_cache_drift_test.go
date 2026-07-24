package query

import (
	"context"
	"database/sql"
	"encoding/json"
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
// The engine must detect out-of-band drift and refuse the cache instead of
// reading an uncommitted file set or falling back to SQLite.
func TestDuckDBEngine_CacheRebuiltUnderneath(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	// Pre-rebuild: current schema (message_type, sender_id, attachment_count present).
	const newMessagesCols = messagesCols
	// Post-rebuild: old schema written by a stale cache builder (no new columns).
	const oldMessagesCols = "id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments, deleted_from_source_at, year, month"

	pb := newParquetBuilder(t).
		addTable("messages", "messages/year=2024", "data.parquet", newMessagesCols, `
			(1::BIGINT, 1::BIGINT, 'm1', 100::BIGINT, 'Hello SOFRA', 'snip', TIMESTAMP '2024-01-15 10:00:00', 1000::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', false, 2024, 1)
		`).
		addTable("sources", "sources", "sources.parquet", sourcesCols, `(1::BIGINT, 'test@gmail.com', 'gmail')`).
		addTable("participants", "participants", "participants.parquet", participantsCols, `(1::BIGINT, 'alice@test.com', 'test.com', 'Alice', '')`).
		addTable("message_recipients", "message_recipients", "message_recipients.parquet", messageRecipientsCols, `(1::BIGINT, 1::BIGINT, 'from', 'Alice')`).
		addEmptyTable("labels", "labels", "labels.parquet", labelsCols, `(1::BIGINT, 'x')`).
		addEmptyTable("message_labels", "message_labels", "message_labels.parquet", messageLabelsCols, `(1::BIGINT, 1::BIGINT)`).
		addEmptyTable("attachments", "attachments", "attachments.parquet", attachmentsCols, `(1::BIGINT, 1::BIGINT, 100::BIGINT, 'x', '')`).
		addTable("conversations", "conversations", "conversations.parquet", conversationsCols, `(100::BIGINT, 'thread100', '', 'email')`)

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

	_, err = engine.SearchFast(ctx, search.Parse("SOFRA"), MessageFilter{}, 10, 0)
	require.ErrorIs(err, ErrCacheUnavailable)
	var unavailable *CacheUnavailableError
	require.ErrorAs(err, &unavailable)
	assert.Equal(CacheDrifted, unavailable.Readiness)
}

func TestDuckDBEngineRejectsInterruptedCache(t *testing.T) {
	require := require.New(t)
	analyticsDir, cleanup := buildStandardTestData(t).Build()
	t.Cleanup(cleanup)

	engine, err := NewDuckDBEngine(analyticsDir, "", nil)
	require.NoError(err, "open ready cache")
	t.Cleanup(func() { _ = engine.Close() })

	require.NoError(os.Remove(CacheStatePath(analyticsDir)), "interrupt cache")
	_, err = engine.Aggregate(context.Background(), ViewSenders, DefaultAggregateOptions())
	require.ErrorIs(err, ErrCacheUnavailable)

	second, err := NewDuckDBEngine(analyticsDir, "", nil)
	if second != nil {
		_ = second.Close()
	}
	require.ErrorIs(err, ErrCacheUnavailable)
}

func TestDuckDBEngine_SearchFastWithStatsRejectsMessageParquetDrift(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	pb := newParquetBuilder(t).
		addTable("messages", "messages/year=2024", "data.parquet", messagesCols, `
			(1::BIGINT, 1::BIGINT, 'm1', 100::BIGINT, 'Hello SOFRA', 'snip', TIMESTAMP '2024-01-15 10:00:00', 1000::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', false, 2024, 1)
		`).
		addTable("sources", "sources", "sources.parquet", sourcesCols, `(1::BIGINT, 'test@gmail.com', 'gmail')`).
		addTable("participants", "participants", "participants.parquet", participantsCols, `(1::BIGINT, 'alice@test.com', 'test.com', 'Alice', '')`).
		addTable("message_recipients", "message_recipients", "message_recipients.parquet", messageRecipientsCols, `(1::BIGINT, 1::BIGINT, 'from', 'Alice')`).
		addEmptyTable("labels", "labels", "labels.parquet", labelsCols, `(1::BIGINT, 'x')`).
		addEmptyTable("message_labels", "message_labels", "message_labels.parquet", messageLabelsCols, `(1::BIGINT, 1::BIGINT)`).
		addEmptyTable("attachments", "attachments", "attachments.parquet", attachmentsCols, `(1::BIGINT, 1::BIGINT, 100::BIGINT, 'x', '')`).
		addTable("conversations", "conversations", "conversations.parquet", conversationsCols, `(100::BIGINT, 'thread100', '', 'email')`)

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
		(1::BIGINT, 1::BIGINT, 'm1', 100::BIGINT, 'Hello SOFRA', 'snip', TIMESTAMP '2024-01-15 10:00:00', 1000::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', false, 2024, 1),
		(2::BIGINT, 1::BIGINT, 'm2', 101::BIGINT, 'Another SOFRA', 'snip', TIMESTAMP '2024-01-16 10:00:00', 2000::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', false, 2024, 1)
	`)

	_, err = engine.SearchFastWithStats(ctx, q, "SOFRA", MessageFilter{}, ViewSenders, 10, 0)
	require.ErrorIs(err, ErrCacheUnavailable)
	var unavailable *CacheUnavailableError
	require.ErrorAs(err, &unavailable)
	assert.Equal(CacheDrifted, unavailable.Readiness)
}

func TestDuckDBEngine_SearchFastWithStatsRejectsAttachmentParquetDrift(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	pb := newParquetBuilder(t).
		addTable("messages", "messages/year=2024", "data.parquet", messagesCols, `
			(1::BIGINT, 1::BIGINT, 'm1', 100::BIGINT, 'Hello SOFRA', 'snip', TIMESTAMP '2024-01-15 10:00:00', 1000::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, 'email', false, 2024, 1)
		`).
		addTable("sources", "sources", "sources.parquet", sourcesCols, `(1::BIGINT, 'test@gmail.com', 'gmail')`).
		addTable("participants", "participants", "participants.parquet", participantsCols, `(1::BIGINT, 'alice@test.com', 'test.com', 'Alice', '')`).
		addTable("message_recipients", "message_recipients", "message_recipients.parquet", messageRecipientsCols, `(1::BIGINT, 1::BIGINT, 'from', 'Alice')`).
		addEmptyTable("labels", "labels", "labels.parquet", labelsCols, `(1::BIGINT, 'x')`).
		addEmptyTable("message_labels", "message_labels", "message_labels.parquet", messageLabelsCols, `(1::BIGINT, 1::BIGINT)`).
		addEmptyTable("attachments", "attachments", "attachments.parquet", attachmentsCols, `(1::BIGINT, 1::BIGINT, 100::BIGINT, 'x', '')`).
		addTable("conversations", "conversations", "conversations.parquet", conversationsCols, `(100::BIGINT, 'thread100', '', 'email')`)

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
	rewriteParquetForTest(t, attPath, attachmentsCols, `(1::BIGINT, 1::BIGINT, 123::BIGINT, 'file.pdf', 'application/pdf')`)

	_, err = engine.SearchFastWithStats(ctx, q, "SOFRA", MessageFilter{}, ViewSenders, 10, 0)
	require.ErrorIs(err, ErrCacheUnavailable)
	var unavailable *CacheUnavailableError
	require.ErrorAs(err, &unavailable)
	assert.Equal(CacheDrifted, unavailable.Readiness)
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

// TestDuckDBEngine_AnalyticalEndpointsFollowCommittedCacheSchemaSwap is the
// regression test for view-based analytical endpoints (Explore, Relationships,
// People, timelines) retaining stale view definitions after a live cache
// publication. Unlike the drift tests above, the swap here is COMMITTED — the
// dataset fingerprint and watermarks are republished, so the ready-cache read
// lock admits the query — and the same engine instance must re-register its
// Parquet views against the new schema instead of failing with
// "Column ... in REPLACE list not found in FROM clause".
func TestDuckDBEngine_AnalyticalEndpointsFollowCommittedCacheSchemaSwap(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	b := NewTestDataBuilder(t)
	sourceID := b.AddSource("me@test.com")
	me := b.AddParticipant("me@test.com", "test.com", "Me")
	bob := b.AddParticipant("bob@example.com", "example.com", "Bob")
	received := b.AddMessage(MessageOpt{Subject: "Hello from Bob", SentAt: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)})
	b.AddFrom(received, bob, "Bob")
	b.AddTo(received, me, "Me")
	sent := b.AddMessage(MessageOpt{Subject: "Reply to Bob", SentAt: time.Date(2024, 1, 16, 10, 0, 0, 0, time.UTC), IsFromMe: true})
	b.AddFrom(sent, me, "Me")
	b.AddTo(sent, bob, "Bob")
	b.AddOwnerParticipant(sourceID, me)

	analyticsDir, cleanup := b.Build()
	t.Cleanup(cleanup)

	engine, err := NewDuckDBEngine(analyticsDir, "", nil)
	require.NoError(err, "NewDuckDBEngine")
	t.Cleanup(func() { _ = engine.Close() })

	ctx := context.Background()
	now := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)

	exploreBefore, err := engine.Explore(ctx, ExploreRequest{})
	require.NoError(err, "Explore before swap")
	require.Len(exploreBefore.Rows, 2)
	relBefore, err := engine.Relationships(ctx, RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
	require.NoError(err, "Relationships before swap")
	require.Len(relBefore.Rows, 1)
	require.True(engine.hasCol(datasetMessages, "message_type"),
		"message_type should be detected as present in the initial schema")

	// A stale cache builder republishes the messages Parquet with the OLD
	// schema (no attachment_count/sender_id/message_type/is_from_me) and
	// commits the swap: new dataset fingerprint, advanced watermarks.
	const legacyMessagesCols = "id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments, deleted_from_source_at, year, month"
	msgPath := filepath.Join(analyticsDir, "messages", "year=2024", "data.parquet")
	rewriteParquetForTest(t, msgPath, legacyMessagesCols, `
		(1::BIGINT, 1::BIGINT, 'msg1', 200::BIGINT, 'Hello from Bob', 'Preview 1', TIMESTAMP '2024-01-15 10:00:00', 0::BIGINT, false, NULL::TIMESTAMP, 2024, 1),
		(2::BIGINT, 1::BIGINT, 'msg2', 201::BIGINT, 'Reply to Bob', 'Preview 2', TIMESTAMP '2024-01-16 10:00:00', 0::BIGINT, false, NULL::TIMESTAMP, 2024, 1)
	`)
	republishCacheStateForTest(t, analyticsDir)

	exploreAfter, err := engine.Explore(ctx, ExploreRequest{})
	require.NoError(err, "Explore after committed schema swap must re-register views, not bind stale REPLACE lists")
	require.Len(exploreAfter.Rows, 2)
	assert.Equal(int64(2), exploreAfter.TotalCount)
	for _, row := range exploreAfter.Rows {
		assert.Equal(EntryEmail, row.Kind, "synthesized empty message_type classifies as email")
	}
	assert.NotEqual(exploreBefore.CacheRevision, exploreAfter.CacheRevision,
		"republication must produce a new committed cache revision")
	assert.False(engine.hasCol(datasetMessages, "message_type"),
		"optional-column probe must reflect the swapped-in schema")

	queryRunsBefore := engine.relationshipsQueryRuns.Load()
	relAfter, err := engine.Relationships(ctx, RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
	require.NoError(err, "Relationships after committed schema swap")
	require.Len(relAfter.Rows, 1)
	assert.Equal("Bob", relAfter.Rows[0].DisplayLabel)
	assert.Equal(exploreAfter.CacheRevision, relAfter.CacheRevision)
	assert.Equal(queryRunsBefore+1, engine.relationshipsQueryRuns.Load(),
		"new cache revision must miss the relationships memo and recompute")
}

// TestDuckDBEngine_QueryPathMemoizesFullFingerprintWalks is the performance
// contract for per-query readiness validation: repeated queries against an
// unchanged committed cache perform exactly one full dataset fingerprint walk
// (at engine startup); a commit-marker republication triggers exactly one
// revalidation; and an out-of-band shard mutation without a marker change
// still forces a full inspection on the next query and is rejected as drift.
func TestDuckDBEngine_QueryPathMemoizesFullFingerprintWalks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	walks := 0
	original := inspectDatasetFingerprint
	inspectDatasetFingerprint = func(analyticsDir string) (string, error) {
		walks++
		return CacheDatasetFingerprint(analyticsDir)
	}
	t.Cleanup(func() { inspectDatasetFingerprint = original })

	analyticsDir, cleanup := buildStandardTestData(t).Build()
	t.Cleanup(cleanup)

	engine, err := NewDuckDBEngine(analyticsDir, "", nil)
	require.NoError(err, "NewDuckDBEngine")
	t.Cleanup(func() { _ = engine.Close() })
	require.Equal(1, walks, "startup performs the single full fingerprint walk")

	ctx := context.Background()
	for range 5 {
		_, err := engine.Aggregate(ctx, ViewSenders, DefaultAggregateOptions())
		require.NoError(err, "Aggregate against unchanged cache")
	}
	assert.Equal(1, walks, "queries against an unchanged cache must not re-walk the dataset")

	// A committed republication rewrites the marker: the next query must run
	// exactly one full revalidation, then memoize again.
	republishCacheStateForTest(t, analyticsDir)
	_, err = engine.Aggregate(ctx, ViewSenders, DefaultAggregateOptions())
	require.NoError(err, "Aggregate after committed republication")
	assert.Equal(2, walks, "marker change triggers exactly one revalidation")
	_, err = engine.Aggregate(ctx, ViewSenders, DefaultAggregateOptions())
	require.NoError(err, "Aggregate after revalidation")
	assert.Equal(2, walks, "revalidated cache memoizes again")

	// An out-of-band shard mutation without a marker change alters the stat
	// signature, forcing a full inspection that classifies the cache as
	// drifted.
	touchParquetForTest(t, firstRequiredParquetForTest(t, analyticsDir, datasetMessages))
	_, err = engine.Aggregate(ctx, ViewSenders, DefaultAggregateOptions())
	require.ErrorIs(err, ErrCacheUnavailable)
	var unavailable *CacheUnavailableError
	require.ErrorAs(err, &unavailable)
	assert.Equal(CacheDrifted, unavailable.Readiness)
	assert.Equal(3, walks, "out-of-band shard change forces a full inspection")
}

// republishCacheStateForTest recommits the cache state after an out-of-band
// Parquet rewrite, simulating an atomic live cache publication as build-cache
// performs it: the dataset fingerprint matches the new files and the
// publication watermarks advance so the committed revision changes.
func republishCacheStateForTest(t *testing.T, analyticsDir string) {
	t.Helper()
	state, err := ReadCacheSyncState(analyticsDir)
	require.NoError(t, err, "read cache state")
	fingerprint, err := CacheDatasetFingerprint(analyticsDir)
	require.NoError(t, err, "fingerprint swapped cache")
	state.DatasetFingerprint = fingerprint
	state.LastSyncAt = state.LastSyncAt.Add(time.Minute)
	state.PublishedAt = state.PublishedAt.Add(time.Minute)
	data, err := json.Marshal(state)
	require.NoError(t, err, "marshal cache state")
	require.NoError(t, os.WriteFile(CacheStatePath(analyticsDir), data, 0o600), "write cache state")
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
