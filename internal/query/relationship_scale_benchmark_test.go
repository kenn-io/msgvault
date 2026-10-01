package query

import (
	"encoding/json/v2"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/duckdbutil"
	"go.kenn.io/msgvault/internal/identityindex"
)

var relationshipScaleRoot = flag.String("relationship-bench-root", "", "Scratch cache directory for relationship scale benchmarks")
var relationshipScaleProfile = flag.Bool("relationship-bench-profile", false, "Write DuckDB JSON profiles for explore scale queries into the scratch cache directory")
var relationshipScaleMemory = flag.String("relationship-bench-memory", "", "Override the interactive DuckDB memory limit for explore scale queries")

// Run Build once, then Queries against the same scratch directory:
//
//	relationship_bench_root=$(mktemp -d)
//	go test -tags 'fts5 sqlite_vec' ./internal/query -run '^$' -bench '^BenchmarkRelationshipScaleBuild$' -benchtime=1x -args -relationship-bench-root="$relationship_bench_root"
//	go test -tags 'fts5 sqlite_vec' ./internal/query -run '^$' -bench '^BenchmarkRelationshipScaleQueries$' -benchtime=4x -args -relationship-bench-root="$relationship_bench_root"
//
// The fixture has 2,000,000 email messages across 303 contacts, plus 2,000
// messages in one 304-member chat. Half the email is in 2024; the rest and the
// chat are in 2025. It is synthetic,
// with no attachments, labels, aliases, deletions, or external services.
func BenchmarkRelationshipScaleBuild(b *testing.B) {
	buildRelationshipScaleCache(b, 2_000_000, 303)
}

func buildRelationshipScaleCache(b *testing.B, emailCount, contactCount int) {
	b.Helper()
	root := *relationshipScaleRoot
	if root == "" {
		b.Skip("set -relationship-bench-root to an empty scratch directory")
	}
	if _, err := os.Stat(filepath.Join(root, datasetSources)); os.IsNotExist(err) {
		seed, cleanup := NewTestDataBuilder(b).Build()
		defer cleanup()
		require.NoError(b, os.CopyFS(root, os.DirFS(seed)))
	}
	db, err := duckdbutil.Open(b.Context(), duckdbutil.BuilderPolicy(b.TempDir()))
	require.NoError(b, err)
	defer func() { require.NoError(b, db.Close()) }()
	_, err = db.Exec(fmt.Sprintf(`CREATE VIEW scale_messages AS
		SELECT i::BIGINT AS id, 1::BIGINT AS source_id,
		       'message-' || i AS source_message_id,
		       CASE WHEN i <= %[1]d THEN i ELSE %[1]d + 1 END::BIGINT AS conversation_id,
		       'Synthetic message' AS subject, 'Synthetic preview' AS snippet,
		       CASE WHEN i <= %[1]d / 2 THEN TIMESTAMP '2024-01-01' ELSE TIMESTAMP '2025-01-01' END
		           + INTERVAL (i %% 1000000) SECOND AS sent_at,
		       100::BIGINT AS size_estimate, false AS has_attachments,
		       0::INTEGER AS attachment_count, NULL::TIMESTAMP AS deleted_from_source_at,
		       (2 + i %% %[2]d)::BIGINT AS sender_id, 1::BIGINT AS owner_participant_id,
		       CASE WHEN i <= %[1]d THEN 'email' ELSE 'imessage' END AS message_type,
		       NULL::VARCHAR AS list_id, false AS is_from_me,
		       CASE WHEN i <= %[1]d / 2 THEN 2024 ELSE 2025 END::INTEGER AS year, 1::INTEGER AS month
		FROM range(1, %[1]d + 2001) t(i)`, emailCount, contactCount))
	require.NoError(b, err)
	for dataset, query := range map[string]string{
		datasetMessages: `SELECT * FROM scale_messages`,
		datasetSources:  `SELECT 1::BIGINT AS id, 'owner@example.test' AS account_email, 'gmail' AS source_type`,
		datasetParticipants: fmt.Sprintf(`SELECT i::BIGINT AS id,
			'person-' || i || '@' || domain AS email_address,
			domain, 'Person ' || i AS display_name, '' AS phone_number
			FROM (SELECT i, CASE WHEN i = 1 THEN 'example.test'
				ELSE 'domain-' || i %% 1000 || '.example.test' END AS domain
				FROM range(1, %d) t(i))`, contactCount+2),
		datasetConversations: fmt.Sprintf(`SELECT id, 'thread-' || id AS source_conversation_id, 'Synthetic thread' AS title,
			CASE WHEN id = %[1]d + 1 THEN 'group_chat' ELSE 'email' END AS conversation_type
			FROM range(1, %[1]d + 2) t(id)`, emailCount),
		datasetConversationParticipants: fmt.Sprintf(`SELECT %d::BIGINT AS conversation_id, i::BIGINT AS participant_id FROM range(1, 305) t(i)`, emailCount+1),
		datasetOwnerParticipants:        `SELECT 1::BIGINT AS source_id, 1::BIGINT AS participant_id`,
		"message_recipients": `SELECT id AS message_id, sender_id AS participant_id, 'from' AS recipient_type,
			'' AS display_name, 'person-' || sender_id || '@domain-' || sender_id % 1000 || '.example.test' AS email_address,
			NULL::VARCHAR AS envelope_address FROM scale_messages
			UNION ALL SELECT id, 1::BIGINT, 'to', '', 'owner@example.test', NULL::VARCHAR FROM scale_messages`,
	} {
		dir := filepath.Join(root, dataset)
		require.NoError(b, os.RemoveAll(dir))
		require.NoError(b, os.MkdirAll(dir, 0o700))
		output := filepath.Join(dir, "data.parquet")
		options := "FORMAT PARQUET"
		if dataset == datasetMessages {
			output = dir
			options += ", PARTITION_BY (year), WRITE_PARTITION_COLUMNS true"
		}
		_, err := db.Exec("COPY (" + query + ") TO '" + escapePath(output) + "' (" + options + ")")
		require.NoError(b, err)
	}
	var result identityindex.BuildResult
	b.ResetTimer()
	for range b.N {
		result, err = identityindex.Build(b.Context(), db, identityindex.BuildOptions{
			Mode: identityindex.ModeFull, StagedBaseRoot: root, OutputRoot: root,
			EffectiveAt: time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC),
		})
		require.NoError(b, err)
	}
	b.StopTimer()
	fingerprint, err := CacheDatasetFingerprint(root)
	require.NoError(b, err)
	marker, err := json.Marshal(CacheSyncState{
		LastMessageID: int64(emailCount + 2000), LastSyncAt: time.Now(), PublishedAt: time.Now(),
		SchemaVersion: CacheSchemaVersion, DatasetFingerprint: fingerprint,
		ConversationParticipantsFingerprint: result.ConversationParticipantsFingerprint,
		Stats:                               result.Stats,
	})
	require.NoError(b, err)
	require.NoError(b, os.WriteFile(CacheStatePath(root), marker, 0o600))
}

// BenchmarkExploreScaleBuild reuses the relationship fixture with 2,560,000
// email entries, 2,000 chat messages, 71,486 people, and 1,001 domains. Build
// once in an empty scratch directory, then query repeatedly:
//
//	relationship_bench_root=$(mktemp -d)
//	go test -tags 'fts5 sqlite_vec' ./internal/query -run '^$' -bench '^BenchmarkExploreScaleBuild$' -benchtime=1x -args -relationship-bench-root="$relationship_bench_root"
//	go test -tags 'fts5 sqlite_vec' ./internal/query -run '^$' -bench '^BenchmarkExploreScaleQueries$' -benchtime=4x -args -relationship-bench-root="$relationship_bench_root"
//
// Add -relationship-bench-profile to the query command to write DuckDB JSON
// operator profiles, and -relationship-bench-memory=8GB to profile queries
// that exceed the default interactive memory limit. This population uses
// uniform email senders, one archive owner, and one chat roster; it does not
// model a real archive's participant skew, aliases, attachments, or full-text
// search. The first-query metric does not imply a cold filesystem cache.
func BenchmarkExploreScaleBuild(b *testing.B) {
	buildRelationshipScaleCache(b, 2_560_000, 71_485)
}

func BenchmarkExploreScaleQueries(b *testing.B) {
	root := *relationshipScaleRoot
	if root == "" {
		b.Skip("run BenchmarkExploreScaleBuild with a scratch directory first")
	}
	for _, name := range []string{"People", "Domains", "DomainDrill"} {
		b.Run(name, func(b *testing.B) {
			engine, err := NewDuckDBEngine(root, "", nil, DuckDBOptions{MemoryLimit: *relationshipScaleMemory})
			require.NoError(b, err)
			defer func() { require.NoError(b, engine.Close()) }()
			if *relationshipScaleProfile {
				_, err = engine.db.Exec("SET enable_profiling = 'json'")
				require.NoError(b, err)
				_, err = engine.db.Exec("SET profiling_output = '" + escapePath(filepath.Join(root, name+".json")) + "'")
				require.NoError(b, err)
			}
			var elapsed []time.Duration
			b.ResetTimer()
			for range b.N {
				start := time.Now()
				if name == "DomainDrill" {
					result, err := engine.Explore(b.Context(), ExploreRequest{
						Context: Context{Domains: []string{"domain-0.example.test"}},
						Page:    PageSpec{Limit: 50},
					})
					require.NoError(b, err)
					require.Len(b, result.Rows, 50)
					// 71 senders have domain-0; 58 sent 36 emails and 13 sent
					// 35. Two also authored messages in the one chat entry.
					require.Equal(b, int64(2544), result.TotalCount)
				} else {
					dimension, want := "participant", int64(71_486)
					if name == "Domains" {
						dimension, want = "domain", 1001
					}
					result, err := engine.ExploreGroups(b.Context(), ExploreGroupRequest{
						Dimension: dimension, Page: PageSpec{Limit: 50},
					})
					require.NoError(b, err)
					require.Len(b, result.Rows, 50)
					require.Equal(b, want, result.TotalCount)
				}
				elapsed = append(elapsed, time.Since(start))
			}
			b.StopTimer()
			b.ReportMetric(float64(elapsed[0].Microseconds())/1000, "first-ms")
			if len(elapsed) > 1 {
				slices.Sort(elapsed[1:])
				b.ReportMetric(float64(elapsed[1+len(elapsed[1:])/2].Microseconds())/1000, "warm-median-ms")
			}
		})
	}
}

func BenchmarkRelationshipScaleQueries(b *testing.B) {
	root := *relationshipScaleRoot
	if root == "" {
		b.Skip("run BenchmarkRelationshipScaleBuild with a scratch directory first")
	}
	for _, name := range []string{"Calendar", "Timeline", "PersonActivity", "MessageActivity"} {
		b.Run(name, func(b *testing.B) {
			engine, err := NewDuckDBEngine(root, "", nil)
			require.NoError(b, err)
			defer func() { require.NoError(b, engine.Close()) }()
			var elapsed []time.Duration
			b.ResetTimer()
			for range b.N {
				start := time.Now()
				switch name {
				case "Calendar":
					result, err := engine.RelationshipCalendar(b.Context(), RelationshipCalendarRequest{CanonicalID: 2, Year: 2025, Timezone: "UTC"})
					require.NoError(b, err)
					var total int64
					for _, day := range result.Days {
						total += day.Total
					}
					// Calendar counts this person's 3,300 emails and seven
					// authored group messages; silent membership is not a signal.
					require.Equal(b, int64(3307), total)
				case "Timeline":
					result, err := engine.RelationshipTimeline(b.Context(), RelationshipTimelineRequest{CanonicalID: 2, Limit: 25, Timezone: "UTC"})
					require.NoError(b, err)
					require.Len(b, result.Rows, 25)
					require.Equal(b, int64(6601), result.TotalCount)
				case "PersonActivity", "MessageActivity":
					filter := "canonical_id = 2"
					want := int64(8600)
					if name == "MessageActivity" {
						filter = "message_id = 2002000"
						want = 304
					}
					result, err := engine.QuerySQL(b.Context(), "SELECT count(*) FROM relationship_activity_expanded WHERE "+filter)
					require.NoError(b, err)
					require.Equal(b, 1, result.RowCount)
					require.Equal(b, want, result.Rows[0][0])
				}
				elapsed = append(elapsed, time.Since(start))
			}
			b.StopTimer()
			b.ReportMetric(float64(elapsed[0].Microseconds())/1000, "first-ms")
			if len(elapsed) > 1 {
				slices.Sort(elapsed[1:])
				b.ReportMetric(float64(elapsed[1+len(elapsed[1:])/2].Microseconds())/1000, "warm-median-ms")
			}
		})
	}
}
