package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestSQLite creates a test SQLite database with realistic email data.
func setupTestSQLite(t *testing.T) string {
	t.Helper()

	tmpDir := t.TempDir()

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open sqlite")
	defer func() { _ = db.Close() }()

	// Create schema
	schema := `
		CREATE TABLE sources (
			id INTEGER PRIMARY KEY,
			source_type TEXT NOT NULL DEFAULT 'gmail',
			identifier TEXT NOT NULL UNIQUE,
			display_name TEXT
		);

		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL REFERENCES sources(id),
			source_message_id TEXT NOT NULL,
			conversation_id INTEGER,
			subject TEXT,
			snippet TEXT,
			sent_at TIMESTAMP,
			received_at TIMESTAMP,
			size_estimate INTEGER,
			has_attachments BOOLEAN DEFAULT FALSE,
			attachment_count INTEGER DEFAULT 0,
			deleted_from_source_at TIMESTAMP,
			sender_id INTEGER,
			message_type TEXT NOT NULL DEFAULT 'email',
			deleted_at DATETIME,
			UNIQUE(source_id, source_message_id)
		);

		CREATE TABLE participants (
			id INTEGER PRIMARY KEY,
			email_address TEXT NOT NULL UNIQUE,
			domain TEXT,
			display_name TEXT,
			phone_number TEXT
		);

		CREATE TABLE message_recipients (
			id INTEGER PRIMARY KEY,
			message_id INTEGER NOT NULL REFERENCES messages(id),
			participant_id INTEGER NOT NULL REFERENCES participants(id),
			recipient_type TEXT NOT NULL,
			display_name TEXT
		);

		CREATE TABLE labels (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL REFERENCES sources(id),
			source_label_id TEXT,
			name TEXT NOT NULL,
			label_type TEXT
		);

		CREATE TABLE message_labels (
			message_id INTEGER NOT NULL REFERENCES messages(id),
			label_id INTEGER NOT NULL REFERENCES labels(id),
			PRIMARY KEY (message_id, label_id)
		);

		CREATE TABLE attachments (
			id INTEGER PRIMARY KEY,
			message_id INTEGER NOT NULL REFERENCES messages(id),
			filename TEXT,
			mime_type TEXT,
			size INTEGER,
			content_hash TEXT
		);

		CREATE TABLE conversations (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL REFERENCES sources(id),
			source_conversation_id TEXT,
			title TEXT,
			conversation_type TEXT NOT NULL DEFAULT 'email'
		);
	`

	if _, err := db.Exec(schema); err != nil {
		_ = os.RemoveAll(tmpDir)
		require.NoError(t, err, "create schema")
	}

	// Insert test data
	testData := `
		-- Source
		INSERT INTO sources (id, identifier, display_name) VALUES (1, 'test@gmail.com', 'Test Account');

		-- Participants
		INSERT INTO participants (id, email_address, domain, display_name) VALUES
			(1, 'alice@example.com', 'example.com', 'Alice Smith'),
			(2, 'bob@company.org', 'company.org', 'Bob Jones'),
			(3, 'carol@example.com', 'example.com', 'Carol White'),
			(4, 'dan@other.net', 'other.net', 'Dan Brown');

		-- Labels
		INSERT INTO labels (id, source_id, name) VALUES
			(1, 1, 'INBOX'),
			(2, 1, 'Work'),
			(3, 1, 'IMPORTANT');

		-- Messages (5 messages across 3 months)
		INSERT INTO messages (id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments) VALUES
			(1, 1, 'msg1', 101, 'Hello World', 'Preview 1', '2024-01-15 10:00:00', 1000, 0),
			(2, 1, 'msg2', 101, 'Re: Hello', 'Preview 2', '2024-01-16 11:00:00', 2000, 1),
			(3, 1, 'msg3', 102, 'Follow up', 'Preview 3', '2024-02-01 09:00:00', 1500, 0),
			(4, 1, 'msg4', 103, 'Question', 'Preview 4', '2024-02-15 14:00:00', 3000, 1),
			(5, 1, 'msg5', 104, 'Final', 'Preview 5', '2024-03-01 16:00:00', 500, 0);

		-- Message recipients
		-- msg1: from alice, to bob+carol
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name) VALUES
			(1, 1, 'from', 'Alice Smith'),
			(1, 2, 'to', 'Bob Jones'),
			(1, 3, 'to', 'Carol White');
		-- msg2: from alice, to bob, cc dan
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name) VALUES
			(2, 1, 'from', 'Alice Smith'),
			(2, 2, 'to', 'Bob Jones'),
			(2, 4, 'cc', 'Dan Brown');
		-- msg3: from alice, to bob
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name) VALUES
			(3, 1, 'from', 'Alice Smith'),
			(3, 2, 'to', 'Bob Jones');
		-- msg4: from bob, to alice
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name) VALUES
			(4, 2, 'from', 'Bob Jones'),
			(4, 1, 'to', 'Alice Smith');
		-- msg5: from bob, to alice
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name) VALUES
			(5, 2, 'from', 'Bob Jones'),
			(5, 1, 'to', 'Alice Smith');

		-- Message labels
		INSERT INTO message_labels (message_id, label_id) VALUES
			(1, 1), (1, 2),  -- msg1: INBOX, Work
			(2, 1), (2, 3),  -- msg2: INBOX, IMPORTANT
			(3, 1),          -- msg3: INBOX
			(4, 1), (4, 2),  -- msg4: INBOX, Work
			(5, 1);          -- msg5: INBOX

		-- Attachments
		INSERT INTO attachments (message_id, filename, mime_type, size) VALUES
			(2, 'document.pdf', 'application/pdf', 10000),
			(2, 'image.png', 'image/png', 5000),
			(4, 'report.xlsx', 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet', 20000);

		-- Conversations
		INSERT INTO conversations (id, source_id, source_conversation_id, title) VALUES
			(101, 1, 'thread101', 'Hello World Thread'),
			(102, 1, 'thread102', 'Follow up Thread'),
			(103, 1, 'thread103', 'Question Thread'),
			(104, 1, 'thread104', 'Final Thread');
	`

	_, err = db.Exec(testData)
	require.NoError(t, err, "insert test data")

	return tmpDir
}

// TestBuildCache_BasicExport tests that buildCache creates all expected Parquet files.
func TestBuildCache_BasicExport(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	result, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "buildCache")

	require.False(result.Skipped, "expected export to run, but was skipped")

	assert.Equal(int64(5), result.ExportedCount, "exported messages")

	// Verify all Parquet directories/files were created
	expectedDirs := []string{
		"messages",
		"sources",
		"participants",
		"message_recipients",
		"labels",
		"message_labels",
		"attachments",
		"conversations",
	}

	for _, dir := range expectedDirs {
		path := filepath.Join(analyticsDir, dir)
		_, err := os.Stat(path)
		assert.False(os.IsNotExist(err), "expected directory %s to exist", dir)
	}

	// Verify sync state was saved
	stateFile := filepath.Join(analyticsDir, "_last_sync.json")
	_, err = os.Stat(stateFile)
	require.False(os.IsNotExist(err), "expected _last_sync.json to exist")

	var state syncState
	data, _ := os.ReadFile(stateFile)
	require.NoError(json.Unmarshal(data, &state), "parse sync state")

	assert.Equal(int64(5), state.LastMessageID)
}

// TestBuildCache_DataIntegrity verifies the exported Parquet data matches SQLite.
func TestBuildCache_DataIntegrity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	_, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "buildCache")

	// Open DuckDB to query the Parquet files
	db, err := sql.Open("duckdb", "")
	require.NoError(err, "open duckdb")
	defer func() { _ = db.Close() }()

	// Helper to count rows in a Parquet file
	countRows := func(pattern string) int64 {
		var count int64
		query := "SELECT COUNT(*) FROM read_parquet('" + pattern + "')"
		require.NoError(db.QueryRow(query).Scan(&count), "count %s", pattern)
		return count
	}

	// Verify row counts
	tests := []struct {
		name     string
		pattern  string
		expected int64
	}{
		{"messages", filepath.Join(analyticsDir, "messages", "**", "*.parquet"), 5},
		{"sources", filepath.Join(analyticsDir, "sources", "*.parquet"), 1},
		{"participants", filepath.Join(analyticsDir, "participants", "*.parquet"), 4},
		{"message_recipients", filepath.Join(analyticsDir, "message_recipients", "*.parquet"), 12},
		{"labels", filepath.Join(analyticsDir, "labels", "*.parquet"), 3},
		{"message_labels", filepath.Join(analyticsDir, "message_labels", "*.parquet"), 8},
		{"attachments", filepath.Join(analyticsDir, "attachments", "*.parquet"), 3},
	}

	for _, tc := range tests {
		count := countRows(tc.pattern)
		assert.Equal(tc.expected, count, "%s row count", tc.name)
	}

	// Verify message data integrity
	var subject string
	msgQuery := "SELECT subject FROM read_parquet('" + filepath.Join(analyticsDir, "messages", "**", "*.parquet") + "') WHERE id = 1"
	require.NoError(db.QueryRow(msgQuery).Scan(&subject), "query message")
	assert.Equal("Hello World", subject)

	// Verify participant data
	var email string
	partQuery := "SELECT email_address FROM read_parquet('" + filepath.Join(analyticsDir, "participants", "*.parquet") + "') WHERE id = 1"
	require.NoError(db.QueryRow(partQuery).Scan(&email), "query participant")
	assert.Equal("alice@example.com", email)

	// Verify attachment sizes
	var totalSize int64
	attQuery := "SELECT SUM(size) FROM read_parquet('" + filepath.Join(analyticsDir, "attachments", "*.parquet") + "')"
	require.NoError(db.QueryRow(attQuery).Scan(&totalSize), "query attachments")
	assert.Equal(int64(35000), totalSize, "expected total attachment size 10000+5000+20000")
}

// TestBuildCache_IncrementalExport tests that incremental exports only add new messages.
func TestBuildCache_IncrementalExport(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// First export
	result1, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "first buildCache")
	assert.Equal(int64(5), result1.ExportedCount, "first export message count")

	// Add new messages to SQLite
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err, "open sqlite")

	_, err = db.Exec(`
		INSERT INTO messages (id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments) VALUES
			(6, 1, 'msg6', 105, 'New Message 1', 'Preview 6', '2024-03-15 10:00:00', 1200, 0),
			(7, 1, 'msg7', 105, 'New Message 2', 'Preview 7', '2024-03-16 11:00:00', 1300, 0);

		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name) VALUES
			(6, 1, 'from', 'Alice Smith'),
			(6, 2, 'to', 'Bob Jones'),
			(7, 2, 'from', 'Bob Jones'),
			(7, 1, 'to', 'Alice Smith');

		INSERT INTO message_labels (message_id, label_id) VALUES
			(6, 1),
			(7, 1);

		INSERT INTO attachments (message_id, filename, mime_type, size) VALUES
			(7, 'notes.txt', 'text/plain', 500);
	`)
	_ = db.Close()
	require.NoError(err, "insert new messages")

	// Second export (incremental)
	result2, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "second buildCache")

	require.False(result2.Skipped, "expected incremental export to run, but was skipped")

	// Verify total count includes both old and new
	assert.Equal(int64(7), result2.ExportedCount, "after incremental: expected 7 total messages")

	// Verify junction tables accumulated across incremental runs
	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err, "open duckdb")
	defer func() { _ = duckdb.Close() }()

	countRows := func(pattern string) int64 {
		var count int64
		// Use forward slashes for DuckDB glob patterns (backslashes fail on Windows)
		pattern = filepath.ToSlash(pattern)
		require.NoError(duckdb.QueryRow("SELECT COUNT(*) FROM read_parquet('"+pattern+"')").Scan(&count), "count %s", pattern)
		return count
	}

	// Messages: 7 total (5 original + 2 new)
	assert.Equal(int64(7), countRows(filepath.Join(analyticsDir, "messages", "**", "*.parquet")), "messages")

	// Message recipients: 16 total (12 original + 4 new)
	assert.Equal(int64(16), countRows(filepath.Join(analyticsDir, "message_recipients", "*.parquet")), "message_recipients")

	// Message labels: 10 total (8 original + 2 new)
	assert.Equal(int64(10), countRows(filepath.Join(analyticsDir, "message_labels", "*.parquet")), "message_labels")

	// Attachments: 4 total (3 original + 1 new)
	assert.Equal(int64(4), countRows(filepath.Join(analyticsDir, "attachments", "*.parquet")), "attachments")

	// Participants: 4 (overwritten each run, not appended)
	assert.Equal(int64(4), countRows(filepath.Join(analyticsDir, "participants", "*.parquet")), "participants")

	// Labels: 3 (overwritten each run)
	assert.Equal(int64(3), countRows(filepath.Join(analyticsDir, "labels", "*.parquet")), "labels")

	// Sources: 1 (overwritten each run)
	assert.Equal(int64(1), countRows(filepath.Join(analyticsDir, "sources", "*.parquet")), "sources")

	// Verify sync state was updated
	var state syncState
	data, _ := os.ReadFile(filepath.Join(analyticsDir, "_last_sync.json"))
	_ = json.Unmarshal(data, &state)

	assert.Equal(int64(7), state.LastMessageID)
}

func TestBuildCache_SnapshotUpperBoundPreventsDuplicateIncrementalRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)
	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	buildCacheAfterSnapshotHook = func() {
		db, err := sql.Open("sqlite3", dbPath)
		require.NoError(err)
		defer func() { require.NoError(db.Close()) }()
		_, err = db.Exec(`
			INSERT INTO messages (id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments)
			VALUES (6, 1, 'msg6', 105, 'Racing Message', 'Preview 6', '2024-03-15 10:00:00', 1200, 1);
			INSERT INTO conversations (id, source_id, source_conversation_id, title)
			VALUES (105, 1, 'thread105', 'Racing Thread');
			INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name)
			VALUES (6, 1, 'from', 'Alice Smith'), (6, 2, 'to', 'Bob Jones');
			INSERT INTO message_labels (message_id, label_id) VALUES (6, 1);
			INSERT INTO attachments (message_id, filename, mime_type, size)
			VALUES (6, 'racing.txt', 'text/plain', 100);
		`)
		require.NoError(err)
	}
	t.Cleanup(func() { buildCacheAfterSnapshotHook = nil })

	first, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err)
	assert.Equal(int64(5), first.MaxMessageID, "state watermark is the captured all-message maximum")
	assert.Equal(int64(5), first.ExportedCount, "first build excludes rows newer than its captured snapshot")

	buildCacheAfterSnapshotHook = nil
	second, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err)
	assert.Equal(int64(6), second.MaxMessageID)
	assert.Equal(int64(6), second.ExportedCount, "next incremental build exports the racing row once")

	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = duckdb.Close() }()

	assertUnique := func(table, idColumn string, id int64) {
		t.Helper()
		var rows, distinctRows int64
		pattern := filepath.ToSlash(filepath.Join(analyticsDir, table, "*.parquet"))
		if table == tableMessages {
			pattern = filepath.ToSlash(filepath.Join(analyticsDir, table, "**", "*.parquet"))
		}
		require.NoError(duckdb.QueryRow(
			"SELECT COUNT(*), COUNT(DISTINCT "+idColumn+") FROM read_parquet(?) WHERE "+idColumn+" = ?",
			pattern, id,
		).Scan(&rows, &distinctRows))
		assert.Equal(int64(1), rows, "%s row count", table)
		assert.Equal(int64(1), distinctRows, "%s distinct row count", table)
	}
	assertUnique(tableMessages, "id", 6)
	assertUnique("message_labels", "message_id", 6)
	assertUnique(tableAttachments, "message_id", 6)
	assertUnique(tableConversations, "id", 105)

	var recipientRows int64
	require.NoError(duckdb.QueryRow(
		`SELECT COUNT(*) FROM read_parquet(?) WHERE message_id = 6`,
		filepath.ToSlash(filepath.Join(analyticsDir, "message_recipients", "*.parquet")),
	).Scan(&recipientRows))
	assert.Equal(int64(2), recipientRows, "recipient junctions exported once")
}

func TestBuildCache_RejectsTerminalAdditionDuringExport(t *testing.T) {
	for _, terminalStatus := range []string{"completed", "failed"} {
		t.Run(terminalStatus, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			tmpDir := setupTestSQLite(t)
			dbPath := filepath.Join(tmpDir, "test.db")
			analyticsDir := filepath.Join(tmpDir, "analytics")
			_, err := buildCache(dbPath, analyticsDir, false)
			require.NoError(err)

			db, err := sql.Open("sqlite3", dbPath)
			require.NoError(err)
			_, err = db.Exec(`
				INSERT INTO conversations (id, source_id, source_conversation_id, title)
				VALUES (105, 1, 'thread105', 'Partial Meeting');
				INSERT INTO messages (
					id, source_id, source_message_id, conversation_id,
					subject, snippet, sent_at, size_estimate, message_type
				) VALUES (
					6, 1, 'meeting-partial', 105,
					'Partial Meeting', 'Parent persisted first',
					'2026-07-12 10:00:00', 500, 'meeting_transcript'
				);
				CREATE TABLE sync_runs (
					id INTEGER PRIMARY KEY,
					source_id INTEGER,
					started_at DATETIME,
					completed_at DATETIME,
					status TEXT,
					messages_processed INTEGER,
					messages_added INTEGER,
					messages_updated INTEGER,
					errors_count INTEGER
				);
				INSERT INTO sync_runs (
					id, source_id, started_at, status,
					messages_processed, messages_added, messages_updated, errors_count
				) VALUES (1, 1, datetime('now'), 'running', 1, 0, 0, 0);
			`)
			require.NoError(err)
			require.NoError(db.Close())

			buildCacheBeforeStateWriteHook = func() {
				hookDB, hookErr := sql.Open("sqlite3", dbPath)
				require.NoError(hookErr)
				defer func() { require.NoError(hookDB.Close()) }()
				_, hookErr = hookDB.Exec(`
					INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name)
					VALUES (6, 1, 'from', 'Alice Smith'), (6, 2, 'to', 'Bob Jones');
					INSERT INTO message_labels (message_id, label_id) VALUES (6, 1);
					INSERT INTO attachments (message_id, filename, mime_type, size)
					VALUES (6, 'late.txt', 'text/plain', 100);
					UPDATE sync_runs
					SET status = ?, completed_at = datetime('now'), messages_added = 1
					WHERE id = 1;
				`, terminalStatus)
				require.NoError(hookErr)
			}
			t.Cleanup(func() { buildCacheBeforeStateWriteHook = nil })

			_, err = buildCache(dbPath, analyticsDir, false)
			require.Error(err)
			assert.Contains(err.Error(), "sync counters changed during cache export")
			buildCacheBeforeStateWriteHook = nil
			_, statErr := os.Stat(analyticsDir)
			assert.True(os.IsNotExist(statErr), "racing incremental output must be discarded")

			_, err = buildCache(dbPath, analyticsDir, false)
			require.NoError(err)
			fresh := cacheNeedsBuild(dbPath, analyticsDir)
			require.False(fresh.NeedsBuild, "retry must capture the terminal addition: %+v", fresh)

			duckdb, err := sql.Open("duckdb", "")
			require.NoError(err)
			defer func() { _ = duckdb.Close() }()
			var recipientRows, attachmentRows int64
			require.NoError(duckdb.QueryRow(
				`SELECT COUNT(*) FROM read_parquet(?) WHERE message_id = 6`,
				filepath.ToSlash(filepath.Join(analyticsDir, "message_recipients", "*.parquet")),
			).Scan(&recipientRows))
			require.NoError(duckdb.QueryRow(
				`SELECT COUNT(*) FROM read_parquet(?) WHERE message_id = 6`,
				filepath.ToSlash(filepath.Join(analyticsDir, "attachments", "*.parquet")),
			).Scan(&attachmentRows))
			assert.Equal(int64(2), recipientRows, "full rebuild captures late recipients")
			assert.Equal(int64(1), attachmentRows, "full rebuild captures late attachment")
		})
	}
}

func TestBuildCache_RejectsZeroCounterFailedRunDuringExport(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)
	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = db.Exec(`
		CREATE TABLE sync_runs (
			id INTEGER PRIMARY KEY,
			source_id INTEGER,
			started_at DATETIME,
			completed_at DATETIME,
			status TEXT,
			messages_processed INTEGER,
			messages_added INTEGER,
			messages_updated INTEGER,
			errors_count INTEGER
		);
		INSERT INTO sync_runs (
			id, source_id, started_at, status,
			messages_processed, messages_added, messages_updated, errors_count
		) VALUES (1, 1, datetime('now'), 'running', 0, 0, 0, 0);
	`)
	require.NoError(err)
	require.NoError(db.Close())

	_, err = buildCache(dbPath, analyticsDir, false)
	require.NoError(err)

	buildCacheBeforeStateWriteHook = func() {
		hookDB, hookErr := sql.Open("sqlite3", dbPath)
		require.NoError(hookErr)
		defer func() { require.NoError(hookDB.Close()) }()
		_, hookErr = hookDB.Exec(`
			UPDATE sync_runs
			SET status = 'failed', completed_at = datetime('now')
			WHERE id = 1
		`)
		require.NoError(hookErr)
	}
	t.Cleanup(func() { buildCacheBeforeStateWriteHook = nil })

	_, err = buildCache(dbPath, analyticsDir, true)
	require.Error(err)
	assert.Contains(err.Error(), "sync counters changed during cache export")
	buildCacheBeforeStateWriteHook = nil
	_, statErr := os.Stat(analyticsDir)
	assert.True(os.IsNotExist(statErr), "racing full rebuild output must be discarded")
}

func TestCacheNeedsBuild_DetectsOlderRunFailingAfterNewerFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)
	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = db.Exec(`
		CREATE TABLE sync_runs (
			id INTEGER PRIMARY KEY,
			source_id INTEGER,
			started_at DATETIME,
			completed_at DATETIME,
			status TEXT,
			messages_processed INTEGER,
			messages_added INTEGER,
			messages_updated INTEGER,
			errors_count INTEGER
		);
		INSERT INTO sync_runs (
			id, source_id, started_at, completed_at, status,
			messages_processed, messages_added, messages_updated, errors_count
		) VALUES
			(1, 1, datetime('now'), NULL, 'running', 0, 0, 0, 0),
			(2, 1, datetime('now'), datetime('now'), 'failed', 0, 0, 0, 0);
	`)
	require.NoError(err)
	require.NoError(db.Close())

	_, err = buildCache(dbPath, analyticsDir, false)
	require.NoError(err)

	db, err = sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = db.Exec(`
		UPDATE sync_runs
		SET status = 'failed', completed_at = datetime('now')
		WHERE id = 1
	`)
	require.NoError(err)
	require.NoError(db.Close())

	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	assert.True(staleness.NeedsBuild, "every newly failed run must invalidate cache: %+v", staleness)
	assert.True(staleness.FullRebuild, "failed-run progress requires a full rebuild: %+v", staleness)
	assert.Contains(staleness.Reason, "failed sync")
}

func TestBuildCache_RejectsOlderRunFailingDuringExport(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)
	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = db.Exec(`
		CREATE TABLE sync_runs (
			id INTEGER PRIMARY KEY,
			source_id INTEGER,
			started_at DATETIME,
			completed_at DATETIME,
			status TEXT,
			messages_processed INTEGER,
			messages_added INTEGER,
			messages_updated INTEGER,
			errors_count INTEGER
		);
		INSERT INTO sync_runs (
			id, source_id, started_at, completed_at, status,
			messages_processed, messages_added, messages_updated, errors_count
		) VALUES
			(1, 1, datetime('now'), NULL, 'running', 0, 0, 0, 0),
			(2, 1, datetime('now'), datetime('now'), 'failed', 0, 0, 0, 0);
	`)
	require.NoError(err)
	require.NoError(db.Close())

	_, err = buildCache(dbPath, analyticsDir, false)
	require.NoError(err)

	buildCacheBeforeStateWriteHook = func() {
		hookDB, hookErr := sql.Open("sqlite3", dbPath)
		require.NoError(hookErr)
		defer func() { require.NoError(hookDB.Close()) }()
		_, hookErr = hookDB.Exec(`
			UPDATE sync_runs
			SET status = 'failed', completed_at = datetime('now')
			WHERE id = 1
		`)
		require.NoError(hookErr)
	}
	t.Cleanup(func() { buildCacheBeforeStateWriteHook = nil })

	_, err = buildCache(dbPath, analyticsDir, true)
	require.Error(err)
	assert.Contains(err.Error(), "sync counters changed during cache export")
}

func TestCacheNeedsBuild_AddOnlySyncUsesIncrementalBuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)
	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	_, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err)

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	defer func() { require.NoError(db.Close()) }()
	_, err = db.Exec(`
		CREATE TABLE sync_runs (
			id INTEGER PRIMARY KEY,
			source_id INTEGER,
			started_at DATETIME,
			completed_at DATETIME,
			status TEXT,
			messages_processed INTEGER,
			messages_added INTEGER,
			messages_updated INTEGER,
			errors_count INTEGER
		);
		INSERT INTO conversations (id, source_id, source_conversation_id, title)
		VALUES (105, 1, 'thread105', 'New Thread');
		INSERT INTO messages (
			id, source_id, source_message_id, conversation_id,
			subject, snippet, sent_at, size_estimate, message_type
		) VALUES (
			6, 1, 'msg6', 105, 'New Message', 'Preview',
			'2026-07-12 10:00:00', 500, 'email'
		);
		INSERT INTO sync_runs (
			id, source_id, started_at, completed_at, status,
			messages_processed, messages_added, messages_updated, errors_count
		) VALUES (1, 1, datetime('now'), datetime('now'), 'completed', 1, 1, 0, 0);
	`)
	require.NoError(err)

	got := cacheNeedsBuild(dbPath, analyticsDir)
	assert.True(got.NeedsBuild, "new message must invalidate cache: %+v", got)
	assert.True(got.HasNew, "new message must use the ID boundary: %+v", got)
	assert.False(got.FullRebuild, "append-only sync must remain incremental: %+v", got)
}

// TestBuildCache_SkipsWhenNoNewMessages tests that export is skipped when no new messages.
func TestBuildCache_SkipsWhenNoNewMessages(t *testing.T) {
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// First export
	_, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(t, err, "first buildCache")

	// Second export without any new data
	result, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(t, err, "second buildCache")

	assert.True(t, result.Skipped, "expected export to be skipped when no new messages")
}

// TestBuildCache_BackfillsMissingConversations tests that an older cache missing
// the conversations parquet table triggers a rebuild even when no new messages
// exist. This simulates the upgrade path from a cache that predates the
// conversations export.
func TestBuildCache_BackfillsMissingConversations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// First export — creates all tables including conversations.
	result1, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "first buildCache")
	require.False(result1.Skipped, "expected first export to run")

	// Simulate a legacy cache by removing the conversations directory.
	conversationsDir := filepath.Join(analyticsDir, "conversations")
	require.NoError(os.RemoveAll(conversationsDir), "remove conversations dir")

	// Verify the conversations dir is actually gone.
	_, err = os.Stat(conversationsDir)
	require.True(os.IsNotExist(err), "expected conversations dir to be removed")

	// Second export — no new messages, but conversations parquet is missing.
	// buildCache must NOT skip; it should backfill the missing table.
	result2, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "second buildCache")

	require.False(result2.Skipped, "expected backfill rebuild when conversations parquet is missing, but was skipped")

	// Verify conversations parquet was recreated.
	pattern := filepath.Join(conversationsDir, "*.parquet")
	matches, _ := filepath.Glob(pattern)
	assert.NotEmpty(matches, "expected conversations parquet files to be recreated after backfill")

	// Verify conversation data is correct.
	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err, "open duckdb")
	defer func() { _ = duckdb.Close() }()

	var count int64
	q := "SELECT COUNT(*) FROM read_parquet('" + filepath.Join(conversationsDir, "*.parquet") + "')"
	require.NoError(duckdb.QueryRow(q).Scan(&count), "count conversations")
	assert.Equal(int64(4), count, "expected 4 conversations after backfill")

	// Third export — everything is up-to-date, should skip.
	result3, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "third buildCache")
	assert.True(result3.Skipped, "expected third export to be skipped (all tables present, no new messages)")
}

// TestBuildCache_BackfillAfterIncrementalNoDuplicates tests the scenario:
// full export → add data → incremental export → remove a required table → backfill.
// This verifies that stale incr_*.parquet shards from prior incremental runs
// are cleaned up during backfill, preventing duplicate rows.
func TestBuildCache_BackfillAfterIncrementalNoDuplicates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Step 1: Initial full export (5 messages, 12 recipients).
	result1, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "first buildCache")
	require.Equal(int64(5), result1.ExportedCount, "expected 5 messages in initial export")

	// Step 2: Add new messages to SQLite, then incremental export.
	// This creates incr_*.parquet files alongside data.parquet.
	sqliteDB, err := sql.Open("sqlite3", dbPath)
	require.NoError(err, "open sqlite")
	_, err = sqliteDB.Exec(`
		INSERT INTO messages (id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments) VALUES
			(6, 1, 'msg6', 101, 'Incremental 1', 'Preview 6', '2024-03-15 10:00:00', 1200, 0),
			(7, 1, 'msg7', 102, 'Incremental 2', 'Preview 7', '2024-03-16 11:00:00', 1300, 0);
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name) VALUES
			(6, 1, 'from', 'Alice Smith'),
			(6, 2, 'to', 'Bob Jones'),
			(7, 2, 'from', 'Bob Jones'),
			(7, 1, 'to', 'Alice Smith');
		INSERT INTO message_labels (message_id, label_id) VALUES (6, 1), (7, 1);
	`)
	_ = sqliteDB.Close()
	require.NoError(err, "insert incremental data")

	result2, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "second buildCache (incremental)")
	require.Equal(int64(7), result2.ExportedCount, "expected 7 messages after incremental")

	// Step 3: Remove conversations dir (simulate legacy cache missing a table).
	conversationsDir := filepath.Join(analyticsDir, "conversations")
	require.NoError(os.RemoveAll(conversationsDir), "remove conversations dir")

	// Step 4: Backfill — no new messages, but conversations is missing.
	// This must do a full rebuild, clearing stale incremental shards.
	result3, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "third buildCache (backfill)")
	require.False(result3.Skipped, "expected backfill, but was skipped")

	// Step 5: Verify exact counts — no duplicates from stale incr_*.parquet.
	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err, "open duckdb")
	defer func() { _ = duckdb.Close() }()

	countRows := func(pattern string) int64 {
		var count int64
		pattern = filepath.ToSlash(pattern)
		require.NoError(duckdb.QueryRow("SELECT COUNT(*) FROM read_parquet('"+pattern+"')").Scan(&count), "count %s", pattern)
		return count
	}

	// Expected: 7 messages (5 original + 2 incremental), NOT 12 (5+2+5 from dup)
	assert.Equal(int64(7), countRows(filepath.Join(analyticsDir, "messages", "**", "*.parquet")),
		"messages: possible duplicate from stale incremental shards")
	// Expected: 16 recipients (12 original + 4 incremental), NOT 28
	assert.Equal(int64(16), countRows(filepath.Join(analyticsDir, "message_recipients", "*.parquet")), "message_recipients")
	// Expected: 10 message_labels (8 original + 2 incremental), NOT 18
	assert.Equal(int64(10), countRows(filepath.Join(analyticsDir, "message_labels", "*.parquet")), "message_labels")
	// Expected: 3 attachments (no new ones added), NOT 6
	assert.Equal(int64(3), countRows(filepath.Join(analyticsDir, "attachments", "*.parquet")), "attachments")
	// Conversations should be restored.
	assert.Equal(int64(4), countRows(filepath.Join(analyticsDir, "conversations", "*.parquet")), "conversations")
}

// TestBuildCache_BackfillWithNewMessages tests that when a required table is
// missing AND new messages exist, the build does a full rebuild (not incremental).
// Without this, the code would stay in incremental mode and only export new
// message_recipients, leaving historical rows missing from the rebuilt table.
func TestBuildCache_BackfillWithNewMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Step 1: Full export (5 messages, 12 recipients).
	_, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "first buildCache")

	// Step 2: Delete message_recipients dir (simulate missing table).
	recipientsDir := filepath.Join(analyticsDir, "message_recipients")
	require.NoError(os.RemoveAll(recipientsDir), "remove message_recipients dir")

	// Step 3: Add new messages to SQLite (so maxID > lastMessageID).
	sqliteDB, err := sql.Open("sqlite3", dbPath)
	require.NoError(err, "open sqlite")
	_, err = sqliteDB.Exec(`
		INSERT INTO messages (id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments) VALUES
			(6, 1, 'msg6', 101, 'New msg', 'Preview 6', '2024-03-15 10:00:00', 1200, 0);
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name) VALUES
			(6, 1, 'from', 'Alice Smith'),
			(6, 2, 'to', 'Bob Jones');
	`)
	_ = sqliteDB.Close()
	require.NoError(err, "insert new data")

	// Step 4: Build — missing table + new messages should force full rebuild.
	result, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "second buildCache")
	require.False(result.Skipped, "expected rebuild, but was skipped")

	// Step 5: Verify ALL recipients present (12 original + 2 new = 14).
	// If only incremental ran, we'd see just 2 (new message's recipients).
	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err, "open duckdb")
	defer func() { _ = duckdb.Close() }()

	var count int64
	q := "SELECT COUNT(*) FROM read_parquet('" + filepath.ToSlash(filepath.Join(recipientsDir, "*.parquet")) + "')"
	require.NoError(duckdb.QueryRow(q).Scan(&count), "count message_recipients")
	assert.Equal(int64(14), count, "message_recipients: expected 14 (12 original + 2 new)")

	// Also verify messages count is correct (6 total, no duplicates).
	var msgCount int64
	msgQ := "SELECT COUNT(*) FROM read_parquet('" + filepath.ToSlash(filepath.Join(analyticsDir, "messages", "**", "*.parquet")) + "', hive_partitioning=true)"
	require.NoError(duckdb.QueryRow(msgQ).Scan(&msgCount), "count messages")
	assert.Equal(int64(6), msgCount, "messages")
}

// TestBuildCache_BackfillMissingMessages tests that when the messages parquet
// directory is missing but other parquet tables exist (e.g. participants),
// the cache is detected as broken and rebuilt. This covers an edge case where
// HasParquetData (messages-only) would return false, causing missingRequiredParquet
// to return false and skip the rebuild.
func TestBuildCache_BackfillMissingMessages(t *testing.T) {
	require := require.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Step 1: Full export to create all tables.
	result1, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "first buildCache")
	require.False(result1.Skipped, "expected first export to run")

	// Step 2: Remove the messages directory (simulate corruption/partial failure).
	messagesDir := filepath.Join(analyticsDir, "messages")
	require.NoError(os.RemoveAll(messagesDir), "remove messages dir")

	// Verify other parquet tables still exist (e.g. participants).
	participantsPattern := filepath.Join(analyticsDir, "participants", "*.parquet")
	matches, _ := filepath.Glob(participantsPattern)
	require.NotEmpty(matches, "expected participants parquet to still exist")

	// Step 3: Build again — messages are missing but other tables exist.
	// Must detect the broken cache and rebuild, NOT skip.
	result2, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "second buildCache")
	require.False(result2.Skipped, "expected rebuild when messages parquet is missing but other tables exist")

	// Verify messages were restored.
	duckdb, err := sql.Open("duckdb", "")
	require.NoError(err, "open duckdb")
	defer func() { _ = duckdb.Close() }()

	var count int64
	q := "SELECT COUNT(*) FROM read_parquet('" + filepath.ToSlash(filepath.Join(messagesDir, "**", "*.parquet")) + "', hive_partitioning=true)"
	require.NoError(duckdb.QueryRow(q).Scan(&count), "count messages")
	assert.Equal(t, int64(5), count, "messages")
}

// TestBuildCache_FullRebuild tests that --full-rebuild clears and recreates cache.
func TestBuildCache_FullRebuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// First export
	_, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "first buildCache")

	// Create a marker file to verify directory is cleared
	markerFile := filepath.Join(analyticsDir, "messages", "marker.txt")
	_ = os.WriteFile(markerFile, []byte("test"), 0644)

	// Full rebuild
	result, err := buildCache(dbPath, analyticsDir, true)
	require.NoError(err, "full rebuild")

	require.False(result.Skipped, "full rebuild should not be skipped")

	// Verify marker file was removed
	_, err = os.Stat(markerFile)
	assert.True(os.IsNotExist(err), "expected marker file to be removed during full rebuild")

	// Verify data was exported
	assert.Equal(int64(5), result.ExportedCount, "expected 5 messages after full rebuild")
}

// TestBuildCache_DeletedMessagesIncluded tests that deleted messages are exported.
func TestBuildCache_DeletedMessagesIncluded(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Mark one message as deleted
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err, "open sqlite")
	_, err = db.Exec("UPDATE messages SET deleted_from_source_at = '2024-06-01 12:00:00' WHERE id = 3")
	_ = db.Close()
	require.NoError(err, "mark deleted")

	// Export
	result, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "buildCache")

	// All 5 messages should be exported (including deleted)
	assert.Equal(int64(5), result.ExportedCount, "expected 5 messages (including deleted)")

	// Verify deleted_from_source_at is preserved
	duckdb, _ := sql.Open("duckdb", "")
	defer func() { _ = duckdb.Close() }()

	var deletedCount int64
	query := "SELECT COUNT(*) FROM read_parquet('" + filepath.Join(analyticsDir, "messages", "**", "*.parquet") + "') WHERE deleted_from_source_at IS NOT NULL"
	require.NoError(duckdb.QueryRow(query).Scan(&deletedCount), "query deleted")

	assert.Equal(int64(1), deletedCount, "expected 1 deleted message in Parquet")
}

// TestBuildCache_MessagesWithoutSentAt tests that messages without sent_at are excluded.
func TestBuildCache_MessagesWithoutSentAt(t *testing.T) {
	require := require.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Add a message without sent_at
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err, "open sqlite")
	_, err = db.Exec(`
		INSERT INTO messages (id, source_id, source_message_id, subject, snippet, size_estimate)
		VALUES (6, 1, 'msg6', 'No Date', 'Preview', 100)
	`)
	_ = db.Close()
	require.NoError(err, "insert")

	result, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "buildCache")

	// Only 5 messages with sent_at should be exported
	assert.Equal(t, int64(5), result.ExportedCount, "expected 5 messages (excluding null sent_at)")
}

// TestBuildCache_EndToEndWithQueryEngine tests the full flow with query engine.
func TestBuildCache_EndToEndWithQueryEngine(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Build cache
	_, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "buildCache")

	// Open DuckDB and test queries that match what the TUI does
	db, err := sql.Open("duckdb", "")
	require.NoError(err, "open duckdb")
	defer func() { _ = db.Close() }()

	// Build the CTEs like the query engine does
	ctes := `
		WITH
		msg AS (SELECT * FROM read_parquet('` + filepath.Join(analyticsDir, "messages", "**", "*.parquet") + `', hive_partitioning=true)),
		mr AS (SELECT * FROM read_parquet('` + filepath.Join(analyticsDir, "message_recipients", "*.parquet") + `')),
		p AS (SELECT * FROM read_parquet('` + filepath.Join(analyticsDir, "participants", "*.parquet") + `')),
		lbl AS (SELECT * FROM read_parquet('` + filepath.Join(analyticsDir, "labels", "*.parquet") + `')),
		ml AS (SELECT * FROM read_parquet('` + filepath.Join(analyticsDir, "message_labels", "*.parquet") + `')),
		att AS (SELECT message_id, SUM(size) as attachment_size, COUNT(*) as attachment_count FROM read_parquet('` + filepath.Join(analyticsDir, "attachments", "*.parquet") + `') GROUP BY message_id)
	`

	// Test 1: Aggregate by sender (like AggregateBySender)
	senderQuery := ctes + `
		SELECT p.email_address as key, COUNT(*) as count
		FROM msg
		JOIN mr ON mr.message_id = msg.id AND mr.recipient_type = 'from'
		JOIN p ON p.id = mr.participant_id
		GROUP BY p.email_address
		ORDER BY count DESC
	`
	// queryCounts runs a key/count aggregate and returns the map, closing the
	// cursor before it returns. A deferred close in this scoped helper keeps
	// sqlclosecheck satisfied without leaking rows across the sequential
	// queries below (which reuse the same connection).
	queryCounts := func(label, query string) map[string]int64 {
		rows, err := db.Query(query)
		require.NoError(err, label+" query")
		defer func() { _ = rows.Close() }()
		counts := make(map[string]int64)
		for rows.Next() {
			var key string
			var count int64
			_ = rows.Scan(&key, &count)
			counts[key] = count
		}
		require.NoError(rows.Err(), label+" rows")
		return counts
	}

	senderCounts := queryCounts("sender", senderQuery)

	assert.Equal(int64(3), senderCounts["alice@example.com"], "alice sent count")
	assert.Equal(int64(2), senderCounts["bob@company.org"], "bob sent count")

	// Test 2: Aggregate by label (like AggregateByLabel)
	labelQuery := ctes + `
		SELECT lbl.name as key, COUNT(*) as count
		FROM msg
		JOIN ml ON ml.message_id = msg.id
		JOIN lbl ON lbl.id = ml.label_id
		GROUP BY lbl.name
		ORDER BY count DESC
	`
	labelCounts := queryCounts("label", labelQuery)

	assert.Equal(int64(5), labelCounts["INBOX"], "INBOX count")
	assert.Equal(int64(2), labelCounts["Work"], "Work count")

	// Test 3: Total stats (like GetTotalStats)
	statsQuery := ctes + `
		SELECT
			COUNT(*) as message_count,
			COALESCE(SUM(msg.size_estimate), 0) as total_size,
			COALESCE(SUM(att.attachment_count), 0) as attachment_count,
			COALESCE(SUM(att.attachment_size), 0) as attachment_size
		FROM msg
		LEFT JOIN att ON att.message_id = msg.id
	`
	var msgCount, totalSize, attCount, attSize int64
	require.NoError(db.QueryRow(statsQuery).Scan(&msgCount, &totalSize, &attCount, &attSize), "stats query")

	assert.Equal(int64(5), msgCount, "message count")
	assert.Equal(int64(8000), totalSize, "total size = 1000+2000+1500+3000+500")
	assert.Equal(int64(3), attCount, "attachment count")
	assert.Equal(int64(35000), attSize, "attachment size = 10000+5000+20000")
}

// TestBuildCache_YearPartitioning tests that messages are partitioned by year.
func TestBuildCache_YearPartitioning(t *testing.T) {
	require := require.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Add messages from different years
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err, "open sqlite")
	_, err = db.Exec(`
		INSERT INTO messages (id, source_id, source_message_id, subject, sent_at, size_estimate) VALUES
			(6, 1, 'msg6', 'Old Message', '2020-06-15 10:00:00', 100),
			(7, 1, 'msg7', 'Recent Message', '2025-01-15 10:00:00', 100);
	`)
	_ = db.Close()
	require.NoError(err, "insert")

	_, err = buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "buildCache")

	// Check for year partitions
	years := []string{"2020", "2024", "2025"}
	for _, year := range years {
		pattern := filepath.Join(analyticsDir, "messages", "year="+year, "*.parquet")
		matches, _ := filepath.Glob(pattern)
		assert.NotEmpty(t, matches, "expected partition for year=%s", year)
	}
}

// TestBuildCache_UTF8Handling tests that invalid UTF-8 is handled gracefully.
func TestBuildCache_UTF8Handling(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Insert data with potentially problematic characters
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err, "open sqlite")
	// Note: SQLite3 driver may sanitize, but we test the flow
	_, err = db.Exec(`
		UPDATE messages SET subject = 'Test émoji 🎉 and unicode' WHERE id = 1;
		UPDATE participants SET display_name = 'Müller' WHERE id = 1;
	`)
	_ = db.Close()
	require.NoError(err, "update")

	// Should not error
	result, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "buildCache with unicode")

	assert.Equal(int64(5), result.ExportedCount)

	// Verify data is readable
	duckdb, _ := sql.Open("duckdb", "")
	defer func() { _ = duckdb.Close() }()

	var subject string
	query := "SELECT subject FROM read_parquet('" + filepath.Join(analyticsDir, "messages", "**", "*.parquet") + "') WHERE id = 1"
	require.NoError(duckdb.QueryRow(query).Scan(&subject), "read unicode subject")

	assert.Equal("Test émoji 🎉 and unicode", subject, "unicode should be preserved")
}

// TestBuildCache_EmptyDatabase tests handling of empty database.
func TestBuildCache_EmptyDatabase(t *testing.T) {
	tmpDir := t.TempDir()

	dbPath := filepath.Join(tmpDir, "empty.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Create empty database with schema
	db, _ := sql.Open("sqlite3", dbPath)
	_, _ = db.Exec(`
		CREATE TABLE sources (id INTEGER PRIMARY KEY, identifier TEXT);
		CREATE TABLE messages (id INTEGER PRIMARY KEY, source_id INTEGER, source_message_id TEXT, sent_at TIMESTAMP, size_estimate INTEGER, has_attachments BOOLEAN, subject TEXT, snippet TEXT, conversation_id INTEGER, deleted_from_source_at TIMESTAMP, attachment_count INTEGER DEFAULT 0, sender_id INTEGER, message_type TEXT NOT NULL DEFAULT 'email', deleted_at DATETIME);
		CREATE TABLE participants (id INTEGER PRIMARY KEY, email_address TEXT, domain TEXT, display_name TEXT, phone_number TEXT);
		CREATE TABLE message_recipients (message_id INTEGER, participant_id INTEGER, recipient_type TEXT, display_name TEXT);
		CREATE TABLE labels (id INTEGER PRIMARY KEY, name TEXT);
		CREATE TABLE message_labels (message_id INTEGER, label_id INTEGER);
		CREATE TABLE attachments (message_id INTEGER, size INTEGER, filename TEXT);
		CREATE TABLE conversations (id INTEGER PRIMARY KEY, source_conversation_id TEXT, title TEXT, conversation_type TEXT NOT NULL DEFAULT 'email');
	`)
	_ = db.Close()

	result, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(t, err, "buildCache on empty db")

	// Should be skipped (no messages)
	assert.True(t, result.Skipped, "expected empty database export to be skipped")
}

// TestCSVFallbackPath exercises the Windows-style CSV intermediate path:
// SQLite → CSV → DuckDB views → COPY to Parquet.
// This runs on all platforms to ensure the fallback logic works correctly.
func TestCSVFallbackPath(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	csvDir := filepath.Join(tmpDir, "csv")
	require.NoError(os.MkdirAll(csvDir, 0755), "create csv dir")

	// 1. Export tables to CSV (same as setupSQLiteSource Windows path)
	sqliteDB, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	require.NoError(err, "open sqlite")

	tables := []struct {
		name          string
		query         string
		typeOverrides string
	}{
		{"messages", "SELECT id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments, attachment_count, deleted_from_source_at, deleted_at, sender_id, message_type FROM messages WHERE sent_at IS NOT NULL",
			"types={'sent_at': 'TIMESTAMP', 'deleted_from_source_at': 'TIMESTAMP', 'deleted_at': 'TIMESTAMP'}"},
		{"message_recipients", "SELECT message_id, participant_id, recipient_type, display_name FROM message_recipients", ""},
		{"message_labels", "SELECT message_id, label_id FROM message_labels", ""},
		{"attachments", "SELECT message_id, size, filename FROM attachments", ""},
		{"participants", "SELECT id, email_address, domain, display_name FROM participants", ""},
		{"labels", "SELECT id, name FROM labels", ""},
		{"sources", "SELECT id, identifier FROM sources", ""},
		{"conversations", "SELECT id, source_conversation_id FROM conversations", ""},
	}

	for _, tbl := range tables {
		csvPath := filepath.Join(csvDir, tbl.name+".csv")
		if err := exportToCSV(sqliteDB, tbl.query, csvPath); err != nil {
			_ = sqliteDB.Close()
			require.NoError(err, "exportToCSV %s", tbl.name)
		}
	}
	_ = sqliteDB.Close()

	// 2. Open DuckDB and create views (same as setupSQLiteSource)
	duckDB, err := sql.Open("duckdb", "")
	require.NoError(err, "open duckdb")
	defer func() { _ = duckDB.Close() }()

	_, err = duckDB.Exec("CREATE SCHEMA sqlite_db")
	require.NoError(err, "create schema")

	for _, tbl := range tables {
		csvPath := filepath.Join(csvDir, tbl.name+".csv")
		escaped := strings.ReplaceAll(csvPath, "\\", "/")
		escaped = strings.ReplaceAll(escaped, "'", "''")
		csvOpts := "header=true, nullstr='\\N'"
		if tbl.typeOverrides != "" {
			csvOpts += ", " + tbl.typeOverrides
		}
		viewSQL := fmt.Sprintf(
			`CREATE VIEW sqlite_db."%s" AS SELECT * FROM read_csv_auto('%s', %s)`,
			tbl.name, escaped, csvOpts,
		)
		_, err := duckDB.Exec(viewSQL)
		require.NoError(err, "create view %s", tbl.name)
	}

	// 3. Verify sent_at is correctly typed as TIMESTAMP
	var year int
	err = duckDB.QueryRow(`SELECT CAST(EXTRACT(YEAR FROM sent_at) AS INTEGER) FROM sqlite_db.messages WHERE id = 1`).Scan(&year)
	require.NoError(err, "EXTRACT(YEAR FROM sent_at) failed — sent_at may not be typed as TIMESTAMP")
	assert.Equal(2024, year)

	// 4. Verify NULLs round-trip correctly (deleted_from_source_at should be NULL)
	var deletedAt sql.NullTime
	err = duckDB.QueryRow(`SELECT deleted_from_source_at FROM sqlite_db.messages WHERE id = 1`).Scan(&deletedAt)
	require.NoError(err, "query deleted_from_source_at")
	assert.False(deletedAt.Valid, "expected deleted_from_source_at to be NULL, got %v", deletedAt.Time)

	// 5. Verify row counts match expectations
	counts := map[string]int64{
		"messages":           5,
		"message_recipients": 12,
		"message_labels":     8,
		"attachments":        3,
		"participants":       4,
		"labels":             3,
		"sources":            1,
		"conversations":      4,
	}
	for tbl, expected := range counts {
		var count int64
		require.NoError(duckDB.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM sqlite_db."%s"`, tbl)).Scan(&count), "count %s", tbl)
		assert.Equal(expected, count, "sqlite_db.%s row count", tbl)
	}

	// 6. Verify the full buildCache pipeline works via CSV views
	// Run the same COPY query that buildCache uses for messages
	analyticsDir := filepath.Join(tmpDir, "analytics")
	messagesDir := filepath.Join(analyticsDir, "messages")
	require.NoError(os.MkdirAll(messagesDir, 0755), "create analytics dir")
	escapedDir := strings.ReplaceAll(messagesDir, "\\", "/")
	escapedDir = strings.ReplaceAll(escapedDir, "'", "''")

	copySQL := fmt.Sprintf(`
		COPY (
			SELECT
				m.id,
				m.source_id,
				m.source_message_id,
				m.conversation_id,
				m.subject,
				m.snippet,
				m.sent_at,
				m.size_estimate,
				m.has_attachments,
				m.deleted_from_source_at,
				CAST(EXTRACT(YEAR FROM m.sent_at) AS INTEGER) as year,
				CAST(EXTRACT(MONTH FROM m.sent_at) AS INTEGER) as month
			FROM sqlite_db.messages m
			WHERE m.sent_at IS NOT NULL
		) TO '%s' (
			FORMAT PARQUET,
			PARTITION_BY (year),
			OVERWRITE_OR_IGNORE,
			COMPRESSION 'zstd'
		)
	`, escapedDir)

	_, err = duckDB.Exec(copySQL)
	require.NoError(err, "COPY messages to Parquet via CSV views failed")

	// Verify Parquet files were created with correct year partitions
	for _, y := range []string{"2024"} {
		pattern := filepath.Join(messagesDir, "year="+y, "*.parquet")
		matches, _ := filepath.Glob(pattern)
		assert.NotEmpty(matches, "expected Parquet partition for year=%s", y)
	}
}

// BenchmarkBuildCache benchmarks the export performance.
func BenchmarkBuildCache(b *testing.B) {
	// Create a larger test dataset
	tmpDir := b.TempDir()

	dbPath := filepath.Join(tmpDir, "bench.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	db, _ := sql.Open("sqlite3", dbPath)

	// Create schema
	_, _ = db.Exec(`
		CREATE TABLE sources (id INTEGER PRIMARY KEY, identifier TEXT);
		CREATE TABLE messages (id INTEGER PRIMARY KEY, source_id INTEGER, source_message_id TEXT, sent_at TIMESTAMP, size_estimate INTEGER, has_attachments BOOLEAN, subject TEXT, snippet TEXT, conversation_id INTEGER, deleted_from_source_at TIMESTAMP, attachment_count INTEGER DEFAULT 0, sender_id INTEGER, message_type TEXT NOT NULL DEFAULT 'email', deleted_at DATETIME);
		CREATE TABLE participants (id INTEGER PRIMARY KEY, email_address TEXT UNIQUE, domain TEXT, display_name TEXT, phone_number TEXT);
		CREATE TABLE message_recipients (message_id INTEGER, participant_id INTEGER, recipient_type TEXT, display_name TEXT);
		CREATE TABLE labels (id INTEGER PRIMARY KEY, name TEXT);
		CREATE TABLE message_labels (message_id INTEGER, label_id INTEGER);
		CREATE TABLE attachments (message_id INTEGER, size INTEGER, filename TEXT);
		CREATE TABLE conversations (id INTEGER PRIMARY KEY, source_conversation_id TEXT, title TEXT, conversation_type TEXT NOT NULL DEFAULT 'email');
		INSERT INTO sources VALUES (1, 'test@gmail.com');
		INSERT INTO labels VALUES (1, 'INBOX'), (2, 'Work');
	`)

	// Insert conversations to match messages
	for i := 1; i <= 100; i++ {
		_, _ = db.Exec("INSERT INTO conversations VALUES (?, ?)", i, "thread"+string(rune('0'+i%10)))
	}

	// Insert 1000 participants
	for i := 1; i <= 1000; i++ {
		_, _ = db.Exec("INSERT INTO participants VALUES (?, ?, ?, ?)",
			i, "user"+string(rune('0'+i%10))+"@domain"+string(rune('0'+i%5))+".com",
			"domain"+string(rune('0'+i%5))+".com", "User "+string(rune('0'+i%10)))
	}

	// Insert 10000 messages
	baseTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 10000; i++ {
		sentAt := baseTime.Add(time.Duration(i) * time.Hour)
		_, _ = db.Exec("INSERT INTO messages VALUES (?, 1, ?, ?, ?, 0, ?, ?, ?, NULL)",
			i, "msg"+string(rune('0'+i%10)), sentAt, 1000+i%5000,
			"Subject "+string(rune('0'+i%10)), "Snippet", i%100+1)

		// Add sender and recipient
		_, _ = db.Exec("INSERT INTO message_recipients VALUES (?, ?, 'from', NULL)", i, i%1000+1)
		_, _ = db.Exec("INSERT INTO message_recipients VALUES (?, ?, 'to', NULL)", i, (i+1)%1000+1)

		// Add labels
		_, _ = db.Exec("INSERT INTO message_labels VALUES (?, 1)", i)
		if i%3 == 0 {
			_, _ = db.Exec("INSERT INTO message_labels VALUES (?, 2)", i)
		}
	}
	_ = db.Close()

	b.ResetTimer()
	for range b.N {
		// Clear analytics dir between runs
		_ = os.RemoveAll(analyticsDir)
		if _, err := buildCache(dbPath, analyticsDir, true); err != nil {
			b.Fatalf("buildCache: %v", err)
		}
	}
}

// setupTestSQLiteEmpty creates a test SQLite database with schema and metadata
// (sources, labels, participants) but zero messages. This simulates a freshly
// initialized account that has been synced but has no exportable messages.
func setupTestSQLiteEmpty(t *testing.T) string {
	t.Helper()

	tmpDir := t.TempDir()

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open sqlite")
	defer func() { _ = db.Close() }()

	schema := `
		CREATE TABLE sources (
			id INTEGER PRIMARY KEY,
			source_type TEXT NOT NULL DEFAULT 'gmail',
			identifier TEXT NOT NULL UNIQUE,
			display_name TEXT
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL REFERENCES sources(id),
			source_message_id TEXT NOT NULL,
			conversation_id INTEGER,
			subject TEXT,
			snippet TEXT,
			sent_at TIMESTAMP,
			received_at TIMESTAMP,
			size_estimate INTEGER,
			has_attachments BOOLEAN DEFAULT FALSE,
			attachment_count INTEGER DEFAULT 0,
			deleted_from_source_at TIMESTAMP,
			sender_id INTEGER,
			message_type TEXT NOT NULL DEFAULT 'email',
			deleted_at DATETIME,
			UNIQUE(source_id, source_message_id)
		);
		CREATE TABLE participants (
			id INTEGER PRIMARY KEY,
			email_address TEXT NOT NULL UNIQUE,
			domain TEXT,
			display_name TEXT,
			phone_number TEXT
		);
		CREATE TABLE message_recipients (
			id INTEGER PRIMARY KEY,
			message_id INTEGER NOT NULL REFERENCES messages(id),
			participant_id INTEGER NOT NULL REFERENCES participants(id),
			recipient_type TEXT NOT NULL,
			display_name TEXT
		);
		CREATE TABLE labels (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL REFERENCES sources(id),
			source_label_id TEXT,
			name TEXT NOT NULL,
			label_type TEXT
		);
		CREATE TABLE message_labels (
			message_id INTEGER NOT NULL REFERENCES messages(id),
			label_id INTEGER NOT NULL REFERENCES labels(id),
			PRIMARY KEY (message_id, label_id)
		);
		CREATE TABLE attachments (
			id INTEGER PRIMARY KEY,
			message_id INTEGER NOT NULL REFERENCES messages(id),
			filename TEXT,
			mime_type TEXT,
			size INTEGER,
			content_hash TEXT
		);
		CREATE TABLE conversations (
			id INTEGER PRIMARY KEY,
			source_id INTEGER NOT NULL REFERENCES sources(id),
			source_conversation_id TEXT,
			title TEXT,
			conversation_type TEXT NOT NULL DEFAULT 'email'
		);
	`
	_, err = db.Exec(schema)
	require.NoError(t, err, "create schema")

	// Insert metadata but NO messages
	metadata := `
		INSERT INTO sources (id, identifier, display_name) VALUES (1, 'test@gmail.com', 'Test Account');
		INSERT INTO participants (id, email_address, domain, display_name) VALUES (1, 'alice@example.com', 'example.com', 'Alice');
		INSERT INTO labels (id, source_id, name) VALUES (1, 1, 'INBOX');
	`
	_, err = db.Exec(metadata)
	require.NoError(t, err, "insert metadata")

	return tmpDir
}

// TestBuildCache_ZeroMessagesNoRepeatedRebuilds verifies that when the DB has
// zero messages but metadata parquet exists (sources, labels, etc.), subsequent
// non-full builds skip correctly and do NOT trigger repeated full rebuilds.
// Regression test for: zero-message accounts entering a rebuild loop because
// missingRequiredParquet() sees non-message parquet but missing messages parquet.
func TestBuildCache_ZeroMessagesNoRepeatedRebuilds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	tmpDir := setupTestSQLiteEmpty(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Step 1: Full rebuild to create metadata parquet (sources, labels, etc.).
	// With zero messages, messages parquet won't be created (no partitions).
	result1, err := buildCache(dbPath, analyticsDir, true)
	require.NoError(err, "first buildCache (full)")
	assert.Equal(int64(0), result1.ExportedCount, "expected 0 exported messages")

	// Step 2: Verify non-message parquet was created.
	sourcesPattern := filepath.Join(analyticsDir, "sources", "*.parquet")
	matches, _ := filepath.Glob(sourcesPattern)
	require.NotEmpty(matches, "expected sources parquet to exist after full rebuild")

	// Step 3: Run non-full build — should skip, NOT trigger another full rebuild.
	result2, err := buildCache(dbPath, analyticsDir, false)
	require.NoError(err, "second buildCache")
	assert.True(result2.Skipped, "expected second build to be skipped (no new messages), but it ran")
}

func TestBuildCacheSnapshotPredicateAcceptsCSVStringIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	db, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { require.NoError(db.Close()) }()

	var count int
	err = db.QueryRow(`
		SELECT COUNT(*)
		FROM (VALUES ('1'), ('2'), ('not-an-id')) AS m(id)
		WHERE TRY_CAST(m.id AS BIGINT) <= 2
	`).Scan(&count)

	require.NoError(err)
	assert.Equal(2, count)
}

// writeSyncState writes a _last_sync.json file to the analytics directory.
func writeSyncState(t *testing.T, analyticsDir string, lastMessageID int64) {
	t.Helper()
	writeSyncStateAt(t, analyticsDir, lastMessageID, time.Now())
}

// writeSyncStateAt writes a _last_sync.json file with an explicit timestamp.
func writeSyncStateAt(t *testing.T, analyticsDir string, lastMessageID int64, syncAt time.Time) {
	t.Helper()
	require.NoError(t, os.MkdirAll(analyticsDir, 0755), "MkdirAll analytics")
	state := syncState{
		LastMessageID: lastMessageID,
		LastSyncAt:    syncAt,
		SchemaVersion: cacheSchemaVersion,
	}
	data, err := json.Marshal(state)
	require.NoError(t, err, "marshal sync state")
	require.NoError(t, os.WriteFile(filepath.Join(analyticsDir, "_last_sync.json"), data, 0644), "write sync state")
}

// createFakeParquet creates fake parquet files for all required directories
// to simulate a complete existing cache.
func createFakeParquet(t *testing.T, analyticsDir string) {
	t.Helper()
	// Messages use hive-partitioned layout
	msgDir := filepath.Join(analyticsDir, "messages", "year=2024")
	require.NoError(t, os.MkdirAll(msgDir, 0755), "MkdirAll messages")
	require.NoError(t, os.WriteFile(filepath.Join(msgDir, "data.parquet"), []byte("fake"), 0644), "write messages parquet")
	// Other required tables use flat layout
	for _, dir := range []string{"sources", "participants", "message_recipients", "labels", "message_labels", "attachments", "conversations"} {
		d := filepath.Join(analyticsDir, dir)
		require.NoError(t, os.MkdirAll(d, 0755), "MkdirAll %s", dir)
		require.NoError(t, os.WriteFile(filepath.Join(d, "data.parquet"), []byte("fake"), 0644), "write %s parquet", dir)
	}
}

func TestCacheNeedsBuild(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, dbPath, analyticsDir string)
		wantBuild  bool
		wantReason string
	}{
		{
			name: "ZeroMessages_ZeroState_NoRebuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				t.Helper()
				// DB has 0 messages, state says 0 — no rebuild needed
				writeSyncState(t, analyticsDir, 0)
			},
			wantBuild: false,
		},
		{
			name: "NoStateFile_NoParquet_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				t.Helper()
				// No _last_sync.json, no parquet files — fresh install
			},
			wantBuild:  true,
			wantReason: "no cache exists",
		},
		{
			name: "NoStateFile_HasParquet_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				t.Helper()
				// Parquet exists but no state file — corrupt/legacy state
				createFakeParquet(t, analyticsDir)
			},
			wantBuild:  true,
			wantReason: "no sync state found",
		},
		{
			name: "NewMessages_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				t.Helper()
				// DB has messages beyond what state recorded
				db, err := sql.Open("sqlite3", dbPath)
				require.NoError(t, err, "open db")
				defer func() { _ = db.Close() }()
				_, err = db.Exec(`INSERT INTO messages (id, source_id, source_message_id, sent_at) VALUES (10, 1, 'msg10', datetime('now'))`)
				require.NoError(t, err, "insert message")
				writeSyncState(t, analyticsDir, 5)
				createFakeParquet(t, analyticsDir)
			},
			wantBuild:  true,
			wantReason: "5 new messages",
		},
		{
			name: "UpToDate_NoRebuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				t.Helper()
				// DB maxID matches state — cache is current
				db, err := sql.Open("sqlite3", dbPath)
				require.NoError(t, err, "open db")
				defer func() { _ = db.Close() }()
				_, err = db.Exec(`INSERT INTO messages (id, source_id, source_message_id, sent_at) VALUES (10, 1, 'msg10', datetime('now'))`)
				require.NoError(t, err, "insert message")
				writeSyncState(t, analyticsDir, 10)
				createFakeParquet(t, analyticsDir)
			},
			wantBuild: false,
		},
		{
			name: "HasState_EmptyParquetDir_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				t.Helper()
				// State file exists, DB has messages, but parquet dir is empty
				db, err := sql.Open("sqlite3", dbPath)
				require.NoError(t, err, "open db")
				defer func() { _ = db.Close() }()
				_, err = db.Exec(`INSERT INTO messages (id, source_id, source_message_id, sent_at) VALUES (5, 1, 'msg5', datetime('now'))`)
				require.NoError(t, err, "insert message")
				writeSyncState(t, analyticsDir, 5)
				// No parquet files created — HasParquetData returns false
			},
			wantBuild:  true,
			wantReason: "no cache exists",
		},
		{
			name: "DeletedMessages_Excluded",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				t.Helper()
				// All messages are soft-deleted — maxID should be 0
				db, err := sql.Open("sqlite3", dbPath)
				require.NoError(t, err, "open db")
				defer func() { _ = db.Close() }()
				_, err = db.Exec(`INSERT INTO messages (id, source_id, source_message_id, sent_at, deleted_from_source_at) VALUES (10, 1, 'msg10', datetime('now'), datetime('now'))`)
				require.NoError(t, err, "insert message")
				writeSyncState(t, analyticsDir, 0)
			},
			wantBuild: false,
		},
		{
			name: "CalendarOnly_NoMessagesParquet_NoRebuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				t.Helper()
				db, err := sql.Open("sqlite3", dbPath)
				require.NoError(t, err, "open db")
				defer func() { _ = db.Close() }()
				_, err = db.Exec(`
					INSERT INTO messages (id, source_id, source_message_id, sent_at, message_type)
					VALUES (10, 1, 'calendar-10', datetime('now'), 'calendar_event')
				`)
				require.NoError(t, err, "insert calendar event")
				writeSyncState(t, analyticsDir, 10)
			},
			wantBuild: false,
		},
		{
			name: "SourceDeletedAndDedupHiddenSinceBuild_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				t.Helper()
				stateTime := time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)
				db, err := sql.Open("sqlite3", dbPath)
				require.NoError(t, err, "open db")
				defer func() { _ = db.Close() }()
				_, err = db.Exec(`
					INSERT INTO messages (
						id, source_id, source_message_id, sent_at,
						deleted_from_source_at, deleted_at
					)
					VALUES (?, 1, 'msg5', ?, ?, ?)
				`, 5, stateTime.Add(-time.Hour), stateTime.Add(time.Minute), stateTime.Add(time.Minute))
				require.NoError(t, err, "insert deleted hidden message")
				writeSyncStateAt(t, analyticsDir, 5, stateTime)
				createFakeParquet(t, analyticsDir)
			},
			wantBuild:  true,
			wantReason: "1 deletions",
		},
		{
			name: "InvalidSyncState_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				t.Helper()
				// Malformed JSON in _last_sync.json
				require.NoError(t, os.MkdirAll(analyticsDir, 0755), "MkdirAll")
				require.NoError(t, os.WriteFile(filepath.Join(analyticsDir, "_last_sync.json"), []byte("{corrupt"), 0644), "write state")
				createFakeParquet(t, analyticsDir)
			},
			wantBuild:  true,
			wantReason: "invalid sync state",
		},
		{
			name: "DBOpenFailure_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				t.Helper()
				// Replace DB file with a directory so store.Open fails
				_ = os.Remove(dbPath)
				require.NoError(t, os.MkdirAll(dbPath, 0755), "MkdirAll")
				writeSyncState(t, analyticsDir, 5)
				createFakeParquet(t, analyticsDir)
			},
			wantBuild:  true,
			wantReason: "cannot verify cache status",
		},
		{
			name: "MissingRequiredParquetTables_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				t.Helper()
				// Only messages parquet exists, missing other required tables
				db, err := sql.Open("sqlite3", dbPath)
				require.NoError(t, err, "open db")
				defer func() { _ = db.Close() }()
				_, err = db.Exec(`INSERT INTO messages (id, source_id, source_message_id, sent_at) VALUES (5, 1, 'msg5', datetime('now'))`)
				require.NoError(t, err, "insert message")
				writeSyncState(t, analyticsDir, 5)
				// Only create messages parquet — other required dirs missing
				msgDir := filepath.Join(analyticsDir, "messages", "year=2024")
				require.NoError(t, os.MkdirAll(msgDir, 0755), "MkdirAll")
				require.NoError(t, os.WriteFile(filepath.Join(msgDir, "data.parquet"), []byte("fake"), 0644), "write parquet")
			},
			wantBuild:  true,
			wantReason: "cache missing required tables",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := setupTestSQLiteEmpty(t)

			dbPath := filepath.Join(tmpDir, "test.db")
			analyticsDir := filepath.Join(tmpDir, "analytics")

			tt.setup(t, dbPath, analyticsDir)

			got := cacheNeedsBuild(dbPath, analyticsDir)
			assert.Equal(t, tt.wantBuild, got.NeedsBuild, "cacheNeedsBuild() build (reason: %q)", got.Reason)
			if tt.wantReason != "" {
				assert.Equal(t, tt.wantReason, got.Reason, "cacheNeedsBuild() reason")
			}
		})
	}
}

func TestCacheNeedsBuild_LabelOnlySyncRequiresFullRebuild(t *testing.T) {
	require := require.New(t)
	tmpDir := setupTestSQLiteEmpty(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	stateTime := time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)
	writeSyncStateAt(t, analyticsDir, 5, stateTime)
	createFakeParquet(t, analyticsDir)

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err, "open db")
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE sync_runs (
			id INTEGER PRIMARY KEY,
			source_id INTEGER,
			started_at DATETIME,
			completed_at DATETIME,
			status TEXT,
			messages_processed INTEGER,
			messages_added INTEGER,
			messages_updated INTEGER,
			errors_count INTEGER
		)
	`)
	require.NoError(err, "create sync_runs")

	_, err = db.Exec(`
		INSERT INTO sync_runs (
			id, source_id, started_at, completed_at, status,
			messages_processed, messages_added, messages_updated, errors_count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		1, 1,
		stateTime.Add(30*time.Second).Format("2006-01-02 15:04:05"),
		stateTime.Add(2*time.Minute).Format("2006-01-02 15:04:05"),
		"completed", 1, 0, 2, 0,
	)
	require.NoError(err, "insert sync_run")

	got := cacheNeedsBuild(dbPath, analyticsDir)
	require.True(got.NeedsBuild, "cacheNeedsBuild() NeedsBuild = false, want true")
	require.True(got.FullRebuild, "cacheNeedsBuild() FullRebuild = false, want true")
	require.Contains(got.Reason, "updated", "cacheNeedsBuild() reason")
}

func TestCacheNeedsBuild_CalendarOnlyUpdateDoesNotRebuild(t *testing.T) {
	require := require.New(t)
	tmpDir := setupTestSQLiteEmpty(t)
	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	writeSyncState(t, analyticsDir, 0)
	createFakeParquet(t, analyticsDir)

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES (2, 'gcal', 'user@example.com/primary');
		CREATE TABLE sync_runs (
			id INTEGER PRIMARY KEY,
			source_id INTEGER,
			started_at DATETIME,
			completed_at DATETIME,
			status TEXT,
			messages_processed INTEGER,
			messages_added INTEGER,
			messages_updated INTEGER,
			errors_count INTEGER
		);
		INSERT INTO sync_runs (
			id, source_id, started_at, completed_at, status,
			messages_processed, messages_added, messages_updated, errors_count
		) VALUES (1, 2, datetime('now'), datetime('now'), 'completed', 1, 0, 1, 0);
	`)
	require.NoError(err)

	got := cacheNeedsBuild(dbPath, analyticsDir)
	require.False(got.NeedsBuild, "calendar-only update must not invalidate searchable cache: %+v", got)
}

func TestCacheNeedsBuild_UpdatedSyncCompletionOrderAndFailure(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		runID     int64
		completed time.Time
	}{
		{
			name:      "older run id completes after cache build",
			status:    "completed",
			runID:     10,
			completed: time.Date(2026, 3, 18, 12, 1, 0, 0, time.UTC),
		},
		{
			name:      "failed run persisted a successful refresh",
			status:    "failed",
			runID:     12,
			completed: time.Date(2026, 3, 18, 12, 1, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			tmpDir := setupTestSQLiteEmpty(t)
			dbPath := filepath.Join(tmpDir, "test.db")
			analyticsDir := filepath.Join(tmpDir, "analytics")
			stateTime := time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)
			require.NoError(os.MkdirAll(analyticsDir, 0755))
			state := syncState{
				LastMessageID:          0,
				LastSyncAt:             stateTime,
				LastCompletedSyncRunID: 11,
				SchemaVersion:          cacheSchemaVersion,
			}
			data, err := json.Marshal(state)
			require.NoError(err)
			require.NoError(os.WriteFile(filepath.Join(analyticsDir, "_last_sync.json"), data, 0644))
			createFakeParquet(t, analyticsDir)

			db, err := sql.Open("sqlite3", dbPath)
			require.NoError(err)
			defer func() { _ = db.Close() }()
			_, err = db.Exec(`
				CREATE TABLE sync_runs (
					id INTEGER PRIMARY KEY,
					source_id INTEGER,
					started_at DATETIME,
					completed_at DATETIME,
					status TEXT,
					messages_processed INTEGER,
					messages_added INTEGER,
					messages_updated INTEGER,
					errors_count INTEGER
				)
			`)
			require.NoError(err)
			_, err = db.Exec(`
				INSERT INTO sync_runs (
					id, source_id, started_at, completed_at, status,
					messages_processed, messages_added, messages_updated, errors_count
				) VALUES (?, 1, ?, ?, ?, 1, 0, 1, 0)
			`, tt.runID, stateTime.Add(-time.Minute), tt.completed, tt.status)
			require.NoError(err)

			got := cacheNeedsBuild(dbPath, analyticsDir)
			require.True(got.NeedsBuild, "updated terminal run must invalidate cache: %+v", got)
			require.True(got.FullRebuild, "updated terminal run requires full rebuild: %+v", got)
		})
	}
}

func TestCacheNeedsBuild_UpdateCounterDecreaseRequiresRebuild(t *testing.T) {
	require := require.New(t)
	tmpDir := setupTestSQLiteEmpty(t)
	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")
	require.NoError(os.MkdirAll(analyticsDir, 0755))
	state := syncState{
		LastSyncAt:           time.Now(),
		SchemaVersion:        cacheSchemaVersion,
		LastCacheUpdateCount: 2,
	}
	data, err := json.Marshal(state)
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(analyticsDir, "_last_sync.json"), data, 0644))
	createFakeParquet(t, analyticsDir)

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`
		CREATE TABLE sync_runs (
			id INTEGER PRIMARY KEY,
			source_id INTEGER,
			started_at DATETIME,
			completed_at DATETIME,
			status TEXT,
			messages_processed INTEGER,
			messages_added INTEGER,
			messages_updated INTEGER,
			errors_count INTEGER
		);
		INSERT INTO sync_runs (
			id, source_id, started_at, completed_at, status,
			messages_processed, messages_added, messages_updated, errors_count
		) VALUES (1, 1, datetime('now'), datetime('now'), 'completed', 1, 0, 1, 0);
	`)
	require.NoError(err)

	got := cacheNeedsBuild(dbPath, analyticsDir)
	require.True(got.NeedsBuild, "mutation watermark decrease must invalidate cache: %+v", got)
	require.True(got.FullRebuild, "mutation watermark decrease requires full rebuild: %+v", got)
}

func TestCacheNeedsBuild_IgnoresAlreadyProcessedUpdatedSyncRun(t *testing.T) {
	require := require.New(t)
	tmpDir := setupTestSQLiteEmpty(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	stateTime := time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)
	require.NoError(os.MkdirAll(analyticsDir, 0755), "MkdirAll analytics")
	state := syncState{
		LastMessageID:          5,
		LastSyncAt:             stateTime,
		LastCompletedSyncRunID: 7,
		LastCacheUpdateCount:   2,
		SchemaVersion:          cacheSchemaVersion,
	}
	data, err := json.Marshal(state)
	require.NoError(err, "marshal sync state")
	require.NoError(os.WriteFile(filepath.Join(analyticsDir, "_last_sync.json"), data, 0644), "write sync state")
	createFakeParquet(t, analyticsDir)

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err, "open db")
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE sync_runs (
			id INTEGER PRIMARY KEY,
			source_id INTEGER,
			started_at DATETIME,
			completed_at DATETIME,
			status TEXT,
			messages_processed INTEGER,
			messages_added INTEGER,
			messages_updated INTEGER,
			errors_count INTEGER
		)
	`)
	require.NoError(err, "create sync_runs")

	_, err = db.Exec(`
		INSERT INTO sync_runs (
			id, source_id, started_at, completed_at, status,
			messages_processed, messages_added, messages_updated, errors_count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		7, 1,
		stateTime.Add(30*time.Second).Format("2006-01-02 15:04:05"),
		stateTime.Add(30*time.Second).Format("2006-01-02 15:04:05"),
		"completed", 1, 0, 2, 0,
	)
	require.NoError(err, "insert sync_run")

	got := cacheNeedsBuild(dbPath, analyticsDir)
	require.False(got.NeedsBuild, "cacheNeedsBuild() = %+v, want no rebuild for already-processed sync run", got)
}

// TestCacheNeedsBuild_SchemaVersionMismatch covers the regression where a
// complete cache that is otherwise up to date (maxLiveID == LastMessageID,
// all required parquet present) was reported fresh after cacheSchemaVersion
// was bumped, leaving the daemon serving stale-layout parquet. A recorded
// schema version other than the current one now forces a full rebuild.
func TestCacheNeedsBuild_SchemaVersionMismatch(t *testing.T) {
	require := require.New(t)
	tmpDir := setupTestSQLiteEmpty(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err, "open db")
	_, err = db.Exec(`INSERT INTO messages (id, source_id, source_message_id, sent_at) VALUES (10, 1, 'msg10', datetime('now'))`)
	require.NoError(err, "insert message")
	require.NoError(db.Close(), "close db")

	// State matches the DB (id 10) and every required parquet exists, so the
	// only staleness signal is the outdated schema version.
	require.NoError(os.MkdirAll(analyticsDir, 0755), "MkdirAll analytics")
	state := syncState{
		LastMessageID: 10,
		LastSyncAt:    time.Now(),
		SchemaVersion: cacheSchemaVersion - 1,
	}
	data, err := json.Marshal(state)
	require.NoError(err, "marshal sync state")
	require.NoError(os.WriteFile(filepath.Join(analyticsDir, "_last_sync.json"), data, 0644), "write sync state")
	createFakeParquet(t, analyticsDir)

	got := cacheNeedsBuild(dbPath, analyticsDir)
	require.True(got.NeedsBuild, "cacheNeedsBuild() = %+v, want NeedsBuild=true on schema mismatch", got)
	require.True(got.FullRebuild, "cacheNeedsBuild() = %+v, want FullRebuild=true on schema mismatch", got)
	require.Contains(got.Reason, "schema", "cacheNeedsBuild() reason should mention schema")
}

// TestCacheNeedsBuild_DedupHidesAfterLastSync covers the regression
// where dedup-hidden rows (deleted_at) added after the cache was built
// silently stayed in Parquet because the staleness check only watched
// deleted_from_source_at. The check now treats dedup hides the same
// way: any row whose deleted_at is at or after LastSyncAt forces a
// full rebuild.
func TestCacheNeedsBuild_DedupHidesAfterLastSync(t *testing.T) {
	require := require.New(t)
	tmpDir := setupTestSQLiteEmpty(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	stateTime := time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)
	writeSyncStateAt(t, analyticsDir, 5, stateTime)
	createFakeParquet(t, analyticsDir)

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err, "open db")
	defer func() { _ = db.Close() }()

	// Insert one live row and one row dedup-hidden after LastSyncAt.
	_, err = db.Exec(
		`INSERT INTO messages
			(id, source_id, source_message_id, sent_at, deleted_at)
		 VALUES (1, 1, 'msg1', datetime('now'), NULL)`,
	)
	require.NoError(err, "insert live row")
	hiddenAt := stateTime.Add(1 * time.Hour).
		Format("2006-01-02 15:04:05")
	_, err = db.Exec(
		`INSERT INTO messages
			(id, source_id, source_message_id, sent_at, deleted_at)
		 VALUES (2, 1, 'msg2', datetime('now'), ?)`,
		hiddenAt,
	)
	require.NoError(err, "insert dedup-hidden row")

	got := cacheNeedsBuild(dbPath, analyticsDir)
	require.True(got.NeedsBuild, "cacheNeedsBuild() = %+v, want NeedsBuild=true after dedup hide", got)
	require.True(got.FullRebuild, "cacheNeedsBuild() = %+v, want FullRebuild=true after dedup hide", got)
	assert.Contains(t, got.Reason, "dedup-hidden", "Reason")
}

func TestBuildCache_RecordsLastCompletedSyncRunID(t *testing.T) {
	require := require.New(t)
	tmpDir := setupTestSQLite(t)

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err, "open db")
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`
		CREATE TABLE sync_runs (
			id INTEGER PRIMARY KEY,
			source_id INTEGER,
			started_at DATETIME,
			completed_at DATETIME,
			status TEXT,
			messages_processed INTEGER,
			messages_added INTEGER,
			messages_updated INTEGER,
			errors_count INTEGER
		)
	`)
	require.NoError(err, "create sync_runs")

	_, err = db.Exec(`
		INSERT INTO sync_runs (
			id, source_id, started_at, completed_at, status,
			messages_processed, messages_added, messages_updated, errors_count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		11, 1,
		"2026-03-18 12:00:00",
		"2026-03-18 12:00:01",
		"completed", 1, 0, 2, 0,
	)
	require.NoError(err, "insert sync_run")

	_, err = buildCache(dbPath, analyticsDir, true)
	require.NoError(err, "buildCache")

	data, err := os.ReadFile(filepath.Join(analyticsDir, "_last_sync.json"))
	require.NoError(err, "read sync state")

	var state syncState
	require.NoError(json.Unmarshal(data, &state), "unmarshal sync state")
	require.Equal(int64(11), state.LastCompletedSyncRunID, "LastCompletedSyncRunID")
}

// TestBuildCache_ErrorDoesNotWriteStateFile verifies that when buildCache fails,
// the state file (_last_sync.json) is not written or updated. Without this
// guard, a failed export could write the current max message ID to the state
// file, causing future incremental builds to skip the rebuild permanently.
func TestBuildCache_ErrorDoesNotWriteStateFile(t *testing.T) {
	tmpDir := t.TempDir()

	analyticsDir := filepath.Join(tmpDir, "analytics")
	stateFile := filepath.Join(analyticsDir, "_last_sync.json")

	// Use a nonexistent DB path to force an error during cache build.
	_, err := buildCache(filepath.Join(tmpDir, "nonexistent.db"), analyticsDir, false)
	require.Error(t, err, "expected error from nonexistent DB")

	// Verify state file was NOT written.
	_, statErr := os.Stat(stateFile)
	assert.True(t, os.IsNotExist(statErr), "state file must not be written when buildCache returns an error")
}

// BenchmarkBuildCacheIncremental benchmarks incremental export performance.
func BenchmarkBuildCacheIncremental(b *testing.B) {
	tmpDir := b.TempDir()

	dbPath := filepath.Join(tmpDir, "bench.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	db, _ := sql.Open("sqlite3", dbPath)

	// Create schema and initial data (10000 messages)
	_, _ = db.Exec(`
		CREATE TABLE sources (id INTEGER PRIMARY KEY, identifier TEXT);
		CREATE TABLE messages (id INTEGER PRIMARY KEY, source_id INTEGER, source_message_id TEXT, sent_at TIMESTAMP, size_estimate INTEGER, has_attachments BOOLEAN, subject TEXT, snippet TEXT, conversation_id INTEGER, deleted_from_source_at TIMESTAMP, attachment_count INTEGER DEFAULT 0, sender_id INTEGER, message_type TEXT NOT NULL DEFAULT 'email', deleted_at DATETIME);
		CREATE TABLE participants (id INTEGER PRIMARY KEY, email_address TEXT UNIQUE, domain TEXT, display_name TEXT, phone_number TEXT);
		CREATE TABLE message_recipients (message_id INTEGER, participant_id INTEGER, recipient_type TEXT, display_name TEXT);
		CREATE TABLE labels (id INTEGER PRIMARY KEY, name TEXT);
		CREATE TABLE message_labels (message_id INTEGER, label_id INTEGER);
		CREATE TABLE attachments (message_id INTEGER, size INTEGER, filename TEXT);
		CREATE TABLE conversations (id INTEGER PRIMARY KEY, source_conversation_id TEXT, title TEXT, conversation_type TEXT NOT NULL DEFAULT 'email');
		INSERT INTO sources VALUES (1, 'test@gmail.com');
		INSERT INTO labels VALUES (1, 'INBOX');
		INSERT INTO participants VALUES (1, 'alice@example.com', 'example.com', 'Alice', NULL);
		INSERT INTO participants VALUES (2, 'bob@example.com', 'example.com', 'Bob', NULL);
	`)

	// Insert conversations to match messages
	for i := 1; i <= 100; i++ {
		_, _ = db.Exec("INSERT INTO conversations VALUES (?, ?)", i, "thread"+string(rune('0'+i%10)))
	}

	baseTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 10000; i++ {
		sentAt := baseTime.Add(time.Duration(i) * time.Hour)
		_, _ = db.Exec("INSERT INTO messages VALUES (?, 1, ?, ?, ?, 0, ?, ?, ?, NULL)",
			i, "msg"+string(rune('0'+i%10)), sentAt, 1000, "Subject", "Snippet", 1)
		_, _ = db.Exec("INSERT INTO message_recipients VALUES (?, 1, 'from', NULL)", i)
		_, _ = db.Exec("INSERT INTO message_recipients VALUES (?, 2, 'to', NULL)", i)
		_, _ = db.Exec("INSERT INTO message_labels VALUES (?, 1)", i)
	}

	// Initial export
	_, _ = buildCache(dbPath, analyticsDir, true)

	// Add 100 new messages for incremental test
	for i := 10001; i <= 10100; i++ {
		sentAt := baseTime.Add(time.Duration(i) * time.Hour)
		_, _ = db.Exec("INSERT INTO messages VALUES (?, 1, ?, ?, ?, 0, ?, ?, ?, NULL)",
			i, "msg"+string(rune('0'+i%10)), sentAt, 1000, "Subject", "Snippet", 1)
		_, _ = db.Exec("INSERT INTO message_recipients VALUES (?, 1, 'from', NULL)", i)
		_, _ = db.Exec("INSERT INTO message_recipients VALUES (?, 2, 'to', NULL)", i)
		_, _ = db.Exec("INSERT INTO message_labels VALUES (?, 1)", i)
	}
	_ = db.Close()

	b.ResetTimer()
	for range b.N {
		// Reset sync state to re-trigger incremental export
		stateFile := filepath.Join(analyticsDir, "_last_sync.json")
		state := syncState{LastMessageID: 10000, LastSyncAt: time.Now(), SchemaVersion: cacheSchemaVersion}
		data, err := json.Marshal(state)
		if err != nil {
			b.Fatalf("marshal sync state: %v", err)
		}
		_ = os.WriteFile(stateFile, data, 0644)

		if _, err := buildCache(dbPath, analyticsDir, false); err != nil {
			b.Fatalf("buildCache: %v", err)
		}
	}
}

// TestGlobalConfigFlagArgs verifies that the persistent flags affecting
// config resolution are forwarded to the build-cache subprocess so it
// loads identical configuration to the daemon that spawned it.
func TestGlobalConfigFlagArgs(t *testing.T) {
	// Save and restore the package globals these flags bind to.
	origCfg, origHome, origLocal := cfgFile, homeDir, useLocal
	origLevel, origVerbose, origSQL, origSlow := logLevel, verbose, logSQL, logSQLSlow
	t.Cleanup(func() {
		cfgFile, homeDir, useLocal = origCfg, origHome, origLocal
		logLevel, verbose, logSQL, logSQLSlow = origLevel, origVerbose, origSQL, origSlow
	})

	tests := []struct {
		name       string
		cfgFile    string
		homeDir    string
		local      bool
		logLevel   string
		verbose    bool
		logSQL     bool
		logSQLSlow int64
		want       []string
	}{
		{name: "none set", want: nil},
		{name: "config only", cfgFile: "/etc/msgvault.toml", want: []string{"--config", "/etc/msgvault.toml"}},
		{name: "home only", homeDir: "/data/msgvault", want: []string{"--home", "/data/msgvault"}},
		{name: "local only", local: true, want: []string{"--local"}},
		{name: "log-level only", logLevel: "info", want: []string{"--log-level", "info"}},
		{name: "verbose only", verbose: true, want: []string{"--verbose"}},
		{name: "log-sql only", logSQL: true, want: []string{"--log-sql"}},
		{name: "log-sql-slow-ms only", logSQLSlow: 250, want: []string{"--log-sql-slow-ms", "250"}},
		{
			name:       "all set",
			cfgFile:    "/etc/msgvault.toml",
			homeDir:    "/data/msgvault",
			local:      true,
			logLevel:   "debug",
			verbose:    true,
			logSQL:     true,
			logSQLSlow: 500,
			want: []string{
				"--config", "/etc/msgvault.toml", "--home", "/data/msgvault", "--local",
				"--log-level", "debug", "--verbose", "--log-sql", "--log-sql-slow-ms", "500",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgFile, homeDir, useLocal = tt.cfgFile, tt.homeDir, tt.local
			logLevel, verbose, logSQL, logSQLSlow = tt.logLevel, tt.verbose, tt.logSQL, tt.logSQLSlow
			assert.Equal(t, tt.want, globalConfigFlagArgs())
		})
	}
}

func TestBuildCacheDaemonChildEnvMarksSubprocess(t *testing.T) {
	got := buildCacheDaemonChildEnv([]string{
		"OTHER=value",
		buildCacheDaemonSubprocessEnv + "=0",
	}, 4242)

	assert.Contains(t, got, "OTHER=value", "preserves existing environment")
	assert.Contains(t, got, buildCacheDaemonSubprocessEnv+"=4242", "marks daemon-owned subprocess")
	assert.NotContains(t, got, buildCacheDaemonSubprocessEnv+"=0", "replaces stale marker")
}
