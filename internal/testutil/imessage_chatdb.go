package testutil

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3" // SQLite driver (database/sql)
	"github.com/stretchr/testify/require"
)

// NewIMessageChatDB creates a synthetic iMessage chat.db holding the message,
// handle, and chat tables and no rows beyond one handle.
func NewIMessageChatDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.db")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
 CREATE TABLE message (
  guid TEXT NOT NULL DEFAULT 'synthetic-message', text TEXT, attributedBody BLOB,
  date INTEGER NOT NULL, is_from_me INTEGER NOT NULL DEFAULT 0,
  service TEXT DEFAULT 'SMS', cache_has_attachments INTEGER NOT NULL DEFAULT 0,
  handle_id INTEGER DEFAULT 1
 );
 CREATE TABLE handle (id TEXT);
 CREATE TABLE chat (guid TEXT, display_name TEXT, chat_identifier TEXT);
 CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER);
 CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER);
 INSERT INTO handle (id) VALUES ('peer@example.test');
 `)
	require.NoError(t, err)
	return path, db
}

// NewIMessageAttachmentChatDB extends NewIMessageChatDB with one direct chat whose
// two messages share one audio attachment (~/Library/Messages/Attachments/voice.caf).
func NewIMessageAttachmentChatDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path, db := NewIMessageChatDB(t)
	_, err := db.Exec(`
 CREATE TABLE attachment (guid TEXT, filename TEXT, transfer_name TEXT, mime_type TEXT, uti TEXT, total_bytes INTEGER, is_sticker INTEGER);
 CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER);
 INSERT INTO message (date,text,cache_has_attachments) VALUES (700000000,'Voice',1),(700000001,'Shared',1);
 INSERT INTO chat VALUES ('any;-;synthetic','', 'synthetic');
 INSERT INTO chat_message_join VALUES (1,1),(1,2);
 INSERT INTO chat_handle_join VALUES (1,1);
 INSERT INTO attachment VALUES ('synthetic-a','~/Library/Messages/Attachments/voice.caf','voice.caf','audio/x-caf','com.apple.coreaudio-format',13,0);
 INSERT INTO message_attachment_join VALUES (1,1),(2,1);
 `)
	require.NoError(t, err)
	return path, db
}
