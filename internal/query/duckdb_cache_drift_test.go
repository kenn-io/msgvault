package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
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
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	// Pre-rebuild: current schema (message_type, sender_id, attachment_count present).
	const newMessagesCols = messagesCols
	// Post-rebuild: old schema written by a stale cache builder (no new columns).
	const oldMessagesCols = "id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments, deleted_from_source_at, year, month"

	pb := newParquetBuilder(t).
		addTable("messages", "messages/year=2024", "data.parquet", newMessagesCols, `
			(1::BIGINT, 1::BIGINT, 'm1', 100::BIGINT, 'Hello SOFRA', 'snip', TIMESTAMP '2024-01-15 10:00:00', 1000::BIGINT, false, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, NULL::BIGINT, 'email', NULL::VARCHAR, false, 2024, 1)
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
	t.Parallel()
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

func TestDuckDBEngine_LiveSQLWhenCacheUnavailable(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%t", enabled), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			b := NewTestDataBuilder(t)
			b.AddSource("archive@example.com")
			sender := b.AddParticipant("old@example.com", "example.com", "Old")
			message := b.AddMessage(MessageOpt{Subject: "old message"})
			b.AddFrom(message, sender, "Old")
			analyticsDir, cleanup := b.Build()
			t.Cleanup(cleanup)
			db := dbtest.NewTestDB(t, "../store/schema.sql")
			_, err := db.DB.Exec(`
				INSERT INTO sources (id, source_type, identifier) VALUES (7, 'gmail', 'live@example.com');
				INSERT INTO participants (id, email_address, domain, display_name) VALUES (11, 'live@example.com', 'example.com', 'Live');
				INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES (701, 7, 'live-thread', 'email', 'Live');
				INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, subject, snippet, sender_id)
				VALUES (801, 701, 7, 'live-message', 'email', '2024-01-15 10:00:00', 'live subject', 'live snippet', 11);
				INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (801, 11, 'from');
				INSERT INTO sources (id, source_type, identifier) VALUES (8, 'whatsapp', 'chat-a'), (9, 'whatsapp', 'chat-b');
				INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title)
				VALUES (702, 8, 'live-chat', 'direct_chat', 'Live chat'), (703, 9, 'other-chat', 'direct_chat', 'Other chat');
				INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
				VALUES (802, 702, 8, 'live-text', 'whatsapp', '2024-01-15 10:00:00', 'live text', 11),
				       (803, 703, 9, 'other-text', 'whatsapp', '2024-01-15 10:00:00', 'other text', 11);
			`)
			requirements.NoError(err)
			engine, err := NewDuckDBEngine(analyticsDir, "", db.DB, DuckDBOptions{
				DisableSQLiteScanner: true, LiveSQLWhenCacheUnavailable: enabled,
			})
			requirements.NoError(err)
			t.Cleanup(func() { requirements.NoError(engine.Close()) })
			rows, err := engine.Aggregate(t.Context(), ViewSenders, DefaultAggregateOptions())
			requirements.NoError(err)
			requirements.Len(rows, 1)
			assertions.Equal("old@example.com", rows[0].Key)
			requirements.NoError(os.Remove(CacheStatePath(analyticsDir)))
			sourceID := int64(7)
			filter := MessageFilter{SourceID: &sourceID}
			textSourceID := int64(8)
			textFilter := TextFilter{SourceID: &textSourceID, Pagination: Pagination{Limit: 1}}
			for _, method := range []string{
				"Aggregate", "SubAggregate", "SubAggregateSourceOverride", "GetTotalStats", "ListMessages", "SearchFastWithStats",
				"ListConversationsSnapshot", "TextAggregate", "GetTextStats",
				"SearchFast", "SearchFastCount",
			} {
				t.Run(method, func(t *testing.T) {
					require := require.New(t)
					assert := assert.New(t)
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					switch method {
					case "Aggregate", "SubAggregate", "SubAggregateSourceOverride":
						var rows []AggregateRow
						var err error
						switch method {
						case "Aggregate":
							rows, err = engine.Aggregate(ctx, ViewSenders, DefaultAggregateOptions())
						case "SubAggregateSourceOverride":
							opts := DefaultAggregateOptions()
							opts.SourceID = &sourceID
							otherSourceID := int64(9)
							rows, err = engine.SubAggregate(ctx, MessageFilter{SourceID: &otherSourceID}, ViewSenders, opts)
						default:
							rows, err = engine.SubAggregate(ctx, filter, ViewSenders, DefaultAggregateOptions())
						}
						if !enabled {
							require.ErrorIs(err, ErrCacheUnavailable)
							return
						}
						require.NoError(err)
						require.Len(rows, 1)
						assert.Equal("live@example.com", rows[0].Key)
					case "GetTotalStats":
						stats, err := engine.GetTotalStats(ctx, StatsOptions{})
						if !enabled {
							require.ErrorIs(err, ErrCacheUnavailable)
							return
						}
						require.NoError(err)
						assert.Equal(int64(1), stats.MessageCount)
					case "ListMessages":
						messages, err := engine.ListMessages(ctx, filter)
						if !enabled {
							require.ErrorIs(err, ErrCacheUnavailable)
							return
						}
						require.NoError(err)
						require.Len(messages, 1)
						assert.Equal("live-message", messages[0].SourceMessageID)
					case "SearchFast":
						messages, err := engine.SearchFast(ctx, search.Parse("subject:live"), filter, 10, 0)
						if !enabled {
							require.ErrorIs(err, ErrCacheUnavailable)
							return
						}
						require.NoError(err)
						require.Len(messages, 1)
						assert.Equal("live-message", messages[0].SourceMessageID)
					case "SearchFastCount":
						count, err := engine.SearchFastCount(ctx, search.Parse("subject:live"), filter)
						if !enabled {
							require.ErrorIs(err, ErrCacheUnavailable)
							return
						}
						require.NoError(err)
						assert.Equal(int64(1), count)
					case "SearchFastWithStats":
						result, err := engine.SearchFastWithStats(ctx, search.Parse("live"), "live", filter, ViewSenders, 10, 0)
						if !enabled {
							require.ErrorIs(err, ErrCacheUnavailable)
							return
						}
						require.NoError(err)
						require.Len(result.Messages, 1)
						assert.Equal("live-message", result.Messages[0].SourceMessageID)
						assert.Equal(int64(1), result.TotalCount)
						require.NotNil(result.Stats)
						assert.Equal(int64(1), result.Stats.MessageCount)
					case "ListConversationsSnapshot":
						rows, revision, err := engine.ListConversationsSnapshot(ctx, textFilter)
						if !enabled {
							require.ErrorIs(err, ErrCacheUnavailable)
							return
						}
						require.NoError(err)
						require.Len(rows, 1)
						assert.Equal(int64(702), rows[0].ConversationID)
						assert.Equal("Live chat", rows[0].Title)
						want, err := engine.sqliteEngine.TextSnapshotRevision(ctx, TextSnapshotScope{Filter: textFilter})
						require.NoError(err)
						assert.NotEmpty(revision)
						assert.Equal(want, revision)
					case "TextAggregate":
						rows, err := engine.TextAggregate(ctx, TextViewContactNames, TextAggregateOptions{SourceID: &textSourceID})
						if !enabled {
							require.ErrorIs(err, ErrCacheUnavailable)
							return
						}
						require.NoError(err)
						require.Len(rows, 1)
						assert.Equal("Live", rows[0].Key)
						assert.Equal(int64(1), rows[0].Count)
					case "GetTextStats":
						stats, err := engine.GetTextStats(ctx, TextStatsOptions{SourceID: &textSourceID})
						if !enabled {
							require.ErrorIs(err, ErrCacheUnavailable)
							return
						}
						require.NoError(err)
						assert.Equal(int64(1), stats.MessageCount)
					}
				})
			}
		})
	}
}

func TestDuckDBEngine_SearchFastWithStatsRejectsMessageParquetDrift(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	pb := newParquetBuilder(t).
		addTable("messages", "messages/year=2024", "data.parquet", messagesCols, `
			(1::BIGINT, 1::BIGINT, 'm1', 100::BIGINT, 'Hello SOFRA', 'snip', TIMESTAMP '2024-01-15 10:00:00', 1000::BIGINT, false, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, NULL::BIGINT, 'email', NULL::VARCHAR, false, 2024, 1)
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
		(1::BIGINT, 1::BIGINT, 'm1', 100::BIGINT, 'Hello SOFRA', 'snip', TIMESTAMP '2024-01-15 10:00:00', 1000::BIGINT, false, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, NULL::BIGINT, 'email', NULL::VARCHAR, false, 2024, 1),
		(2::BIGINT, 1::BIGINT, 'm2', 101::BIGINT, 'Another SOFRA', 'snip', TIMESTAMP '2024-01-16 10:00:00', 2000::BIGINT, false, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, NULL::BIGINT, 'email', NULL::VARCHAR, false, 2024, 1)
	`)

	_, err = engine.SearchFastWithStats(ctx, q, "SOFRA", MessageFilter{}, ViewSenders, 10, 0)
	require.ErrorIs(err, ErrCacheUnavailable)
	var unavailable *CacheUnavailableError
	require.ErrorAs(err, &unavailable)
	assert.Equal(CacheDrifted, unavailable.Readiness)
}

func TestDuckDBEngine_SearchFastWithStatsRejectsAttachmentParquetDrift(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	pb := newParquetBuilder(t).
		addTable("messages", "messages/year=2024", "data.parquet", messagesCols, `
			(1::BIGINT, 1::BIGINT, 'm1', 100::BIGINT, 'Hello SOFRA', 'snip', TIMESTAMP '2024-01-15 10:00:00', 1000::BIGINT, false, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, NULL::BIGINT, 'email', NULL::VARCHAR, false, 2024, 1)
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
	t.Parallel()
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
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	staleCols := map[string]map[string]bool{datasetMessages: map[string]bool{"message_type": true}}
	freshCols := map[string]map[string]bool{datasetMessages: map[string]bool{"message_type": false}}
	fingerprints := []string{"before", "after", "after", "after"}
	probeCalls := 0

	cols, fp, err := stableOptionalColumns(t.Context(), func() string {
		require.NotEmpty(fingerprints, "unexpected fingerprint call")
		fp := fingerprints[0]
		fingerprints = fingerprints[1:]
		return fp
	}, func() (map[string]map[string]bool, error) {
		probeCalls++
		if probeCalls == 1 {
			return staleCols, nil
		}
		return freshCols, nil
	})

	require.NoError(err)
	assert.Equal(2, probeCalls)
	assert.Equal(freshCols, cols)
	assert.Equal("after", fp)
}

func bobExchangeFixture(t *testing.T) (*TestDataBuilder, int64) {
	t.Helper()
	b := NewTestDataBuilder(t)
	sourceID := b.AddSource("me@example.test")
	me := b.AddParticipant("me@example.test", "example.test", "Me")
	bob := b.AddParticipant("bob@example.com", "example.com", "Bob")
	received := b.AddMessage(MessageOpt{Subject: "Hello from Bob", SentAt: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)})
	b.AddFrom(received, bob, "Bob")
	b.AddTo(received, me, "Me")
	sent := b.AddMessage(MessageOpt{Subject: "Reply to Bob", SentAt: time.Date(2024, 1, 16, 10, 0, 0, 0, time.UTC), IsFromMe: true})
	b.AddFrom(sent, me, "Me")
	b.AddTo(sent, bob, "Bob")
	b.AddOwnerParticipant(sourceID, me)

	return b, sent
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
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	b, _ := bobExchangeFixture(t)

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

	relAfter, err := engine.Relationships(ctx, RelationshipsRequest{Now: now, Limit: 10, ShowAll: true})
	require.NoError(err, "Relationships after committed schema swap")
	require.Len(relAfter.Rows, 1)
	assert.Equal("Bob", relAfter.Rows[0].DisplayLabel)
	assert.Equal(exploreAfter.CacheRevision, relAfter.CacheRevision)
	assert.NotEqual(relBefore.CacheRevision, relAfter.CacheRevision,
		"relationship reads must report the newly committed revision")
}

func TestDuckDBEngine_ReloadsUnsentIDsAfterPublicationAndRetriesFailedRefresh(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"reloads after sent publication", "retries after unreadable activity is restored"} {
		t.Run(outcome, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			b, sent := bobExchangeFixture(t)
			b.unsentMessages = fmt.Sprintf("(SELECT %d::BIGINT AS message_id)", sent)
			root, cleanup := b.Build()
			t.Cleanup(cleanup)
			engine, err := NewDuckDBEngine(root, "", nil)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(engine.Close()) })
			_, err = engine.db.Exec("SET enable_external_file_cache = false")
			require.NoError(err)
			after := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
			request := PersonSearchRequest{Explore: ExploreRequest{Context: Context{After: &after}}, Query: "Bob"}
			people, err := engine.SearchPeople(t.Context(), request)
			require.NoError(err)
			require.Len(people.Rows, 1)
			assert.Equal(int64(1), people.Rows[0].ActivityCount)
			assert.Equal(time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC), people.Rows[0].LastAt)

			wantCount := int64(2)
			wantLast := time.Date(2024, 1, 16, 10, 0, 0, 0, time.UTC)
			if outcome == "reloads after sent publication" {
				_, err = identityindex.Build(t.Context(), engine.db, identityindex.BuildOptions{
					Mode: identityindex.ModeFull, StagedBaseRoot: root, OutputRoot: root,
				})
				require.NoError(err)
				republishCacheStateForTest(t, root)
			} else {
				activity := firstRequiredParquetForTest(t, root, identityindex.DatasetActivity)
				original, err := os.ReadFile(activity)
				require.NoError(err)
				unreadable := append([]byte(nil), original...)
				copy(unreadable[len(unreadable)-4:], "FAIL")
				require.NoError(os.WriteFile(activity, unreadable, 0o600))
				info, err := os.Stat(activity)
				require.NoError(err)
				republishCacheStateForTest(t, root)
				messages, err := engine.ListMessages(t.Context(), MessageFilter{})
				require.NoError(err, "base messages remain available when contact refresh fails")
				assert.Len(messages, 2)
				_, err = engine.SearchPeople(t.Context(), request)
				require.Error(err, "contact reads must reject stale exclusions")
				require.ErrorContains(err, "load unsent message IDs", "surface the refresh failure instead of querying a missing exclusion table")
				_, err = engine.ExploreSelectionStats(t.Context(), ExploreSelectionRequest{Explore: ExploreRequest{SentOnly: true}})
				require.ErrorContains(err, "load unsent message IDs", "SentOnly analysis must reject the prior exclusion set")
				_, err = engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{CanonicalID: 2, Year: 2024})
				require.ErrorContains(err, "load unsent message IDs", "calendar must reject stale owners")
				_, err = engine.RelationshipTimeline(t.Context(), RelationshipTimelineRequest{CanonicalID: 2})
				require.ErrorContains(err, "load unsent message IDs", "timeline must reject stale owners")
				require.NoError(os.WriteFile(activity, original, 0o600))
				// Restore the bytes without a new marker or shard-stat change. A failed
				// contact refresh must retry even when the committed signature is unchanged.
				require.NoError(os.Chtimes(activity, info.ModTime(), info.ModTime()))
				wantCount = 1
				wantLast = time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
			}
			people, err = engine.SearchPeople(t.Context(), request)
			require.NoError(err, "contact reads recover on retry")
			require.Len(people.Rows, 1)
			assert.Equal(wantCount, people.Rows[0].ActivityCount)
			assert.Equal(wantLast, people.Rows[0].LastAt)
			messages, err := engine.ListMessages(t.Context(), MessageFilter{})
			require.NoError(err)
			assert.Len(messages, 2)
		})
	}
}

// TestDuckDBEngine_QueryPathMemoizesFullFingerprintWalks is the performance
// contract for per-query readiness validation: repeated queries against an
// unchanged committed cache perform exactly one full dataset fingerprint walk
// (at engine startup); a commit-marker republication triggers exactly one
// revalidation; and an out-of-band shard mutation without a marker change
// still forces a full inspection on the next query and is rejected as drift.
func TestDuckDBEngine_QueryPathMemoizesFullFingerprintWalks(t *testing.T) {
	// serial: swaps the package global inspectDatasetFingerprint.
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
	var match string
	err := filepath.WalkDir(filepath.Join(analyticsDir, dir),
		func(path string, entry os.DirEntry, walkErr error) error {
			require.NoError(t, walkErr)
			if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".parquet") {
				match = path
				return filepath.SkipAll
			}
			return nil
		},
	)
	require.NoError(t, err, "walk parquet files")
	if match != "" {
		return match
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
