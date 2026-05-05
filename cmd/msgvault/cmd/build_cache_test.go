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

	_ "github.com/marcboeker/go-duckdb"
	_ "github.com/mattn/go-sqlite3"
)

// setupTestSQLite creates a test SQLite database with realistic email data.
func setupTestSQLite(t *testing.T) (string, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "msgvault-build-cache-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("open sqlite: %v", err)
	}
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
		t.Fatalf("create schema: %v", err)
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

	if _, err := db.Exec(testData); err != nil {
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("insert test data: %v", err)
	}

	cleanup := func() {
		_ = os.RemoveAll(tmpDir)
	}

	return tmpDir, cleanup
}

// TestBuildCache_BasicExport tests that buildCache creates all expected Parquet files.
func TestBuildCache_BasicExport(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	result, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("buildCache: %v", err)
	}

	if result.Skipped {
		t.Fatal("expected export to run, but was skipped")
	}

	if result.ExportedCount != 5 {
		t.Errorf("expected 5 exported messages, got %d", result.ExportedCount)
	}

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
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected directory %s to exist", dir)
		}
	}

	// Verify sync state was saved
	stateFile := filepath.Join(analyticsDir, "_last_sync.json")
	if _, err := os.Stat(stateFile); os.IsNotExist(err) {
		t.Fatal("expected _last_sync.json to exist")
	}

	var state syncState
	data, _ := os.ReadFile(stateFile)
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse sync state: %v", err)
	}

	if state.LastMessageID != 5 {
		t.Errorf("expected LastMessageID=5, got %d", state.LastMessageID)
	}
}

// TestBuildCache_DataIntegrity verifies the exported Parquet data matches SQLite.
func TestBuildCache_DataIntegrity(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	if _, err := buildCache(dbPath, analyticsDir, false); err != nil {
		t.Fatalf("buildCache: %v", err)
	}

	// Open DuckDB to query the Parquet files
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("open duckdb: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Helper to count rows in a Parquet file
	countRows := func(pattern string) int64 {
		var count int64
		query := "SELECT COUNT(*) FROM read_parquet('" + pattern + "')"
		if err := db.QueryRow(query).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", pattern, err)
		}
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
		if count != tc.expected {
			t.Errorf("%s: expected %d rows, got %d", tc.name, tc.expected, count)
		}
	}

	// Verify message data integrity
	var subject string
	msgQuery := "SELECT subject FROM read_parquet('" + filepath.Join(analyticsDir, "messages", "**", "*.parquet") + "') WHERE id = 1"
	if err := db.QueryRow(msgQuery).Scan(&subject); err != nil {
		t.Fatalf("query message: %v", err)
	}
	if subject != "Hello World" {
		t.Errorf("expected subject 'Hello World', got %q", subject)
	}

	// Verify participant data
	var email string
	partQuery := "SELECT email_address FROM read_parquet('" + filepath.Join(analyticsDir, "participants", "*.parquet") + "') WHERE id = 1"
	if err := db.QueryRow(partQuery).Scan(&email); err != nil {
		t.Fatalf("query participant: %v", err)
	}
	if email != "alice@example.com" {
		t.Errorf("expected email 'alice@example.com', got %q", email)
	}

	// Verify attachment sizes
	var totalSize int64
	attQuery := "SELECT SUM(size) FROM read_parquet('" + filepath.Join(analyticsDir, "attachments", "*.parquet") + "')"
	if err := db.QueryRow(attQuery).Scan(&totalSize); err != nil {
		t.Fatalf("query attachments: %v", err)
	}
	if totalSize != 35000 { // 10000 + 5000 + 20000
		t.Errorf("expected total attachment size 35000, got %d", totalSize)
	}
}

// TestBuildCache_IncrementalExport tests that incremental exports only add new messages.
func TestBuildCache_IncrementalExport(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// First export
	result1, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("first buildCache: %v", err)
	}
	if result1.ExportedCount != 5 {
		t.Errorf("first export: expected 5 messages, got %d", result1.ExportedCount)
	}

	// Add new messages to SQLite
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

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
	if err != nil {
		t.Fatalf("insert new messages: %v", err)
	}

	// Second export (incremental)
	result2, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("second buildCache: %v", err)
	}

	if result2.Skipped {
		t.Fatal("expected incremental export to run, but was skipped")
	}

	// Verify total count includes both old and new
	if result2.ExportedCount != 7 {
		t.Errorf("after incremental: expected 7 total messages, got %d", result2.ExportedCount)
	}

	// Verify junction tables accumulated across incremental runs
	duckdb, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("open duckdb: %v", err)
	}
	defer func() { _ = duckdb.Close() }()

	countRows := func(pattern string) int64 {
		var count int64
		// Use forward slashes for DuckDB glob patterns (backslashes fail on Windows)
		pattern = filepath.ToSlash(pattern)
		if err := duckdb.QueryRow("SELECT COUNT(*) FROM read_parquet('" + pattern + "')").Scan(&count); err != nil {
			t.Fatalf("count %s: %v", pattern, err)
		}
		return count
	}

	// Messages: 7 total (5 original + 2 new)
	if c := countRows(filepath.Join(analyticsDir, "messages", "**", "*.parquet")); c != 7 {
		t.Errorf("messages: expected 7, got %d", c)
	}

	// Message recipients: 16 total (12 original + 4 new)
	if c := countRows(filepath.Join(analyticsDir, "message_recipients", "*.parquet")); c != 16 {
		t.Errorf("message_recipients: expected 16, got %d", c)
	}

	// Message labels: 10 total (8 original + 2 new)
	if c := countRows(filepath.Join(analyticsDir, "message_labels", "*.parquet")); c != 10 {
		t.Errorf("message_labels: expected 10, got %d", c)
	}

	// Attachments: 4 total (3 original + 1 new)
	if c := countRows(filepath.Join(analyticsDir, "attachments", "*.parquet")); c != 4 {
		t.Errorf("attachments: expected 4, got %d", c)
	}

	// Participants: 4 (overwritten each run, not appended)
	if c := countRows(filepath.Join(analyticsDir, "participants", "*.parquet")); c != 4 {
		t.Errorf("participants: expected 4, got %d", c)
	}

	// Labels: 3 (overwritten each run)
	if c := countRows(filepath.Join(analyticsDir, "labels", "*.parquet")); c != 3 {
		t.Errorf("labels: expected 3, got %d", c)
	}

	// Sources: 1 (overwritten each run)
	if c := countRows(filepath.Join(analyticsDir, "sources", "*.parquet")); c != 1 {
		t.Errorf("sources: expected 1, got %d", c)
	}

	// Verify sync state was updated
	var state syncState
	data, _ := os.ReadFile(filepath.Join(analyticsDir, "_last_sync.json"))
	_ = json.Unmarshal(data, &state)

	if state.LastMessageID != 7 {
		t.Errorf("expected LastMessageID=7, got %d", state.LastMessageID)
	}
}

// TestBuildCache_SkipsWhenNoNewMessages tests that export is skipped when no new messages.
func TestBuildCache_SkipsWhenNoNewMessages(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// First export
	if _, err := buildCache(dbPath, analyticsDir, false); err != nil {
		t.Fatalf("first buildCache: %v", err)
	}

	// Second export without any new data
	result, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("second buildCache: %v", err)
	}

	if !result.Skipped {
		t.Error("expected export to be skipped when no new messages")
	}
}

// TestBuildCache_BackfillsMissingConversations tests that an older cache missing
// the conversations parquet table triggers a rebuild even when no new messages
// exist. This simulates the upgrade path from a cache that predates the
// conversations export.
func TestBuildCache_BackfillsMissingConversations(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// First export — creates all tables including conversations.
	result1, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("first buildCache: %v", err)
	}
	if result1.Skipped {
		t.Fatal("expected first export to run")
	}

	// Simulate a legacy cache by removing the conversations directory.
	conversationsDir := filepath.Join(analyticsDir, "conversations")
	if err := os.RemoveAll(conversationsDir); err != nil {
		t.Fatalf("remove conversations dir: %v", err)
	}

	// Verify the conversations dir is actually gone.
	if _, err := os.Stat(conversationsDir); !os.IsNotExist(err) {
		t.Fatal("expected conversations dir to be removed")
	}

	// Second export — no new messages, but conversations parquet is missing.
	// buildCache must NOT skip; it should backfill the missing table.
	result2, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("second buildCache: %v", err)
	}

	if result2.Skipped {
		t.Fatal("expected backfill rebuild when conversations parquet is missing, but was skipped")
	}

	// Verify conversations parquet was recreated.
	pattern := filepath.Join(conversationsDir, "*.parquet")
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		t.Error("expected conversations parquet files to be recreated after backfill")
	}

	// Verify conversation data is correct.
	duckdb, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("open duckdb: %v", err)
	}
	defer func() { _ = duckdb.Close() }()

	var count int64
	q := "SELECT COUNT(*) FROM read_parquet('" + filepath.Join(conversationsDir, "*.parquet") + "')"
	if err := duckdb.QueryRow(q).Scan(&count); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if count != 4 { // 4 conversations in test data
		t.Errorf("expected 4 conversations after backfill, got %d", count)
	}

	// Third export — everything is up-to-date, should skip.
	result3, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("third buildCache: %v", err)
	}
	if !result3.Skipped {
		t.Error("expected third export to be skipped (all tables present, no new messages)")
	}
}

// TestBuildCache_BackfillAfterIncrementalNoDuplicates tests the scenario:
// full export → add data → incremental export → remove a required table → backfill.
// This verifies that stale incr_*.parquet shards from prior incremental runs
// are cleaned up during backfill, preventing duplicate rows.
func TestBuildCache_BackfillAfterIncrementalNoDuplicates(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Step 1: Initial full export (5 messages, 12 recipients).
	result1, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("first buildCache: %v", err)
	}
	if result1.ExportedCount != 5 {
		t.Fatalf("expected 5 messages in initial export, got %d", result1.ExportedCount)
	}

	// Step 2: Add new messages to SQLite, then incremental export.
	// This creates incr_*.parquet files alongside data.parquet.
	sqliteDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
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
	if err != nil {
		t.Fatalf("insert incremental data: %v", err)
	}

	result2, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("second buildCache (incremental): %v", err)
	}
	if result2.ExportedCount != 7 {
		t.Fatalf("expected 7 messages after incremental, got %d", result2.ExportedCount)
	}

	// Step 3: Remove conversations dir (simulate legacy cache missing a table).
	conversationsDir := filepath.Join(analyticsDir, "conversations")
	if err := os.RemoveAll(conversationsDir); err != nil {
		t.Fatalf("remove conversations dir: %v", err)
	}

	// Step 4: Backfill — no new messages, but conversations is missing.
	// This must do a full rebuild, clearing stale incremental shards.
	result3, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("third buildCache (backfill): %v", err)
	}
	if result3.Skipped {
		t.Fatal("expected backfill, but was skipped")
	}

	// Step 5: Verify exact counts — no duplicates from stale incr_*.parquet.
	duckdb, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("open duckdb: %v", err)
	}
	defer func() { _ = duckdb.Close() }()

	countRows := func(pattern string) int64 {
		var count int64
		pattern = filepath.ToSlash(pattern)
		if err := duckdb.QueryRow("SELECT COUNT(*) FROM read_parquet('" + pattern + "')").Scan(&count); err != nil {
			t.Fatalf("count %s: %v", pattern, err)
		}
		return count
	}

	// Expected: 7 messages (5 original + 2 incremental), NOT 12 (5+2+5 from dup)
	if c := countRows(filepath.Join(analyticsDir, "messages", "**", "*.parquet")); c != 7 {
		t.Errorf("messages: expected 7, got %d (possible duplicate from stale incremental shards)", c)
	}
	// Expected: 16 recipients (12 original + 4 incremental), NOT 28
	if c := countRows(filepath.Join(analyticsDir, "message_recipients", "*.parquet")); c != 16 {
		t.Errorf("message_recipients: expected 16, got %d", c)
	}
	// Expected: 10 message_labels (8 original + 2 incremental), NOT 18
	if c := countRows(filepath.Join(analyticsDir, "message_labels", "*.parquet")); c != 10 {
		t.Errorf("message_labels: expected 10, got %d", c)
	}
	// Expected: 3 attachments (no new ones added), NOT 6
	if c := countRows(filepath.Join(analyticsDir, "attachments", "*.parquet")); c != 3 {
		t.Errorf("attachments: expected 3, got %d", c)
	}
	// Conversations should be restored.
	if c := countRows(filepath.Join(analyticsDir, "conversations", "*.parquet")); c != 4 {
		t.Errorf("conversations: expected 4, got %d", c)
	}
}

// TestBuildCache_BackfillWithNewMessages tests that when a required table is
// missing AND new messages exist, the build does a full rebuild (not incremental).
// Without this, the code would stay in incremental mode and only export new
// message_recipients, leaving historical rows missing from the rebuilt table.
func TestBuildCache_BackfillWithNewMessages(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Step 1: Full export (5 messages, 12 recipients).
	if _, err := buildCache(dbPath, analyticsDir, false); err != nil {
		t.Fatalf("first buildCache: %v", err)
	}

	// Step 2: Delete message_recipients dir (simulate missing table).
	recipientsDir := filepath.Join(analyticsDir, "message_recipients")
	if err := os.RemoveAll(recipientsDir); err != nil {
		t.Fatalf("remove message_recipients dir: %v", err)
	}

	// Step 3: Add new messages to SQLite (so maxID > lastMessageID).
	sqliteDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	_, err = sqliteDB.Exec(`
		INSERT INTO messages (id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments) VALUES
			(6, 1, 'msg6', 101, 'New msg', 'Preview 6', '2024-03-15 10:00:00', 1200, 0);
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name) VALUES
			(6, 1, 'from', 'Alice Smith'),
			(6, 2, 'to', 'Bob Jones');
	`)
	_ = sqliteDB.Close()
	if err != nil {
		t.Fatalf("insert new data: %v", err)
	}

	// Step 4: Build — missing table + new messages should force full rebuild.
	result, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("second buildCache: %v", err)
	}
	if result.Skipped {
		t.Fatal("expected rebuild, but was skipped")
	}

	// Step 5: Verify ALL recipients present (12 original + 2 new = 14).
	// If only incremental ran, we'd see just 2 (new message's recipients).
	duckdb, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("open duckdb: %v", err)
	}
	defer func() { _ = duckdb.Close() }()

	var count int64
	q := "SELECT COUNT(*) FROM read_parquet('" + filepath.ToSlash(filepath.Join(recipientsDir, "*.parquet")) + "')"
	if err := duckdb.QueryRow(q).Scan(&count); err != nil {
		t.Fatalf("count message_recipients: %v", err)
	}
	if count != 14 {
		t.Errorf("message_recipients: expected 14 (12 original + 2 new), got %d", count)
	}

	// Also verify messages count is correct (6 total, no duplicates).
	var msgCount int64
	msgQ := "SELECT COUNT(*) FROM read_parquet('" + filepath.ToSlash(filepath.Join(analyticsDir, "messages", "**", "*.parquet")) + "', hive_partitioning=true)"
	if err := duckdb.QueryRow(msgQ).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 6 {
		t.Errorf("messages: expected 6, got %d", msgCount)
	}
}

// TestBuildCache_BackfillMissingMessages tests that when the messages parquet
// directory is missing but other parquet tables exist (e.g. participants),
// the cache is detected as broken and rebuilt. This covers an edge case where
// HasParquetData (messages-only) would return false, causing missingRequiredParquet
// to return false and skip the rebuild.
func TestBuildCache_BackfillMissingMessages(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Step 1: Full export to create all tables.
	result1, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("first buildCache: %v", err)
	}
	if result1.Skipped {
		t.Fatal("expected first export to run")
	}

	// Step 2: Remove the messages directory (simulate corruption/partial failure).
	messagesDir := filepath.Join(analyticsDir, "messages")
	if err := os.RemoveAll(messagesDir); err != nil {
		t.Fatalf("remove messages dir: %v", err)
	}

	// Verify other parquet tables still exist (e.g. participants).
	participantsPattern := filepath.Join(analyticsDir, "participants", "*.parquet")
	if matches, _ := filepath.Glob(participantsPattern); len(matches) == 0 {
		t.Fatal("expected participants parquet to still exist")
	}

	// Step 3: Build again — messages are missing but other tables exist.
	// Must detect the broken cache and rebuild, NOT skip.
	result2, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("second buildCache: %v", err)
	}
	if result2.Skipped {
		t.Fatal("expected rebuild when messages parquet is missing but other tables exist")
	}

	// Verify messages were restored.
	duckdb, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("open duckdb: %v", err)
	}
	defer func() { _ = duckdb.Close() }()

	var count int64
	q := "SELECT COUNT(*) FROM read_parquet('" + filepath.ToSlash(filepath.Join(messagesDir, "**", "*.parquet")) + "', hive_partitioning=true)"
	if err := duckdb.QueryRow(q).Scan(&count); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if count != 5 {
		t.Errorf("messages: expected 5, got %d", count)
	}
}

// TestBuildCache_FullRebuild tests that --full-rebuild clears and recreates cache.
func TestBuildCache_FullRebuild(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// First export
	if _, err := buildCache(dbPath, analyticsDir, false); err != nil {
		t.Fatalf("first buildCache: %v", err)
	}

	// Create a marker file to verify directory is cleared
	markerFile := filepath.Join(analyticsDir, "messages", "marker.txt")
	_ = os.WriteFile(markerFile, []byte("test"), 0644)

	// Full rebuild
	result, err := buildCache(dbPath, analyticsDir, true)
	if err != nil {
		t.Fatalf("full rebuild: %v", err)
	}

	if result.Skipped {
		t.Fatal("full rebuild should not be skipped")
	}

	// Verify marker file was removed
	if _, err := os.Stat(markerFile); !os.IsNotExist(err) {
		t.Error("expected marker file to be removed during full rebuild")
	}

	// Verify data was exported
	if result.ExportedCount != 5 {
		t.Errorf("expected 5 messages after full rebuild, got %d", result.ExportedCount)
	}
}

// TestBuildCache_DeletedMessagesIncluded tests that deleted messages are exported.
func TestBuildCache_DeletedMessagesIncluded(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Mark one message as deleted
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	_, err = db.Exec("UPDATE messages SET deleted_from_source_at = '2024-06-01 12:00:00' WHERE id = 3")
	_ = db.Close()
	if err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	// Export
	result, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("buildCache: %v", err)
	}

	// All 5 messages should be exported (including deleted)
	if result.ExportedCount != 5 {
		t.Errorf("expected 5 messages (including deleted), got %d", result.ExportedCount)
	}

	// Verify deleted_from_source_at is preserved
	duckdb, _ := sql.Open("duckdb", "")
	defer func() { _ = duckdb.Close() }()

	var deletedCount int64
	query := "SELECT COUNT(*) FROM read_parquet('" + filepath.Join(analyticsDir, "messages", "**", "*.parquet") + "') WHERE deleted_from_source_at IS NOT NULL"
	if err := duckdb.QueryRow(query).Scan(&deletedCount); err != nil {
		t.Fatalf("query deleted: %v", err)
	}

	if deletedCount != 1 {
		t.Errorf("expected 1 deleted message in Parquet, got %d", deletedCount)
	}
}

// TestBuildCache_MessagesWithoutSentAt tests that messages without sent_at are excluded.
func TestBuildCache_MessagesWithoutSentAt(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Add a message without sent_at
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO messages (id, source_id, source_message_id, subject, snippet, size_estimate)
		VALUES (6, 1, 'msg6', 'No Date', 'Preview', 100)
	`)
	_ = db.Close()
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	result, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("buildCache: %v", err)
	}

	// Only 5 messages with sent_at should be exported
	if result.ExportedCount != 5 {
		t.Errorf("expected 5 messages (excluding null sent_at), got %d", result.ExportedCount)
	}
}

// TestBuildCache_EndToEndWithQueryEngine tests the full flow with query engine.
func TestBuildCache_EndToEndWithQueryEngine(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Build cache
	if _, err := buildCache(dbPath, analyticsDir, false); err != nil {
		t.Fatalf("buildCache: %v", err)
	}

	// Open DuckDB and test queries that match what the TUI does
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("open duckdb: %v", err)
	}
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
	rows, err := db.Query(senderQuery)
	if err != nil {
		t.Fatalf("sender query: %v", err)
	}

	senderCounts := make(map[string]int64)
	for rows.Next() {
		var email string
		var count int64
		_ = rows.Scan(&email, &count)
		senderCounts[email] = count
	}
	_ = rows.Close()

	if senderCounts["alice@example.com"] != 3 {
		t.Errorf("expected alice sent 3 messages, got %d", senderCounts["alice@example.com"])
	}
	if senderCounts["bob@company.org"] != 2 {
		t.Errorf("expected bob sent 2 messages, got %d", senderCounts["bob@company.org"])
	}

	// Test 2: Aggregate by label (like AggregateByLabel)
	labelQuery := ctes + `
		SELECT lbl.name as key, COUNT(*) as count
		FROM msg
		JOIN ml ON ml.message_id = msg.id
		JOIN lbl ON lbl.id = ml.label_id
		GROUP BY lbl.name
		ORDER BY count DESC
	`
	rows, err = db.Query(labelQuery)
	if err != nil {
		t.Fatalf("label query: %v", err)
	}

	labelCounts := make(map[string]int64)
	for rows.Next() {
		var name string
		var count int64
		_ = rows.Scan(&name, &count)
		labelCounts[name] = count
	}
	_ = rows.Close()

	if labelCounts["INBOX"] != 5 {
		t.Errorf("expected INBOX has 5 messages, got %d", labelCounts["INBOX"])
	}
	if labelCounts["Work"] != 2 {
		t.Errorf("expected Work has 2 messages, got %d", labelCounts["Work"])
	}

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
	if err := db.QueryRow(statsQuery).Scan(&msgCount, &totalSize, &attCount, &attSize); err != nil {
		t.Fatalf("stats query: %v", err)
	}

	if msgCount != 5 {
		t.Errorf("expected 5 messages, got %d", msgCount)
	}
	if totalSize != 8000 { // 1000+2000+1500+3000+500
		t.Errorf("expected total size 8000, got %d", totalSize)
	}
	if attCount != 3 {
		t.Errorf("expected 3 attachments, got %d", attCount)
	}
	if attSize != 35000 { // 10000+5000+20000
		t.Errorf("expected attachment size 35000, got %d", attSize)
	}
}

// TestBuildCache_YearPartitioning tests that messages are partitioned by year.
func TestBuildCache_YearPartitioning(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Add messages from different years
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO messages (id, source_id, source_message_id, subject, sent_at, size_estimate) VALUES
			(6, 1, 'msg6', 'Old Message', '2020-06-15 10:00:00', 100),
			(7, 1, 'msg7', 'Recent Message', '2025-01-15 10:00:00', 100);
	`)
	_ = db.Close()
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := buildCache(dbPath, analyticsDir, false); err != nil {
		t.Fatalf("buildCache: %v", err)
	}

	// Check for year partitions
	years := []string{"2020", "2024", "2025"}
	for _, year := range years {
		pattern := filepath.Join(analyticsDir, "messages", "year="+year, "*.parquet")
		matches, _ := filepath.Glob(pattern)
		if len(matches) == 0 {
			t.Errorf("expected partition for year=%s", year)
		}
	}
}

// TestBuildCache_UTF8Handling tests that invalid UTF-8 is handled gracefully.
func TestBuildCache_UTF8Handling(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Insert data with potentially problematic characters
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Note: SQLite3 driver may sanitize, but we test the flow
	_, err = db.Exec(`
		UPDATE messages SET subject = 'Test émoji 🎉 and unicode' WHERE id = 1;
		UPDATE participants SET display_name = 'Müller' WHERE id = 1;
	`)
	_ = db.Close()
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	// Should not error
	result, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("buildCache with unicode: %v", err)
	}

	if result.ExportedCount != 5 {
		t.Errorf("expected 5 messages, got %d", result.ExportedCount)
	}

	// Verify data is readable
	duckdb, _ := sql.Open("duckdb", "")
	defer func() { _ = duckdb.Close() }()

	var subject string
	query := "SELECT subject FROM read_parquet('" + filepath.Join(analyticsDir, "messages", "**", "*.parquet") + "') WHERE id = 1"
	if err := duckdb.QueryRow(query).Scan(&subject); err != nil {
		t.Fatalf("read unicode subject: %v", err)
	}

	if subject != "Test émoji 🎉 and unicode" {
		t.Errorf("unicode not preserved: got %q", subject)
	}
}

// TestBuildCache_EmptyDatabase tests handling of empty database.
func TestBuildCache_EmptyDatabase(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "msgvault-empty-db-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

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
	if err != nil {
		t.Fatalf("buildCache on empty db: %v", err)
	}

	// Should be skipped (no messages)
	if !result.Skipped {
		t.Error("expected empty database export to be skipped")
	}
}

// TestCSVFallbackPath exercises the Windows-style CSV intermediate path:
// SQLite → CSV → DuckDB views → COPY to Parquet.
// This runs on all platforms to ensure the fallback logic works correctly.
func TestCSVFallbackPath(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	csvDir := filepath.Join(tmpDir, "csv")
	if err := os.MkdirAll(csvDir, 0755); err != nil {
		t.Fatalf("create csv dir: %v", err)
	}

	// 1. Export tables to CSV (same as setupSQLiteSource Windows path)
	sqliteDB, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

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
			t.Fatalf("exportToCSV %s: %v", tbl.name, err)
		}
	}
	_ = sqliteDB.Close()

	// 2. Open DuckDB and create views (same as setupSQLiteSource)
	duckDB, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("open duckdb: %v", err)
	}
	defer func() { _ = duckDB.Close() }()

	if _, err := duckDB.Exec("CREATE SCHEMA sqlite_db"); err != nil {
		t.Fatalf("create schema: %v", err)
	}

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
		if _, err := duckDB.Exec(viewSQL); err != nil {
			t.Fatalf("create view %s: %v", tbl.name, err)
		}
	}

	// 3. Verify sent_at is correctly typed as TIMESTAMP
	var year int
	err = duckDB.QueryRow(`SELECT CAST(EXTRACT(YEAR FROM sent_at) AS INTEGER) FROM sqlite_db.messages WHERE id = 1`).Scan(&year)
	if err != nil {
		t.Fatalf("EXTRACT(YEAR FROM sent_at) failed — sent_at may not be typed as TIMESTAMP: %v", err)
	}
	if year != 2024 {
		t.Errorf("expected year 2024, got %d", year)
	}

	// 4. Verify NULLs round-trip correctly (deleted_from_source_at should be NULL)
	var deletedAt sql.NullTime
	err = duckDB.QueryRow(`SELECT deleted_from_source_at FROM sqlite_db.messages WHERE id = 1`).Scan(&deletedAt)
	if err != nil {
		t.Fatalf("query deleted_from_source_at: %v", err)
	}
	if deletedAt.Valid {
		t.Errorf("expected deleted_from_source_at to be NULL, got %v", deletedAt.Time)
	}

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
		if err := duckDB.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM sqlite_db."%s"`, tbl)).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if count != expected {
			t.Errorf("sqlite_db.%s: expected %d rows, got %d", tbl, expected, count)
		}
	}

	// 6. Verify the full buildCache pipeline works via CSV views
	// Run the same COPY query that buildCache uses for messages
	analyticsDir := filepath.Join(tmpDir, "analytics")
	messagesDir := filepath.Join(analyticsDir, "messages")
	if err := os.MkdirAll(messagesDir, 0755); err != nil {
		t.Fatalf("create analytics dir: %v", err)
	}
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

	if _, err := duckDB.Exec(copySQL); err != nil {
		t.Fatalf("COPY messages to Parquet via CSV views failed: %v", err)
	}

	// Verify Parquet files were created with correct year partitions
	for _, y := range []string{"2024"} {
		pattern := filepath.Join(messagesDir, "year="+y, "*.parquet")
		matches, _ := filepath.Glob(pattern)
		if len(matches) == 0 {
			t.Errorf("expected Parquet partition for year=%s", y)
		}
	}
}

// BenchmarkBuildCache benchmarks the export performance.
func BenchmarkBuildCache(b *testing.B) {
	// Create a larger test dataset
	tmpDir, err := os.MkdirTemp("", "msgvault-bench-*")
	if err != nil {
		b.Fatalf("create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

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
	for i := 0; i < b.N; i++ {
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
func setupTestSQLiteEmpty(t *testing.T) (string, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "msgvault-build-cache-empty-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}

	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("open sqlite: %v", err)
	}
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
	if _, err := db.Exec(schema); err != nil {
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("create schema: %v", err)
	}

	// Insert metadata but NO messages
	metadata := `
		INSERT INTO sources (id, identifier, display_name) VALUES (1, 'test@gmail.com', 'Test Account');
		INSERT INTO participants (id, email_address, domain, display_name) VALUES (1, 'alice@example.com', 'example.com', 'Alice');
		INSERT INTO labels (id, source_id, name) VALUES (1, 1, 'INBOX');
	`
	if _, err := db.Exec(metadata); err != nil {
		_ = os.RemoveAll(tmpDir)
		t.Fatalf("insert metadata: %v", err)
	}

	return tmpDir, func() { _ = os.RemoveAll(tmpDir) }
}

// TestBuildCache_ZeroMessagesNoRepeatedRebuilds verifies that when the DB has
// zero messages but metadata parquet exists (sources, labels, etc.), subsequent
// non-full builds skip correctly and do NOT trigger repeated full rebuilds.
// Regression test for: zero-message accounts entering a rebuild loop because
// missingRequiredParquet() sees non-message parquet but missing messages parquet.
func TestBuildCache_ZeroMessagesNoRepeatedRebuilds(t *testing.T) {
	tmpDir, cleanup := setupTestSQLiteEmpty(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	// Step 1: Full rebuild to create metadata parquet (sources, labels, etc.).
	// With zero messages, messages parquet won't be created (no partitions).
	result1, err := buildCache(dbPath, analyticsDir, true)
	if err != nil {
		t.Fatalf("first buildCache (full): %v", err)
	}
	if result1.ExportedCount != 0 {
		t.Errorf("expected 0 exported messages, got %d", result1.ExportedCount)
	}

	// Step 2: Verify non-message parquet was created.
	sourcesPattern := filepath.Join(analyticsDir, "sources", "*.parquet")
	if matches, _ := filepath.Glob(sourcesPattern); len(matches) == 0 {
		t.Fatal("expected sources parquet to exist after full rebuild")
	}

	// Step 3: Run non-full build — should skip, NOT trigger another full rebuild.
	result2, err := buildCache(dbPath, analyticsDir, false)
	if err != nil {
		t.Fatalf("second buildCache: %v", err)
	}
	if !result2.Skipped {
		t.Error("expected second build to be skipped (no new messages), but it ran")
	}
}

// writeSyncState writes a _last_sync.json file to the analytics directory.
func writeSyncState(t *testing.T, analyticsDir string, lastMessageID int64) {
	t.Helper()
	writeSyncStateAt(t, analyticsDir, lastMessageID, time.Now())
}

// writeSyncStateAt writes a _last_sync.json file with an explicit timestamp.
func writeSyncStateAt(t *testing.T, analyticsDir string, lastMessageID int64, syncAt time.Time) {
	t.Helper()
	if err := os.MkdirAll(analyticsDir, 0755); err != nil {
		t.Fatalf("MkdirAll analytics: %v", err)
	}
	state := syncState{LastMessageID: lastMessageID, LastSyncAt: syncAt}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal sync state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(analyticsDir, "_last_sync.json"), data, 0644); err != nil {
		t.Fatalf("write sync state: %v", err)
	}
}

// createFakeParquet creates fake parquet files for all required directories
// to simulate a complete existing cache.
func createFakeParquet(t *testing.T, analyticsDir string) {
	t.Helper()
	// Messages use hive-partitioned layout
	msgDir := filepath.Join(analyticsDir, "messages", "year=2024")
	if err := os.MkdirAll(msgDir, 0755); err != nil {
		t.Fatalf("MkdirAll messages: %v", err)
	}
	if err := os.WriteFile(filepath.Join(msgDir, "data.parquet"), []byte("fake"), 0644); err != nil {
		t.Fatalf("write messages parquet: %v", err)
	}
	// Other required tables use flat layout
	for _, dir := range []string{"sources", "participants", "message_recipients", "labels", "message_labels", "attachments", "conversations"} {
		d := filepath.Join(analyticsDir, dir)
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(d, "data.parquet"), []byte("fake"), 0644); err != nil {
			t.Fatalf("write %s parquet: %v", dir, err)
		}
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
				// DB has 0 messages, state says 0 — no rebuild needed
				writeSyncState(t, analyticsDir, 0)
			},
			wantBuild: false,
		},
		{
			name: "NoStateFile_NoParquet_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				// No _last_sync.json, no parquet files — fresh install
			},
			wantBuild:  true,
			wantReason: "no cache exists",
		},
		{
			name: "NoStateFile_HasParquet_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				// Parquet exists but no state file — corrupt/legacy state
				createFakeParquet(t, analyticsDir)
			},
			wantBuild:  true,
			wantReason: "no sync state found",
		},
		{
			name: "NewMessages_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				// DB has messages beyond what state recorded
				db, err := sql.Open("sqlite3", dbPath)
				if err != nil {
					t.Fatalf("open db: %v", err)
				}
				defer func() { _ = db.Close() }()
				_, err = db.Exec(`INSERT INTO messages (id, source_id, source_message_id, sent_at) VALUES (10, 1, 'msg10', datetime('now'))`)
				if err != nil {
					t.Fatalf("insert message: %v", err)
				}
				writeSyncState(t, analyticsDir, 5)
				createFakeParquet(t, analyticsDir)
			},
			wantBuild:  true,
			wantReason: "5 new messages",
		},
		{
			name: "UpToDate_NoRebuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				// DB maxID matches state — cache is current
				db, err := sql.Open("sqlite3", dbPath)
				if err != nil {
					t.Fatalf("open db: %v", err)
				}
				defer func() { _ = db.Close() }()
				_, err = db.Exec(`INSERT INTO messages (id, source_id, source_message_id, sent_at) VALUES (10, 1, 'msg10', datetime('now'))`)
				if err != nil {
					t.Fatalf("insert message: %v", err)
				}
				writeSyncState(t, analyticsDir, 10)
				createFakeParquet(t, analyticsDir)
			},
			wantBuild: false,
		},
		{
			name: "HasState_EmptyParquetDir_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				// State file exists, DB has messages, but parquet dir is empty
				db, err := sql.Open("sqlite3", dbPath)
				if err != nil {
					t.Fatalf("open db: %v", err)
				}
				defer func() { _ = db.Close() }()
				_, err = db.Exec(`INSERT INTO messages (id, source_id, source_message_id, sent_at) VALUES (5, 1, 'msg5', datetime('now'))`)
				if err != nil {
					t.Fatalf("insert message: %v", err)
				}
				writeSyncState(t, analyticsDir, 5)
				// No parquet files created — HasParquetData returns false
			},
			wantBuild:  true,
			wantReason: "no cache exists",
		},
		{
			name: "DeletedMessages_Excluded",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				// All messages are soft-deleted — maxID should be 0
				db, err := sql.Open("sqlite3", dbPath)
				if err != nil {
					t.Fatalf("open db: %v", err)
				}
				defer func() { _ = db.Close() }()
				_, err = db.Exec(`INSERT INTO messages (id, source_id, source_message_id, sent_at, deleted_from_source_at) VALUES (10, 1, 'msg10', datetime('now'), datetime('now'))`)
				if err != nil {
					t.Fatalf("insert message: %v", err)
				}
				writeSyncState(t, analyticsDir, 0)
			},
			wantBuild: false,
		},
		{
			name: "InvalidSyncState_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				// Malformed JSON in _last_sync.json
				if err := os.MkdirAll(analyticsDir, 0755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				if err := os.WriteFile(filepath.Join(analyticsDir, "_last_sync.json"), []byte("{corrupt"), 0644); err != nil {
					t.Fatalf("write state: %v", err)
				}
				createFakeParquet(t, analyticsDir)
			},
			wantBuild:  true,
			wantReason: "invalid sync state",
		},
		{
			name: "DBOpenFailure_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				// Replace DB file with a directory so store.Open fails
				_ = os.Remove(dbPath)
				if err := os.MkdirAll(dbPath, 0755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				writeSyncState(t, analyticsDir, 5)
				createFakeParquet(t, analyticsDir)
			},
			wantBuild:  true,
			wantReason: "cannot verify cache status",
		},
		{
			name: "MissingRequiredParquetTables_NeedsBuild",
			setup: func(t *testing.T, dbPath, analyticsDir string) {
				// Only messages parquet exists, missing other required tables
				db, err := sql.Open("sqlite3", dbPath)
				if err != nil {
					t.Fatalf("open db: %v", err)
				}
				defer func() { _ = db.Close() }()
				_, err = db.Exec(`INSERT INTO messages (id, source_id, source_message_id, sent_at) VALUES (5, 1, 'msg5', datetime('now'))`)
				if err != nil {
					t.Fatalf("insert message: %v", err)
				}
				writeSyncState(t, analyticsDir, 5)
				// Only create messages parquet — other required dirs missing
				msgDir := filepath.Join(analyticsDir, "messages", "year=2024")
				if err := os.MkdirAll(msgDir, 0755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				if err := os.WriteFile(filepath.Join(msgDir, "data.parquet"), []byte("fake"), 0644); err != nil {
					t.Fatalf("write parquet: %v", err)
				}
			},
			wantBuild:  true,
			wantReason: "cache missing required tables",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir, cleanup := setupTestSQLiteEmpty(t)
			defer cleanup()

			dbPath := filepath.Join(tmpDir, "test.db")
			analyticsDir := filepath.Join(tmpDir, "analytics")

			tt.setup(t, dbPath, analyticsDir)

			got := cacheNeedsBuild(dbPath, analyticsDir)
			if got.NeedsBuild != tt.wantBuild {
				t.Errorf("cacheNeedsBuild() build = %v, want %v (reason: %q)", got.NeedsBuild, tt.wantBuild, got.Reason)
			}
			if tt.wantReason != "" && got.Reason != tt.wantReason {
				t.Errorf("cacheNeedsBuild() reason = %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}

func TestCacheNeedsBuild_LabelOnlySyncRequiresFullRebuild(t *testing.T) {
	tmpDir, cleanup := setupTestSQLiteEmpty(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	stateTime := time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)
	writeSyncStateAt(t, analyticsDir, 5, stateTime)
	createFakeParquet(t, analyticsDir)

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
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
	if err != nil {
		t.Fatalf("create sync_runs: %v", err)
	}

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
	if err != nil {
		t.Fatalf("insert sync_run: %v", err)
	}

	got := cacheNeedsBuild(dbPath, analyticsDir)
	if !got.NeedsBuild {
		t.Fatal("cacheNeedsBuild() NeedsBuild = false, want true")
	}
	if !got.FullRebuild {
		t.Fatal("cacheNeedsBuild() FullRebuild = false, want true")
	}
	if !strings.Contains(got.Reason, "updated") {
		t.Fatalf("cacheNeedsBuild() reason = %q, want substring %q", got.Reason, "updated")
	}
}

func TestCacheNeedsBuild_IgnoresAlreadyProcessedUpdatedSyncRun(t *testing.T) {
	tmpDir, cleanup := setupTestSQLiteEmpty(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	stateTime := time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)
	if err := os.MkdirAll(analyticsDir, 0755); err != nil {
		t.Fatalf("MkdirAll analytics: %v", err)
	}
	state := syncState{
		LastMessageID:          5,
		LastSyncAt:             stateTime,
		LastCompletedSyncRunID: 7,
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal sync state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(analyticsDir, "_last_sync.json"), data, 0644); err != nil {
		t.Fatalf("write sync state: %v", err)
	}
	createFakeParquet(t, analyticsDir)

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
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
	if err != nil {
		t.Fatalf("create sync_runs: %v", err)
	}

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
	if err != nil {
		t.Fatalf("insert sync_run: %v", err)
	}

	got := cacheNeedsBuild(dbPath, analyticsDir)
	if got.NeedsBuild {
		t.Fatalf("cacheNeedsBuild() = %+v, want no rebuild for already-processed sync run", got)
	}
}

// TestCacheNeedsBuild_DedupHidesAfterLastSync covers the regression
// where dedup-hidden rows (deleted_at) added after the cache was built
// silently stayed in Parquet because the staleness check only watched
// deleted_from_source_at. The check now treats dedup hides the same
// way: any row whose deleted_at is at or after LastSyncAt forces a
// full rebuild.
func TestCacheNeedsBuild_DedupHidesAfterLastSync(t *testing.T) {
	tmpDir, cleanup := setupTestSQLiteEmpty(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")

	stateTime := time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)
	writeSyncStateAt(t, analyticsDir, 5, stateTime)
	createFakeParquet(t, analyticsDir)

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Insert one live row and one row dedup-hidden after LastSyncAt.
	if _, err := db.Exec(
		`INSERT INTO messages
			(id, source_id, source_message_id, sent_at, deleted_at)
		 VALUES (1, 1, 'msg1', datetime('now'), NULL)`,
	); err != nil {
		t.Fatalf("insert live row: %v", err)
	}
	hiddenAt := stateTime.Add(1 * time.Hour).
		Format("2006-01-02 15:04:05")
	if _, err := db.Exec(
		`INSERT INTO messages
			(id, source_id, source_message_id, sent_at, deleted_at)
		 VALUES (2, 1, 'msg2', datetime('now'), ?)`,
		hiddenAt,
	); err != nil {
		t.Fatalf("insert dedup-hidden row: %v", err)
	}

	got := cacheNeedsBuild(dbPath, analyticsDir)
	if !got.NeedsBuild {
		t.Fatalf("cacheNeedsBuild() = %+v, want NeedsBuild=true after dedup hide", got)
	}
	if !got.FullRebuild {
		t.Fatalf("cacheNeedsBuild() = %+v, want FullRebuild=true after dedup hide", got)
	}
	if !strings.Contains(got.Reason, "dedup-hidden") {
		t.Errorf("Reason = %q, want substring 'dedup-hidden'", got.Reason)
	}
}

func TestBuildCache_RecordsLastCompletedSyncRunID(t *testing.T) {
	tmpDir, cleanup := setupTestSQLite(t)
	defer cleanup()

	dbPath := filepath.Join(tmpDir, "test.db")
	analyticsDir := filepath.Join(tmpDir, "analytics")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
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
	if err != nil {
		t.Fatalf("create sync_runs: %v", err)
	}

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
	if err != nil {
		t.Fatalf("insert sync_run: %v", err)
	}

	if _, err := buildCache(dbPath, analyticsDir, true); err != nil {
		t.Fatalf("buildCache: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(analyticsDir, "_last_sync.json"))
	if err != nil {
		t.Fatalf("read sync state: %v", err)
	}

	var state syncState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal sync state: %v", err)
	}
	if state.LastCompletedSyncRunID != 11 {
		t.Fatalf("LastCompletedSyncRunID = %d, want 11", state.LastCompletedSyncRunID)
	}
}

// TestBuildCache_ErrorDoesNotWriteStateFile verifies that when buildCache fails,
// the state file (_last_sync.json) is not written or updated. Without this
// guard, a failed export could write the current max message ID to the state
// file, causing future incremental builds to skip the rebuild permanently.
func TestBuildCache_ErrorDoesNotWriteStateFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "msgvault-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	analyticsDir := filepath.Join(tmpDir, "analytics")
	stateFile := filepath.Join(analyticsDir, "_last_sync.json")

	// Use a nonexistent DB path to force an error during cache build.
	_, err = buildCache(filepath.Join(tmpDir, "nonexistent.db"), analyticsDir, false)
	if err == nil {
		t.Fatal("expected error from nonexistent DB, got nil")
	}

	// Verify state file was NOT written.
	if _, statErr := os.Stat(stateFile); !os.IsNotExist(statErr) {
		t.Error("state file must not be written when buildCache returns an error")
	}
}

// BenchmarkBuildCacheIncremental benchmarks incremental export performance.
func BenchmarkBuildCacheIncremental(b *testing.B) {
	tmpDir, err := os.MkdirTemp("", "msgvault-bench-incr-*")
	if err != nil {
		b.Fatalf("create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

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
	for i := 0; i < b.N; i++ {
		// Reset sync state to re-trigger incremental export
		stateFile := filepath.Join(analyticsDir, "_last_sync.json")
		state := syncState{LastMessageID: 10000, LastSyncAt: time.Now()}
		data, _ := json.Marshal(state)
		_ = os.WriteFile(stateFile, data, 0644)

		if _, err := buildCache(dbPath, analyticsDir, false); err != nil {
			b.Fatalf("buildCache: %v", err)
		}
	}
}
