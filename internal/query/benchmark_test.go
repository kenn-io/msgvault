package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

const (
	benchChatMessageCount = 100_000
	benchAuxMessageCount  = 4
	benchMessageCount     = benchChatMessageCount + benchAuxMessageCount
)

// buildBenchData generates a mixed 100K+-message Parquet dataset directly via
// DuckDB SQL (no Go-side row generation). This produces realistic
// cardinality: 100 chat conversations containing 100K raw fragments plus
// email, calendar, meeting-note, and file-bearing records, 500 participants
// across 50 domains, 10 labels, varied subjects/snippets, and 20% attachment
// rate.
func buildBenchData(tb testing.TB) *DuckDBEngine {
	tb.Helper()
	requirements := require.New(tb)

	tmpDir := tb.TempDir()

	db, err := sql.Open("duckdb", "")
	requirements.NoError(err, "open duckdb")
	defer func() { _ = db.Close() }()

	// Seed tables as DuckDB views, then COPY to Parquet.
	seedSQL := fmt.Sprintf(`
		-- Sources
		CREATE TABLE bench_sources AS
		SELECT 1::BIGINT AS id,
			   'bench@example.com' AS account_email,
			   'gmail' AS source_type;

		-- 500 participants across 50 domains
		CREATE TABLE bench_domains AS
		SELECT row_number() OVER () AS idx, domain FROM (VALUES
			('acme'),('globex'),('initech'),('hooli'),
			('piedpiper'),('waystar'),('dunder'),('sterling'),
			('prestige'),('vandelay'),('contoso'),('fabrikam'),
			('northwind'),('widgetco'),('megacorp'),('cyberdyne'),
			('umbrella'),('abstergo'),('aperture'),('blackmesa'),
			('oscorp'),('lexcorp'),('stark'),('wayne'),
			('genco'),('soylent'),('tyrell'),('weyland'),
			('momcorp'),('planet'),('capsule'),('zorg'),
			('omni'),('axiom'),('buyenlarge'),('cybertron'),
			('starfleet'),('shield'),('hydra'),('dharma'),
			('oceanic'),('hanso'),('massive'),('ellingson'),
			('shinra'),('mako'),('jenova'),('umbrellacorp'),
			('racoon'),('torgue')
		) d(domain);

		CREATE TABLE bench_participants AS
		SELECT
			i::BIGINT AS id,
			'user' || i || '@' || d.domain || '.com' AS email_address,
			d.domain || '.com' AS domain,
			'User ' || i AS display_name,
			'' AS phone_number
		FROM generate_series(1, 500) t(i)
		JOIN bench_domains d ON d.idx = ((i - 1) %% 50) + 1;

		CREATE TABLE bench_participant_identifiers AS
		SELECT id AS participant_id, 'email' AS identifier_type,
		       email_address AS identifier_value, email_address AS display_value,
		       true AS is_primary
		FROM bench_participants;

		-- 10 labels
		CREATE TABLE bench_labels AS
		SELECT * FROM (VALUES
			(1::BIGINT, 'INBOX'), (2, 'Work'), (3, 'IMPORTANT'),
			(4, 'SENT'), (5, 'Personal'), (6, 'Promotions'),
			(7, 'Updates'), (8, 'Social'), (9, 'Finance'),
			(10, 'Travel')
		) AS t(id, name);

		-- Subject templates (10 patterns)
		CREATE TABLE bench_subjects AS
		SELECT * FROM (VALUES
			('Q%% budget review meeting notes'),
			('Re: Project alpha deployment plan'),
			('Weekly standup summary for team'),
			('Invoice #%% attached for approval'),
			('Security advisory: update required'),
			('Quarterly report financials'),
			('Team offsite planning logistics'),
			('PR review: fix authentication bug'),
			('Database migration rollback plan'),
			('Customer feedback analysis report')
		) AS t(template);

		-- 100K chat fragments plus mixed durable archive items.
		CREATE TABLE bench_messages AS
		SELECT
			i::BIGINT AS id,
			1::BIGINT AS source_id,
			'msg' || i AS source_message_id,
			CASE WHEN i <= %d THEN (200 + ((i - 1) %% 100))::BIGINT ELSE (10000 + i)::BIGINT END AS conversation_id,
			CASE (i %% 10)
				WHEN 0 THEN 'Q' || (i/10000+1) || ' budget review meeting notes'
				WHEN 1 THEN 'Re: Project alpha deployment plan #' || i
				WHEN 2 THEN 'Weekly standup summary for team ' || (i%%20+1)
				WHEN 3 THEN 'Invoice #' || i || ' attached for approval'
				WHEN 4 THEN 'Security advisory: update required'
				WHEN 5 THEN 'Quarterly report Q' || (i%%4+1) || ' financials'
				WHEN 6 THEN 'Team offsite planning logistics'
				WHEN 7 THEN 'PR review: fix authentication bug #' || i
				WHEN 8 THEN 'Database migration rollback plan'
				ELSE 'Customer feedback analysis report Q' || (i%%4+1)
			END AS subject,
			'Preview text for message ' || i || ' about various topics' AS snippet,
			TIMESTAMP '2020-01-01' + INTERVAL (i * 30) MINUTE AS sent_at,
			(500 + (i %% 10) * 200)::BIGINT AS size_estimate,
			(i %% 5 = 0)::BOOLEAN AS has_attachments,
			NULL::TIMESTAMP AS deleted_from_source_at,
			(i %% 5 = 0)::INTEGER AS attachment_count,
			CASE WHEN i <= %d THEN ((i %% 500) + 1)::BIGINT ELSE NULL::BIGINT END AS sender_id,
			CASE
				WHEN i <= %d THEN 'imessage'
				WHEN i %% 4 = 0 THEN 'email'
				WHEN i %% 4 = 1 THEN 'calendar_event'
				WHEN i %% 4 = 2 THEN 'meeting_note'
				ELSE 'file_item'
			END AS message_type,
			EXTRACT(YEAR FROM TIMESTAMP '2020-01-01' + INTERVAL (i * 30) MINUTE)::INTEGER AS year,
			EXTRACT(MONTH FROM TIMESTAMP '2020-01-01' + INTERVAL (i * 30) MINUTE)::INTEGER AS month
		FROM generate_series(1, %d) t(i);

		-- message_recipients: 1 sender + 1-2 recipients per message
		CREATE TABLE bench_recipients AS
		-- sender
		SELECT
			m.id AS message_id,
			((m.id %% 500) + 1)::BIGINT AS participant_id,
			'from' AS recipient_type,
			'User ' || ((m.id %% 500) + 1) AS display_name
		FROM bench_messages m
		UNION ALL
		-- primary recipient
		SELECT
			m.id AS message_id,
			(((m.id + 1) %% 500) + 1)::BIGINT AS participant_id,
			'to' AS recipient_type,
			'User ' || (((m.id + 1) %% 500) + 1) AS display_name
		FROM bench_messages m
		UNION ALL
		-- cc on 33%% of messages
		SELECT
			m.id AS message_id,
			(((m.id + 2) %% 500) + 1)::BIGINT AS participant_id,
			'cc' AS recipient_type,
			'User ' || (((m.id + 2) %% 500) + 1) AS display_name
		FROM bench_messages m
		WHERE m.id %% 3 = 0;

		-- message_labels: 1-2 labels per message
		CREATE TABLE bench_message_labels AS
		-- every message gets INBOX
		SELECT m.id AS message_id, 1::BIGINT AS label_id
		FROM bench_messages m
		UNION ALL
		-- second label round-robin (skip label 1 to avoid dups)
		SELECT m.id AS message_id, ((m.id %% 9) + 2)::BIGINT AS label_id
		FROM bench_messages m;

		-- attachments on ~20%% of messages
		CREATE TABLE bench_attachments AS
		SELECT
			row_number() OVER (ORDER BY m.id)::BIGINT AS attachment_id,
			m.id AS message_id,
			(1000 + m.id * 10)::BIGINT AS size,
			'file' || m.id || '.pdf' AS filename,
			'application/pdf' AS mime_type
		FROM bench_messages m
		WHERE m.id %% 5 = 0;

		-- conversations
		CREATE TABLE bench_conversations AS
		SELECT DISTINCT
			conversation_id AS id,
			'thread' || conversation_id AS source_conversation_id,
			CASE WHEN conversation_id BETWEEN 200 AND 299 THEN 'Synthetic chat ' || conversation_id ELSE '' END AS title,
			CASE WHEN conversation_id BETWEEN 200 AND 299 THEN 'direct_chat' ELSE 'email' END AS conversation_type
		FROM bench_messages;

		CREATE TABLE bench_conversation_participants AS
		SELECT id AS conversation_id, (((id - 200) %% 500) + 1)::BIGINT AS participant_id
		FROM bench_conversations
		WHERE id BETWEEN 200 AND 299;
	`, benchChatMessageCount, benchChatMessageCount, benchChatMessageCount, benchMessageCount)

	_, err = db.Exec(seedSQL)
	requirements.NoError(err, "seed bench data")

	// Write Parquet files in the layout the engine expects.
	type tableSpec struct {
		query  string
		subdir string
		file   string
	}

	tables := []tableSpec{
		{"SELECT id, account_email, source_type FROM bench_sources",
			"sources", "sources.parquet"},
		{"SELECT id, email_address, domain, display_name, phone_number FROM bench_participants",
			"participants", "participants.parquet"},
		{"SELECT participant_id, identifier_type, identifier_value, display_value, is_primary FROM bench_participant_identifiers",
			"participant_identifiers", "participant_identifiers.parquet"},
		{"SELECT message_id, participant_id, recipient_type, display_name FROM bench_recipients",
			"message_recipients", "message_recipients.parquet"},
		{"SELECT id, name FROM bench_labels",
			"labels", "labels.parquet"},
		{"SELECT message_id, label_id FROM bench_message_labels",
			"message_labels", "message_labels.parquet"},
		{"SELECT attachment_id, message_id, size, filename, mime_type FROM bench_attachments",
			"attachments", "attachments.parquet"},
		{"SELECT id, source_conversation_id, title, conversation_type FROM bench_conversations",
			"conversations", "conversations.parquet"},
		{"SELECT conversation_id, participant_id FROM bench_conversation_participants",
			"conversation_participants", "conversation_participants.parquet"},
		// No confirmed identities or linked participants in the benchmark
		// fixture; write empty, schema-bearing files so RequiredParquetDirs
		// stays complete.
		{"SELECT 0::BIGINT AS source_id, 0::BIGINT AS participant_id WHERE false",
			datasetOwnerParticipants, datasetOwnerParticipants + ".parquet"},
		{"SELECT 0::BIGINT AS participant_id, 0::BIGINT AS canonical_id WHERE false",
			datasetParticipantClusters, datasetParticipantClusters + ".parquet"},
	}

	for _, t := range tables {
		dir := filepath.Join(tmpDir, t.subdir)
		requirements.NoError(os.MkdirAll(dir, 0o755), "mkdir %s", dir)
		path := escapePath(filepath.Join(dir, t.file))
		q := fmt.Sprintf("COPY (%s) TO '%s' (FORMAT PARQUET)", t.query, path)
		_, err = db.Exec(q)
		requirements.NoError(err, "write parquet %s", t.file)
	}

	// Messages are hive-partitioned by year.
	msgDir := filepath.Join(tmpDir, "messages")
	requirements.NoError(os.MkdirAll(msgDir, 0o755), "mkdir messages")
	msgPath := escapePath(msgDir)
	msgCopy := fmt.Sprintf(`
		COPY (
			SELECT id, source_id, source_message_id, conversation_id,
				   subject, snippet, sent_at, size_estimate, has_attachments,
				   deleted_from_source_at, attachment_count, sender_id,
				   message_type, year, month
			FROM bench_messages
		) TO '%s' (FORMAT PARQUET, PARTITION_BY (year), OVERWRITE_OR_IGNORE)
	`, msgPath)
	_, err = db.Exec(msgCopy)
	requirements.NoError(err, "write messages parquet")
	fingerprint, err := CacheDatasetFingerprint(tmpDir)
	requirements.NoError(err, "fingerprint benchmark cache")
	state, err := json.Marshal(CacheSyncState{
		LastMessageID:      benchMessageCount,
		LastSyncAt:         time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC),
		PublishedAt:        time.Date(2026, 1, 3, 12, 1, 0, 0, time.UTC),
		SchemaVersion:      CacheSchemaVersion,
		DatasetFingerprint: fingerprint,
	})
	requirements.NoError(err, "marshal benchmark cache state")
	requirements.NoError(os.WriteFile(CacheStatePath(tmpDir), state, 0o600), "write benchmark cache state")

	engine, err := NewDuckDBEngine(tmpDir, "", nil)
	requirements.NoError(err, "NewDuckDBEngine")
	tb.Cleanup(func() { requirements.NoError(engine.Close()) })
	return engine
}

func BenchmarkSearchFast(b *testing.B) {
	engine := buildBenchData(b)
	ctx := context.Background()

	b.Run("single_term", func(b *testing.B) {
		q := search.Parse("budget")
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.SearchFast(ctx, q,
				MessageFilter{}, 50, 0)
			require.NoError(b, err)
		}
	})

	b.Run("multi_term", func(b *testing.B) {
		q := search.Parse("budget review meeting")
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.SearchFast(ctx, q,
				MessageFilter{}, 50, 0)
			require.NoError(b, err)
		}
	})

	b.Run("from_filter", func(b *testing.B) {
		q := search.Parse("from:user5@acme.com budget")
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.SearchFast(ctx, q,
				MessageFilter{}, 50, 0)
			require.NoError(b, err)
		}
	})

	b.Run("no_match", func(b *testing.B) {
		q := search.Parse("xyzzynonexistent")
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.SearchFast(ctx, q,
				MessageFilter{}, 50, 0)
			require.NoError(b, err)
		}
	})

	b.Run("with_sender_filter", func(b *testing.B) {
		q := search.Parse("report")
		filter := MessageFilter{
			Sender: "user1@acme.com",
		}
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.SearchFast(ctx, q,
				filter, 50, 0)
			require.NoError(b, err)
		}
	})
}

func BenchmarkSearchFastWithStats(b *testing.B) {
	engine := buildBenchData(b)
	ctx := context.Background()

	b.Run("single_term", func(b *testing.B) {
		q := search.Parse("budget")
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.SearchFastWithStats(ctx, q,
				"budget", MessageFilter{},
				ViewSenders, 50, 0)
			require.NoError(b, err)
		}
	})

	b.Run("multi_term", func(b *testing.B) {
		q := search.Parse("quarterly report financials")
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.SearchFastWithStats(ctx, q,
				"quarterly report financials",
				MessageFilter{}, ViewSenders, 50, 0)
			require.NoError(b, err)
		}
	})
}

func BenchmarkAggregate(b *testing.B) {
	engine := buildBenchData(b)
	ctx := context.Background()
	opts := AggregateOptions{SortField: SortByCount}

	b.Run("senders", func(b *testing.B) {
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.Aggregate(ctx, ViewSenders, opts)
			require.NoError(b, err)
		}
	})

	b.Run("domains", func(b *testing.B) {
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.Aggregate(ctx, ViewDomains, opts)
			require.NoError(b, err)
		}
	})

	b.Run("labels", func(b *testing.B) {
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.Aggregate(ctx, ViewLabels, opts)
			require.NoError(b, err)
		}
	})

	b.Run("time", func(b *testing.B) {
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.Aggregate(ctx, ViewTime, opts)
			require.NoError(b, err)
		}
	})

	b.Run("senders_with_search", func(b *testing.B) {
		opts := AggregateOptions{
			SortField:   SortByCount,
			SearchQuery: "budget",
		}
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.Aggregate(ctx, ViewSenders, opts)
			require.NoError(b, err)
		}
	})
}

func BenchmarkGetTotalStats(b *testing.B) {
	engine := buildBenchData(b)
	ctx := context.Background()

	b.Run("no_filter", func(b *testing.B) {
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.GetTotalStats(ctx, StatsOptions{})
			require.NoError(b, err)
		}
	})

	b.Run("with_search", func(b *testing.B) {
		opts := StatsOptions{SearchQuery: "budget review"}
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.GetTotalStats(ctx, opts)
			require.NoError(b, err)
		}
	})
}

func BenchmarkListMessages(b *testing.B) {
	engine := buildBenchData(b)
	ctx := context.Background()

	b.Run("no_filter", func(b *testing.B) {
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.ListMessages(ctx, MessageFilter{})
			require.NoError(b, err)
		}
	})

	b.Run("sender_filter", func(b *testing.B) {
		filter := MessageFilter{Sender: "user1@acme.com"}
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.ListMessages(ctx, filter)
			require.NoError(b, err)
		}
	})

	b.Run("domain_filter", func(b *testing.B) {
		filter := MessageFilter{Domain: "acme.com"}
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.ListMessages(ctx, filter)
			require.NoError(b, err)
		}
	})

	b.Run("label_filter", func(b *testing.B) {
		filter := MessageFilter{Label: "Work"}
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.ListMessages(ctx, filter)
			require.NoError(b, err)
		}
	})
}

func BenchmarkSubAggregate(b *testing.B) {
	engine := buildBenchData(b)
	ctx := context.Background()
	opts := AggregateOptions{SortField: SortByCount}

	b.Run("sender_to_labels", func(b *testing.B) {
		filter := MessageFilter{Sender: "user1@acme.com"}
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.SubAggregate(ctx, filter,
				ViewLabels, opts)
			require.NoError(b, err)
		}
	})

	b.Run("domain_to_senders", func(b *testing.B) {
		filter := MessageFilter{Domain: "acme.com"}
		b.ResetTimer()
		for b.Loop() {
			_, err := engine.SubAggregate(ctx, filter,
				ViewSenders, opts)
			require.NoError(b, err)
		}
	})
}
