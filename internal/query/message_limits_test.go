package query

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Native DuckDB tables exercise the production sqlite_scan SQL reader without
// requiring extension downloads. Their text/blob types match scanned SQLite.
func TestBoundedDuckDBMessageDetails(t *testing.T) {
	requirements := require.New(t)

	db, err := sql.Open("duckdb", "")
	requirements.NoError(err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
 CREATE SCHEMA sqlite_db;
 CREATE TABLE sqlite_db.messages (id BIGINT, source_id BIGINT, source_message_id VARCHAR, rfc822_message_id VARCHAR, conversation_id BIGINT, subject VARCHAR, message_type VARCHAR, snippet VARCHAR, sent_at TIMESTAMP, received_at TIMESTAMP, size_estimate BIGINT, has_attachments BOOLEAN, is_from_me BOOLEAN, deleted_from_source_at TIMESTAMP, deleted_at TIMESTAMP, sender_id BIGINT);
 CREATE TABLE sqlite_db.conversations (id BIGINT, source_conversation_id VARCHAR);
 CREATE TABLE sqlite_db.message_bodies (message_id BIGINT, body_text VARCHAR, body_html VARCHAR);
 CREATE TABLE sqlite_db.message_raw (message_id BIGINT, raw_data BLOB, compression VARCHAR);
 CREATE TABLE sqlite_db.participants (id BIGINT, email_address VARCHAR, phone_number VARCHAR, display_name VARCHAR);
 CREATE TABLE sqlite_db.message_recipients (message_id BIGINT, participant_id BIGINT, recipient_type VARCHAR, display_name VARCHAR);
 CREATE TABLE sqlite_db.labels (id BIGINT, name VARCHAR);
 CREATE TABLE sqlite_db.message_labels (message_id BIGINT, label_id BIGINT);
 CREATE TABLE sqlite_db.attachments (id BIGINT, message_id BIGINT, filename VARCHAR, mime_type VARCHAR, size BIGINT, content_hash VARCHAR, storage_path VARCHAR);
 INSERT INTO sqlite_db.messages (id, source_id, source_message_id, conversation_id, has_attachments) VALUES (1,1,'fixture',1,false);
 INSERT INTO sqlite_db.conversations VALUES (1,'fixture-thread');`)
	requirements.NoError(err)
	body := strings.Repeat("é", 17)
	_, err = db.Exec(`INSERT INTO sqlite_db.message_bodies VALUES (1, ?, '')`, body)
	requirements.NoError(err)
	t.Run("text", func(t *testing.T) {
		for _, limit := range []int64{32, 34} {
			ctx := WithMessageByteLimit(t.Context(), limit)
			detail, err := getMessageByQueryShared(ctx, db, noopRebind, "sqlite_db.", "m.id = ?", 1)
			if limit == 32 {
				require.ErrorIs(t, err, ErrOriginalMessageTooLarge)
				continue
			}
			require.NoError(t, err)
			assert.Equal(t, body, detail.BodyText)
		}
	})
	raw := []byte("Subject: Fixture\r\n\r\nSynthetic body.")
	_, err = db.Exec(`INSERT INTO sqlite_db.message_raw VALUES (1, ?, NULL)`, raw)
	requirements.NoError(err)
	t.Run("raw", func(t *testing.T) {
		for _, limit := range []int64{16, 64} {
			got, err := getMessageRawShared(WithMessageByteLimit(t.Context(), limit), db, noopRebind, "sqlite_db.", 1)
			if limit == 16 {
				require.ErrorIs(t, err, ErrOriginalMessageTooLarge)
				continue
			}
			require.NoError(t, err)
			assert.Equal(t, raw, got)
		}
	})
}
