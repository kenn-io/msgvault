package query

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

// openTextSearchDB creates a minimal in-memory SQLite DB with one text
// message indexed in FTS. The caller may soft-delete the message via
// SQL after this call to verify live-message filtering.
func openTextSearchDB(t *testing.T) (*sql.DB, int64) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	requirepkg.NoError(t, err, "open")
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
	requirepkg.NoError(t, err, "insert source")
	_, err = db.Exec(`INSERT INTO conversations (id, source_id) VALUES (1, 1)`)
	requirepkg.NoError(t, err, "insert conv")
	res, err := db.Exec(`INSERT INTO messages (id, source_id, conversation_id, subject, message_type) VALUES (1, 1, 1, 'hello world', 'imessage')`)
	requirepkg.NoError(t, err, "insert message")
	msgID, _ := res.LastInsertId()
	_, err = db.Exec(`INSERT INTO messages_fts (rowid, subject, body) VALUES (?, 'hello world', 'hello world')`, msgID)
	requirepkg.NoError(t, err, "insert fts")
	return db, msgID
}

func TestSQLiteEngine_TextSearch_ExcludesDedupHidden(t *testing.T) {
	require := requirepkg.New(t)
	db, msgID := openTextSearchDB(t)
	engine := NewSQLiteEngine(db)
	ctx := context.Background()

	// Confirm the message appears before deletion.
	results, err := engine.TextSearch(ctx, "hello", 10, 0)
	require.NoError(err, "TextSearch before delete")
	require.Len(results, 1, "want 1 result before delete")

	// Soft-delete via dedup (deleted_at).
	_, err = db.Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, msgID)
	require.NoError(err, "set deleted_at")

	results, err = engine.TextSearch(ctx, "hello", 10, 0)
	require.NoError(err, "TextSearch after dedup delete")
	assertpkg.Empty(t, results, "want 0 results after dedup delete")
}

func TestSQLiteEngine_TextSearch_ExcludesSourceDeleted(t *testing.T) {
	db, msgID := openTextSearchDB(t)
	engine := NewSQLiteEngine(db)
	ctx := context.Background()

	// Soft-delete via source deletion (deleted_from_source_at).
	_, err := db.Exec(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`, msgID)
	requirepkg.NoError(t, err, "set deleted_from_source_at")

	results, err := engine.TextSearch(ctx, "hello", 10, 0)
	requirepkg.NoError(t, err, "TextSearch after source delete")
	assertpkg.Empty(t, results, "want 0 results after source delete")
}

func TestTextModeIncludesMMSAndExcludesSynctechCalls(t *testing.T) {
	assert := assertpkg.New(t)
	db, _ := openTextSearchDB(t)
	engine := NewSQLiteEngine(db)
	ctx := context.Background()

	insertTextSearchMessage(t, db, 2, "sms", "sms body")
	insertTextSearchMessage(t, db, 3, "mms", "mms body")
	insertTextSearchMessage(t, db, 4, "synctech_sms_call", "missed call body")

	results, err := engine.TextSearch(ctx, "body", 10, 0)
	requirepkg.NoError(t, err, "TextSearch")
	var types []string
	for _, r := range results {
		types = append(types, r.MessageType)
	}
	assert.Contains(types, "sms", "text mode should include sms")
	assert.Contains(types, "mms", "text mode should include mms")
	assert.NotContains(types, "synctech_sms_call", "text mode should not include call log")
}

func TestIsTextMessageTypeIncludesMMSAndExcludesSynctechCalls(t *testing.T) {
	assertpkg.True(t, IsTextMessageType("mms"), "mms should be a text message type")
	assertpkg.False(t, IsTextMessageType("synctech_sms_call"), "synctech_sms_call should not be a text message type")
}

func insertTextSearchMessage(t *testing.T, db *sql.DB, id int64, messageType, body string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO messages (id, source_id, conversation_id, subject, snippet, message_type) VALUES (?, 1, 1, ?, ?, ?)`, id, body, body, messageType)
	requirepkg.NoError(t, err, "insert %s message", messageType)
	_, err = db.Exec(`INSERT INTO messages_fts (rowid, subject, body) VALUES (?, ?, ?)`, id, body, body)
	requirepkg.NoError(t, err, "insert %s fts", messageType)
}
