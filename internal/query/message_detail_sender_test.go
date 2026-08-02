package query

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type messageDetailReader interface {
	GetMessage(ctx context.Context, id int64) (*MessageDetail, error)
	GetMessageBySourceID(ctx context.Context, sourceMessageID string) (*MessageDetail, error)
}

func seedMessageDetailSenderFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "msgvault.db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err, "open sqlite fixture")
	schema, err := os.ReadFile("../store/schema.sql")
	require.NoError(t, err, "read schema")
	_, err = db.Exec(string(schema))
	require.NoError(t, err, "create schema")
	_, err = db.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES
			(1, 'beeper', 'synthetic-network');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES
			(1, 1, 'synthetic-chat', 'chat', 'Synthetic Chat');
		INSERT INTO participants (id, email_address, display_name, phone_number, domain) VALUES
			(1, 'sender@example.com', 'Test Sender', NULL, 'example.com'),
			(2, 'explicit@example.net', 'Stored Explicit Sender', NULL, 'example.net'),
			(3, NULL, NULL, '+15550100003', ''),
			(4, 'email-only@example.org', NULL, NULL, 'example.org'),
			(5, 'mention@example.com', 'Mentioned User', NULL, 'example.com');
		INSERT INTO messages (
			id, conversation_id, source_id, source_message_id, message_type,
			sent_at, subject, snippet, size_estimate, has_attachments, sender_id
		) VALUES
			(1, 1, 1, 'sender-only', 'beeper', '2026-01-01 10:00:00', 'Sender only', '', 10, 0, 1),
			(2, 1, 1, 'explicit-wins', 'beeper', '2026-01-01 10:01:00', 'Explicit wins', '', 10, 0, 1),
			(3, 1, 1, 'phone-and-mention', 'beeper', '2026-01-01 10:02:00', 'Phone sender', '', 10, 0, 3),
			(4, 1, 1, 'email-only', 'beeper', '2026-01-01 10:03:00', 'Email sender', '', 10, 0, 4);
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name) VALUES
			(2, 2, 'from', 'Per-message Sender'),
			(3, 5, 'mention', 'Mentioned User');
	`)
	require.NoError(t, err, "seed sender fixture")
	return dbPath, db
}

func assertMessageDetailSenderFallback(t *testing.T, reader messageDetailReader) {
	t.Helper()
	ctx := context.Background()

	byID, err := reader.GetMessage(ctx, 1)
	require.NoError(t, err, "GetMessage sender-only")
	require.NotNil(t, byID)
	assert.Equal(t, []Address{{Email: "sender@example.com", Name: "Test Sender"}}, byID.From)

	bySourceID, err := reader.GetMessageBySourceID(ctx, "sender-only")
	require.NoError(t, err, "GetMessageBySourceID sender-only")
	require.NotNil(t, bySourceID)
	assert.Equal(t, byID.From, bySourceID.From)

	explicit, err := reader.GetMessage(ctx, 2)
	require.NoError(t, err, "GetMessage explicit sender")
	require.NotNil(t, explicit)
	assert.Equal(t, []Address{{Email: "explicit@example.net", Name: "Per-message Sender"}}, explicit.From)

	phone, err := reader.GetMessage(ctx, 3)
	require.NoError(t, err, "GetMessage phone sender")
	require.NotNil(t, phone)
	assert.Equal(t, []Address{{Email: "+15550100003", Name: "+15550100003"}}, phone.From)

	emailOnly, err := reader.GetMessage(ctx, 4)
	require.NoError(t, err, "GetMessage email-only sender")
	require.NotNil(t, emailOnly)
	assert.Equal(t, []Address{{Email: "email-only@example.org", Name: "email-only@example.org"}}, emailOnly.From)
}

func TestSQLiteMessageDetailUsesDirectSenderFallback(t *testing.T) {
	_, db := seedMessageDetailSenderFixture(t)
	t.Cleanup(func() { _ = db.Close() })
	assertMessageDetailSenderFallback(t, NewSQLiteEngine(db))
}

func TestDuckDBMessageDetailUsesDirectSenderFallback(t *testing.T) {
	dbPath, db := seedMessageDetailSenderFixture(t)
	require.NoError(t, db.Close(), "close SQLite writer")

	engine, err := NewDuckDBEngine("", dbPath, nil)
	require.NoError(t, err, "NewDuckDBEngine")
	t.Cleanup(func() { _ = engine.Close() })
	if !engine.hasSQLite() {
		t.Skip("DuckDB sqlite_scanner extension unavailable")
	}
	assertMessageDetailSenderFallback(t, engine)
}
