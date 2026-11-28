package query

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/duckdbutil"
)

func TestDuckDBEngine_QuerySQL(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	builder := NewTestDataBuilder(t)
	srcID := builder.AddSource("test@example.com")
	bob := builder.AddParticipant(
		"bob@example.com", "example.com", "Bob",
	)
	msgID := builder.AddMessage(MessageOpt{
		Subject: "Test", SourceID: srcID, SizeEstimate: 100,
	})
	builder.AddFrom(msgID, bob, "Bob")

	engine := builder.BuildEngine()
	defer func() { _ = engine.Close() }()

	ctx := t.Context()
	result, err := engine.QuerySQL(ctx,
		"SELECT from_email, message_count FROM v_senders")
	require.NoError(err, "QuerySQL")
	require.GreaterOrEqual(len(result.Columns), 2, "columns")
	assert.Equal("from_email", result.Columns[0])
	assert.Equal(1, result.RowCount)
}

func TestRegisterViews_BaseViews(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	builder := NewTestDataBuilder(t)
	srcID := builder.AddSource("alice@example.com")
	partID := builder.AddParticipant(
		"bob@example.com", "example.com", "Bob",
	)
	lblID := builder.AddLabel("INBOX")
	msgID := builder.AddMessage(MessageOpt{
		Subject:  "Hello",
		SourceID: srcID,
	})
	builder.AddFrom(msgID, partID, "Bob")
	builder.AddMessageLabel(msgID, lblID)
	builder.AddAttachment(msgID, 500, "preview.jpg")

	dir, cleanup := builder.Build()
	defer cleanup()

	engine := builder.BuildEngine()
	defer func() { _ = engine.Close() }()

	require.NoError(RegisterViews(t.Context(), engine.db, dir), "RegisterViews")

	tables := []string{
		"messages", "participants", "message_recipients",
		"labels", "message_labels", "attachments",
		"conversations", "sources",
	}
	for _, table := range tables {
		var count int
		err := engine.db.QueryRowContext(
			t.Context(),
			"SELECT COUNT(*) FROM "+table,
		).Scan(&count)
		require.NoError(err, "query %s", table)
	}

	var id int64
	var subject, messageType string
	var attachmentCount int
	err := engine.db.QueryRowContext(
		t.Context(),
		"SELECT id, subject, attachment_count, message_type FROM messages LIMIT 1",
	).Scan(&id, &subject, &attachmentCount, &messageType)
	require.NoError(err, "scan messages")
	assert.Equal("Hello", subject)

	var attachmentMetadata sql.NullString
	err = engine.db.QueryRowContext(
		t.Context(),
		"SELECT attachment_metadata FROM attachments LIMIT 1",
	).Scan(&attachmentMetadata)
	require.NoError(err, "legacy attachment cache must expose attachment_metadata")
	assert.False(attachmentMetadata.Valid, "legacy attachment rows default to unclassified")
}

func TestRegisterViews_ListsCompatibility(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	listID := "<announce.example.test>"
	builder := NewTestDataBuilder(t)
	builder.AddSource("test@example.com")
	builder.AddMessage(MessageOpt{Subject: "Current cache", ListID: &listID})
	dir, cleanup := builder.Build()
	defer cleanup()

	engine, err := NewDuckDBEngine(t.Context(), "", "", nil)
	require.NoError(err)
	defer func() { _ = engine.Close() }()

	require.NoError(RegisterViews(t.Context(), engine.db, dir))
	var got string
	require.NoError(engine.db.QueryRowContext(t.Context(), "SELECT list_id FROM messages").Scan(&got))
	assert.Equal(t, listID, got)
}

func TestRegisterViews_OldMessagesCacheDefaultsListIDToNull(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	builder := NewTestDataBuilder(t)
	builder.AddSource("test@example.com")
	builder.AddMessage(MessageOpt{Subject: "Old cache"})
	dir, cleanup := builder.Build()
	defer cleanup()

	engine := builder.BuildEngine()
	defer func() { _ = engine.Close() }()

	messageFile := filepath.Join(dir, "messages", "year=2024", "data.parquet")
	oldMessageFile := filepath.Join(dir, "messages", "year=2024", "old.parquet")
	_, err := engine.db.ExecContext(t.Context(), fmt.Sprintf(
		"COPY (SELECT * EXCLUDE (list_id) FROM read_parquet('%s')) TO '%s' (FORMAT PARQUET)",
		escapePath(messageFile), escapePath(oldMessageFile),
	))
	require.NoError(err)
	require.NoError(os.Remove(messageFile))
	require.NoError(os.Rename(oldMessageFile, messageFile))
	fingerprint, err := CacheDatasetFingerprint(dir)
	require.NoError(err)
	state, err := ReadCacheSyncState(dir)
	require.NoError(err)
	state.DatasetFingerprint = fingerprint
	stateData, err := json.Marshal(state)
	require.NoError(err)
	require.NoError(os.WriteFile(CacheStatePath(dir), stateData, 0o600))

	require.NoError(RegisterViews(t.Context(), engine.db, dir))
	var listID sql.NullString
	require.NoError(engine.db.QueryRowContext(t.Context(), "SELECT list_id FROM messages").Scan(&listID))
	assert.False(listID.Valid)

	oldEngine, err := NewDuckDBEngine(t.Context(), dir, "", nil)
	require.NoError(err)
	defer func() { _ = oldEngine.Close() }()
	rows, err := oldEngine.Aggregate(t.Context(), ViewLists, DefaultAggregateOptions())
	require.NoError(err)
	assert.Empty(rows)
}

func TestRegisterViews_ConvenienceViews(t *testing.T) {
	t.Parallel()
	builder := NewTestDataBuilder(t)
	srcID := builder.AddSource("alice@example.com")
	bob := builder.AddParticipant(
		"bob@corp.com", "corp.com", "Bob Smith",
	)
	carol := builder.AddParticipant(
		"carol@corp.com", "corp.com", "Carol",
	)
	inbox := builder.AddLabel("INBOX")
	sent := builder.AddLabel("SENT")

	msg1 := builder.AddMessage(MessageOpt{
		Subject:      "First",
		SourceID:     srcID,
		SizeEstimate: 1000,
	})
	builder.AddFrom(msg1, bob, "Bob Smith")
	builder.AddTo(msg1, carol, "Carol")
	builder.AddMessageLabel(msg1, inbox)
	builder.AddAttachment(msg1, 500, "doc.pdf")

	msg2 := builder.AddMessage(MessageOpt{
		Subject:      "Second",
		SourceID:     srcID,
		SizeEstimate: 2000,
	})
	builder.AddFrom(msg2, bob, "Bob Smith")
	builder.AddMessageLabel(msg2, inbox)
	builder.AddMessageLabel(msg2, sent)

	dir, cleanup := builder.Build()
	defer cleanup()
	engine := builder.BuildEngine()
	defer func() { _ = engine.Close() }()

	require.NoError(t, RegisterViews(t.Context(), engine.db, dir), "RegisterViews")
	ctx := t.Context()

	t.Run("v_messages", func(t *testing.T) {
		assert := assert.New(t)
		var fromEmail, fromDomain, labels string
		err := engine.db.QueryRowContext(ctx,
			"SELECT from_email, from_domain, labels "+
				"FROM v_messages WHERE subject = 'First'",
		).Scan(&fromEmail, &fromDomain, &labels)
		require.NoError(t, err, "scan v_messages")
		assert.Equal("bob@corp.com", fromEmail)
		assert.Equal("corp.com", fromDomain)
		assert.Equal(`["INBOX"]`, labels)
	})

	t.Run("v_messages_multi_labels", func(t *testing.T) {
		var labels string
		err := engine.db.QueryRowContext(ctx,
			"SELECT labels FROM v_messages "+
				"WHERE subject = 'Second'",
		).Scan(&labels)
		require.NoError(t, err, "scan v_messages")
		assert.Equal(t, `["INBOX","SENT"]`, labels)
	})

	t.Run("v_senders", func(t *testing.T) {
		assert := assert.New(t)
		var fromName string
		var msgCount int64
		var totalSize int64
		err := engine.db.QueryRowContext(ctx,
			"SELECT from_name, message_count, total_size "+
				"FROM v_senders "+
				"WHERE from_email = 'bob@corp.com'",
		).Scan(&fromName, &msgCount, &totalSize)
		require.NoError(t, err, "scan v_senders")
		assert.Equal("Bob Smith", fromName)
		assert.Equal(int64(2), msgCount)
		assert.Equal(int64(3000), totalSize)
	})

	t.Run("v_domains", func(t *testing.T) {
		var msgCount, senderCount int64
		err := engine.db.QueryRowContext(ctx,
			"SELECT message_count, sender_count "+
				"FROM v_domains "+
				"WHERE domain = 'corp.com'",
		).Scan(&msgCount, &senderCount)
		require.NoError(t, err, "scan v_domains")
		assert.Equal(t, int64(2), msgCount)
		assert.Equal(t, int64(1), senderCount)
	})

	t.Run("v_labels", func(t *testing.T) {
		var msgCount int64
		err := engine.db.QueryRowContext(ctx,
			"SELECT message_count FROM v_labels "+
				"WHERE name = 'INBOX'",
		).Scan(&msgCount)
		require.NoError(t, err, "scan v_labels")
		assert.Equal(t, int64(2), msgCount)
	})

	t.Run("v_threads", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		// Both messages share the same conversation (auto-assigned),
		// so we expect exactly 2 threads (one per conversation).
		var threadCount int
		err := engine.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM v_threads",
		).Scan(&threadCount)
		require.NoError(err, "scan v_threads count")
		assert.Equal(2, threadCount)

		// Sum of message_count across all threads should be 2.
		var totalMsgCount int64
		err = engine.db.QueryRowContext(ctx,
			"SELECT SUM(message_count) FROM v_threads",
		).Scan(&totalMsgCount)
		require.NoError(err, "scan v_threads sum")
		assert.Equal(int64(2), totalMsgCount)

		// Verify participant_emails, conversation_title, conversation_type
		var participantEmails sql.NullString
		var convTitle, convType string
		err = engine.db.QueryRowContext(ctx,
			"SELECT participant_emails, conversation_title, conversation_type FROM v_threads LIMIT 1",
		).Scan(&participantEmails, &convTitle, &convType)
		require.NoError(err, "scan v_threads columns")
		assert.Equal("email", convType)
	})
}

// TestRegisterViews_RecipientAddressColumns proves the message_recipients
// view exposes both address columns of cache schema v26: envelope_address
// keeps NULL for rows where no header address was recorded instead of
// coercing them to the empty string, while email_address resolves those rows
// to the participant's current address. A legacy cache lacking both columns
// reads as NULL for each.
func TestRegisterViews_RecipientAddressColumns(t *testing.T) {
	t.Parallel()
	type addresses struct {
		resolved sql.NullString
		envelope sql.NullString
	}
	scan := func(t *testing.T, builder *TestDataBuilder) (populated, absent addresses) {
		t.Helper()
		dir, cleanup := builder.Build()
		t.Cleanup(cleanup)
		engine := builder.BuildEngine()
		t.Cleanup(func() { _ = engine.Close() })
		require.NoError(t, RegisterViews(t.Context(), engine.db, dir), "RegisterViews")

		err := engine.db.QueryRowContext(
			t.Context(),
			`SELECT email_address, envelope_address FROM message_recipients WHERE recipient_type = 'from'`,
		).Scan(&populated.resolved, &populated.envelope)
		require.NoError(t, err, "scan populated row")
		err = engine.db.QueryRowContext(
			t.Context(),
			`SELECT email_address, envelope_address FROM message_recipients WHERE recipient_type = 'to'`,
		).Scan(&absent.resolved, &absent.envelope)
		require.NoError(t, err, "scan absent row")
		return populated, absent
	}

	t.Run("current cache", func(t *testing.T) {
		assert := assert.New(t)
		builder := NewTestDataBuilder(t)
		srcID := builder.AddSource("owner@example.com")
		alice := builder.AddParticipant("alice@example.com", "example.com", "Alice")
		bob := builder.AddParticipant("bob@example.com", "example.com", "Bob")
		msgID := builder.AddMessage(MessageOpt{Subject: "Hello", SourceID: srcID})
		builder.AddRecipientWithEnvelope(msgID, alice, "from", "Alice", "alice-alias@example.com")
		builder.AddRecipient(msgID, bob, "to", "Bob")

		populated, absent := scan(t, builder)
		alias := sql.NullString{String: "alice-alias@example.com", Valid: true}
		assert.Equal(alias, populated.envelope)
		assert.Equal(alias, populated.resolved,
			"a recorded header address is also the resolved address")
		assert.Equal(sql.NullString{}, absent.envelope,
			"row without a recorded address reads as NULL, not ''")
		assert.Equal(sql.NullString{String: "bob@example.com", Valid: true}, absent.resolved,
			"row without a recorded address resolves to the participant's address")
	})

	t.Run("legacy cache without the columns", func(t *testing.T) {
		builder := NewTestDataBuilder(t)
		builder.legacyRecipientSchema = true
		srcID := builder.AddSource("owner@example.com")
		alice := builder.AddParticipant("alice@example.com", "example.com", "Alice")
		bob := builder.AddParticipant("bob@example.com", "example.com", "Bob")
		msgID := builder.AddMessage(MessageOpt{Subject: "Hello", SourceID: srcID})
		builder.AddFrom(msgID, alice, "Alice")
		builder.AddTo(msgID, bob, "Bob")

		populated, absent := scan(t, builder)
		assert.Equal(t, addresses{}, populated, "legacy cache carries neither column")
		assert.Equal(t, addresses{}, absent)
	})
}

func TestVMessagesKeepUnnamedDirectSenderNamesEmpty(t *testing.T) {
	builder := NewTestDataBuilder(t)
	builder.AddSource("archive@example.com")
	phone := builder.AddPhoneParticipant("+12025550123", "")
	email := builder.AddParticipant("sender@example.com", "example.com", "")
	ids := []int64{
		builder.AddMessage(MessageOpt{SenderID: &phone}),
		builder.AddMessage(MessageOpt{SenderID: &email}),
	}
	engine := builder.BuildEngine()
	for _, id := range ids {
		var fromName string
		require.NoError(t, engine.db.QueryRowContext(t.Context(), "SELECT from_name FROM v_messages WHERE id = ?", id).Scan(&fromName))
		assert.Empty(t, fromName, "other readers keep an empty name for unnamed direct senders")
	}
}

// TestVMessagesFilteredWideColumnsUnderSpill guards against DuckDB's
// dict-surviving hash join bug (fixed in DuckDB v2.0.1 / duckdb-go v2.20001.0)
// where filtering v_messages to a small message_type subset while selecting
// wide columns (subject, sent_at) failed when the intermediate direct-sender
// join spilled its probe side under a bounded memory limit.
func TestVMessagesFilteredWideColumnsUnderSpill(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-million-row spill regression in short mode")
	}
	dir := t.TempDir()
	for _, sub := range []string{
		"messages/year=2025", "messages/year=2026",
		"participants", "message_recipients", "labels", "message_labels",
		"attachments", "conversations", "conversation_participants",
		"participant_identifiers", "sources",
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, sub), 0o755))
	}

	// Build a Parquet archive large enough that v_messages' unaggregated
	// direct-sender join spills under a 128MB memory limit while the filtered
	// meeting_transcript subset stays small enough for dictionary emission.
	setupDB, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	defer func() { _ = setupDB.Close() }()

	const totalEmails = 1_200_000
	const halfEmails = totalEmails / 2
	exec := func(stmt string) {
		t.Helper()
		_, execErr := setupDB.ExecContext(t.Context(), stmt)
		require.NoError(t, execErr)
	}
	parquetPath := func(rel string) string {
		return escapePath(filepath.Join(dir, rel))
	}

	exec(fmt.Sprintf(`COPY (
		SELECT 1::BIGINT AS id, 'gmail'::VARCHAR AS source_type, 'owner@example.com'::VARCHAR AS account_email
		UNION ALL SELECT 2::BIGINT, 'granola'::VARCHAR, 'notes@example.com'::VARCHAR
	) TO '%s' (FORMAT PARQUET)`, parquetPath("sources/data.parquet")))
	exec(fmt.Sprintf(`COPY (
		SELECT i::BIGINT AS id,
			'user' || i || '@example.com' AS email_address,
			'example.com' AS domain,
			'User ' || i AS display_name,
			'' AS phone_number
		FROM range(1, 2001) t(i)
	) TO '%s' (FORMAT PARQUET)`, parquetPath("participants/data.parquet")))
	exec(fmt.Sprintf(`COPY (
		SELECT i::BIGINT AS id, 'LABEL_' || i AS name FROM range(1, 11) t(i)
	) TO '%s' (FORMAT PARQUET)`, parquetPath("labels/data.parquet")))
	exec(fmt.Sprintf(`COPY (
		SELECT i::BIGINT AS id, 1::BIGINT AS source_id, 'm-' || i AS source_message_id,
			i::BIGINT AS conversation_id, 'Subject ' || (i %% 10000) AS subject,
			'Snippet' AS snippet, TIMESTAMP '2025-01-01 00:00:00' + INTERVAL (i %% 86400) SECOND AS sent_at,
			1024::BIGINT AS size_estimate, false AS has_attachments, 0::INTEGER AS attachment_count,
			'email'::VARCHAR AS message_type, 1::INTEGER AS month,
			((i %% 2000) + 1)::BIGINT AS sender_id, NULL::BIGINT AS owner_participant_id,
			NULL::VARCHAR AS list_id, NULL::TIMESTAMP AS deleted_at, NULL::TIMESTAMP AS deleted_from_source_at,
			false AS is_from_me, false AS archive_only, NULL::VARCHAR AS account_address, NULL::VARCHAR AS account_path
		FROM range(1, %d) t(i)
	) TO '%s' (FORMAT PARQUET)`, halfEmails+1, parquetPath("messages/year=2025/data.parquet")))
	exec(fmt.Sprintf(`COPY (
		SELECT i::BIGINT AS id, 1::BIGINT AS source_id, 'm-' || i AS source_message_id,
			i::BIGINT AS conversation_id, 'Subject ' || (i %% 10000) AS subject,
			'Snippet' AS snippet, TIMESTAMP '2026-01-01 00:00:00' + INTERVAL (i %% 86400) SECOND AS sent_at,
			1024::BIGINT AS size_estimate, false AS has_attachments, 0::INTEGER AS attachment_count,
			'email'::VARCHAR AS message_type, 1::INTEGER AS month,
			((i %% 2000) + 1)::BIGINT AS sender_id, NULL::BIGINT AS owner_participant_id,
			NULL::VARCHAR AS list_id, NULL::TIMESTAMP AS deleted_at, NULL::TIMESTAMP AS deleted_from_source_at,
			false AS is_from_me, false AS archive_only, NULL::VARCHAR AS account_address, NULL::VARCHAR AS account_path
		FROM range(%d, %d) t(i)
		UNION ALL
		SELECT (%d + i)::BIGINT AS id, 2::BIGINT AS source_id, 'mt-' || i AS source_message_id,
			(%d + i)::BIGINT AS conversation_id, 'Meeting ' || i AS subject,
			'Notes ' || i AS snippet, TIMESTAMP '2026-02-01 00:00:00' + INTERVAL (i) HOUR AS sent_at,
			512::BIGINT AS size_estimate, false AS has_attachments, 0::INTEGER AS attachment_count,
			'meeting_transcript'::VARCHAR AS message_type, 2::INTEGER AS month,
			CASE WHEN i < 3 THEN NULL::BIGINT ELSE ((i %% 50) + 1)::BIGINT END AS sender_id,
			NULL::BIGINT AS owner_participant_id, NULL::VARCHAR AS list_id,
			NULL::TIMESTAMP AS deleted_at, NULL::TIMESTAMP AS deleted_from_source_at,
			false AS is_from_me, false AS archive_only, NULL::VARCHAR AS account_address, NULL::VARCHAR AS account_path
		FROM range(754) t(i)
	) TO '%s' (FORMAT PARQUET)`, halfEmails+1, totalEmails+1, totalEmails+1, totalEmails+1, parquetPath("messages/year=2026/data.parquet")))
	exec(fmt.Sprintf(`COPY (
		SELECT i::BIGINT AS message_id, ((i %% 2000) + 1)::BIGINT AS participant_id,
			'from'::VARCHAR AS recipient_type, 'User' AS display_name,
			'user' || ((i %% 2000) + 1) || '@example.com' AS email_address,
			NULL::VARCHAR AS envelope_address, false AS is_relayed
		FROM range(1, %d) t(i)
	) TO '%s' (FORMAT PARQUET)`, totalEmails+50, parquetPath("message_recipients/data.parquet")))
	exec(fmt.Sprintf(`COPY (
		SELECT i::BIGINT AS message_id, ((i %% 10) + 1)::BIGINT AS label_id
		FROM range(1, %d) t(i)
	) TO '%s' (FORMAT PARQUET)`, totalEmails+100, parquetPath("message_labels/data.parquet")))
	exec(fmt.Sprintf(`COPY (SELECT 1::BIGINT AS message_id, 0::BIGINT AS size, ''::VARCHAR AS filename, ''::VARCHAR AS mime_type, NULL::VARCHAR AS attachment_metadata WHERE false) TO '%s' (FORMAT PARQUET)`, parquetPath("attachments/data.parquet")))
	exec(fmt.Sprintf(`COPY (SELECT 1::BIGINT AS id, ''::VARCHAR AS source_conversation_id, ''::VARCHAR AS title, 'email'::VARCHAR AS conversation_type WHERE false) TO '%s' (FORMAT PARQUET)`, parquetPath("conversations/data.parquet")))
	exec(fmt.Sprintf(`COPY (SELECT 1::BIGINT AS conversation_id, 1::BIGINT AS participant_id WHERE false) TO '%s' (FORMAT PARQUET)`, parquetPath("conversation_participants/data.parquet")))
	exec(fmt.Sprintf(`COPY (SELECT 1::BIGINT AS participant_id, ''::VARCHAR AS identifier_type, ''::VARCHAR AS identifier_value, ''::VARCHAR AS display_value, false AS is_primary WHERE false) TO '%s' (FORMAT PARQUET)`, parquetPath("participant_identifiers/data.parquet")))

	policy := duckdbutil.InteractivePolicyWithOverrides(t.TempDir(), duckdbutil.InteractiveOverrides{
		MemoryLimit: "128MB",
		Threads:     4,
	})
	db, err := duckdbutil.Open(t.Context(), policy)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	require.NoError(t, RegisterViews(t.Context(), db, dir))

	t.Run("subject", func(t *testing.T) {
		rows, err := db.QueryContext(t.Context(),
			"SELECT id, subject FROM v_messages WHERE message_type = 'meeting_transcript' LIMIT 2")
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		count := 0
		for rows.Next() {
			var id int64
			var subject string
			require.NoError(t, rows.Scan(&id, &subject))
			assert.Greater(t, id, int64(totalEmails))
			assert.True(t, strings.HasPrefix(subject, "Meeting "), "unexpected subject %q", subject)
			count++
		}
		require.NoError(t, rows.Err())
		assert.Equal(t, 2, count)
	})

	t.Run("sent_at", func(t *testing.T) {
		rows, err := db.QueryContext(t.Context(),
			"SELECT id, sent_at FROM v_messages WHERE message_type = 'meeting_transcript' LIMIT 2")
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		count := 0
		for rows.Next() {
			var id int64
			var sentAt time.Time
			require.NoError(t, rows.Scan(&id, &sentAt))
			assert.Greater(t, id, int64(totalEmails))
			assert.Equal(t, 2026, sentAt.Year())
			assert.Equal(t, time.February, sentAt.Month())
			count++
		}
		require.NoError(t, rows.Err())
		assert.Equal(t, 2, count)
	})

	t.Run("like_sent_at_and_subject", func(t *testing.T) {
		rows, err := db.QueryContext(t.Context(),
			"SELECT id, sent_at, subject FROM v_messages WHERE message_type LIKE 'meeting_transcript' LIMIT 2")
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		count := 0
		for rows.Next() {
			var id int64
			var sentAt time.Time
			var subject string
			require.NoError(t, rows.Scan(&id, &sentAt, &subject))
			assert.Greater(t, id, int64(totalEmails))
			assert.Equal(t, 2026, sentAt.Year())
			assert.True(t, strings.HasPrefix(subject, "Meeting "), "unexpected subject %q", subject)
			count++
		}
		require.NoError(t, rows.Err())
		assert.Equal(t, 2, count)
	})
}
