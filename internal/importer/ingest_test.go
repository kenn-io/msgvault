package importer

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/email"
)

func TestNormalizeMessageID_InvalidUTF8(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "latin1 bytes in angle brackets",
			input: "<03f501c9a35b$add3cc60$cc22f472@\xD5\xC5\xC6\xE6\xB9\xF3>",
		},
		{
			name:  "bare invalid bytes",
			input: "msg-\x80\x81\x82@example.com",
		},
		{
			name:  "valid utf8 unchanged",
			input: "<valid@example.com>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeMessageID(tt.input)
			assertpkg.True(t, utf8.ValidString(result),
				"normalizeMessageID(%q) produced invalid UTF-8: %q",
				tt.input, result)
		})
	}
}

func TestNormalizeMessageID_PreservesValidContent(t *testing.T) {
	assertpkg.Equal(t, "valid@example.com", normalizeMessageID("<valid@example.com>"))
}

func TestIngestRawMessage_SanitizesAddressFields(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")

	src, err := st.GetOrCreateSource("test", "test@example.com")
	require.NoError(err, "get/create source")

	// Build a message with a Message-ID containing invalid UTF-8.
	// The From address has a display name with invalid bytes.
	invalidName := "User \xD5\xC5\xC6"
	invalidMsgID := "<03f501c9a35b@\xD5\xC5\xC6\xE6\xB9\xF3>"

	raw := email.NewMessage().
		From(invalidName+" <sender@example.com>").
		To("recipient@example.com").
		Subject("Test").
		Header("Message-ID", invalidMsgID).
		Body("body text").
		Bytes()

	log := slog.Default()

	err = IngestRawMessage(
		context.Background(), st,
		src.ID, "test@example.com", "",
		nil, "source-msg-1", "fakehash",
		raw, time.Time{}, log,
	)
	require.NoError(err, "IngestRawMessage")

	// Verify all participant fields are valid UTF-8
	db := st.DB()
	rows, err := db.Query(
		"SELECT email_address, display_name, domain FROM participants",
	)
	require.NoError(err, "query participants")
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var emailAddr string
		var displayName sql.NullString
		var domain string
		require.NoError(rows.Scan(&emailAddr, &displayName, &domain))
		assert.True(utf8.ValidString(emailAddr),
			"invalid UTF-8 in email_address: %q", emailAddr)
		if displayName.Valid {
			assert.True(utf8.ValidString(displayName.String),
				"invalid UTF-8 in display_name: %q", displayName.String)
		}
		assert.True(utf8.ValidString(domain),
			"invalid UTF-8 in domain: %q", domain)
	}
	require.NoError(rows.Err(), "participants rows")

	// Verify conversation source_conversation_id is valid UTF-8
	rows2, err := db.Query(
		"SELECT source_conversation_id FROM conversations",
	)
	require.NoError(err, "query conversations")
	defer func() { _ = rows2.Close() }()

	for rows2.Next() {
		var srcID string
		require.NoError(rows2.Scan(&srcID))
		assert.True(utf8.ValidString(srcID),
			"invalid UTF-8 in source_conversation_id: %q", srcID)
	}
	require.NoError(rows2.Err(), "conversations rows")
}

func TestIngestRawMessage_InvalidUTF8_RecipientLinkage(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	require.NoError(st.InitSchema(), "init schema")

	src, err := st.GetOrCreateSource("test", "test@example.com")
	require.NoError(err, "get/create source")

	// RFC 2047 Q-encoded display names that decode to invalid UTF-8.
	// enmime decodes these successfully, producing names with raw
	// invalid bytes that SanitizeUTF8 will clean up.
	raw := []byte("From: =?utf-8?q?Sender_=D5=C5=C6?= <sender@example.com>\r\n" +
		"To: =?utf-8?q?Recip_=E6=B9=F3?= <recipient@example.com>\r\n" +
		"Subject: linkage test\r\n" +
		"Date: Mon, 01 Jan 2024 12:00:00 +0000\r\n" +
		"\r\n" +
		"test body\r\n")

	err = IngestRawMessage(
		context.Background(), st,
		src.ID, "test@example.com", "",
		nil, "source-msg-linkage", "fakehash",
		raw, time.Time{}, slog.Default(),
	)
	require.NoError(err, "IngestRawMessage")

	db := st.DB()

	// Verify sender_id is set on the message.
	var senderID sql.NullInt64
	err = db.QueryRow(
		`SELECT sender_id FROM messages
		 WHERE source_message_id = ?`, "source-msg-linkage",
	).Scan(&senderID)
	require.NoError(err, "query sender_id")
	assert.True(senderID.Valid, "sender_id should be set")

	// Verify message_recipients rows exist for from, to.
	for _, rtype := range []string{"from", "to"} {
		var count int
		err = db.QueryRow(
			`SELECT COUNT(*) FROM message_recipients mr
			 JOIN messages m ON m.id = mr.message_id
			 WHERE m.source_message_id = ?
			   AND mr.recipient_type = ?`,
			"source-msg-linkage", rtype,
		).Scan(&count)
		require.NoError(err, "query recipients (%s)", rtype)
		assert.Positive(count, "expected at least 1 %s recipient", rtype)
	}

	// Verify display names are valid UTF-8 (sanitized).
	rows, err := db.Query(
		`SELECT display_name FROM participants
		 WHERE display_name IS NOT NULL`,
	)
	require.NoError(err, "query display names")
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		require.NoError(rows.Scan(&name))
		assert.True(utf8.ValidString(name),
			"invalid UTF-8 in display_name: %q", name)
	}
	require.NoError(rows.Err(), "display_name rows")
}
