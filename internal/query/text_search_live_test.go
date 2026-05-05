package query

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// openTextSearchDB creates a minimal in-memory SQLite DB with one text
// message indexed in FTS. The caller may soft-delete the message via
// SQL after this call to verify live-message filtering.
func openTextSearchDB(t *testing.T) (*sql.DB, int64) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`
		CREATE TABLE sources (
			id INTEGER PRIMARY KEY,
			source_type TEXT NOT NULL DEFAULT 'imessage',
			identifier TEXT NOT NULL UNIQUE
		);
		CREATE TABLE conversations (
			id INTEGER PRIMARY KEY,
			source_id INTEGER,
			source_conversation_id TEXT,
			title TEXT
		);
		CREATE TABLE participants (
			id INTEGER PRIMARY KEY,
			email_address TEXT,
			display_name TEXT,
			phone_number TEXT,
			domain TEXT
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			source_id INTEGER,
			source_message_id TEXT,
			conversation_id INTEGER,
			sender_id INTEGER,
			subject TEXT,
			snippet TEXT,
			sent_at DATETIME,
			size_estimate INTEGER DEFAULT 0,
			has_attachments INTEGER DEFAULT 0,
			attachment_count INTEGER DEFAULT 0,
			deleted_at DATETIME,
			deleted_from_source_at DATETIME,
			message_type TEXT NOT NULL DEFAULT 'imessage'
		);
		CREATE VIRTUAL TABLE messages_fts USING fts5(subject, body, content='', contentless_delete=1);
	`)
	if err != nil {
		t.Skipf("FTS5 not available: %v", err)
	}

	_, err = db.Exec(`INSERT INTO sources (id, identifier) VALUES (1, 'test@example.com')`)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	_, err = db.Exec(`INSERT INTO conversations (id, source_id) VALUES (1, 1)`)
	if err != nil {
		t.Fatalf("insert conv: %v", err)
	}
	res, err := db.Exec(`INSERT INTO messages (id, source_id, conversation_id, subject, message_type) VALUES (1, 1, 1, 'hello world', 'imessage')`)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
	msgID, _ := res.LastInsertId()
	_, err = db.Exec(`INSERT INTO messages_fts (rowid, subject, body) VALUES (?, 'hello world', 'hello world')`, msgID)
	if err != nil {
		t.Fatalf("insert fts: %v", err)
	}
	return db, msgID
}

func TestSQLiteEngine_TextSearch_ExcludesDedupHidden(t *testing.T) {
	db, msgID := openTextSearchDB(t)
	engine := NewSQLiteEngine(db)
	ctx := context.Background()

	// Confirm the message appears before deletion.
	results, err := engine.TextSearch(ctx, "hello", 10, 0)
	if err != nil {
		t.Fatalf("TextSearch before delete: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result before delete, got %d", len(results))
	}

	// Soft-delete via dedup (deleted_at).
	if _, err := db.Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, msgID); err != nil {
		t.Fatalf("set deleted_at: %v", err)
	}

	results, err = engine.TextSearch(ctx, "hello", 10, 0)
	if err != nil {
		t.Fatalf("TextSearch after dedup delete: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("want 0 results after dedup delete, got %d", len(results))
	}
}

func TestSQLiteEngine_TextSearch_ExcludesSourceDeleted(t *testing.T) {
	db, msgID := openTextSearchDB(t)
	engine := NewSQLiteEngine(db)
	ctx := context.Background()

	// Soft-delete via source deletion (deleted_from_source_at).
	if _, err := db.Exec(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`, msgID); err != nil {
		t.Fatalf("set deleted_from_source_at: %v", err)
	}

	results, err := engine.TextSearch(ctx, "hello", 10, 0)
	if err != nil {
		t.Fatalf("TextSearch after source delete: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("want 0 results after source delete, got %d", len(results))
	}
}
