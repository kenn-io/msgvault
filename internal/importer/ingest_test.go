package importer

import (
	"bytes"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/testutil"
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
			assert.True(t, utf8.ValidString(result),
				"normalizeMessageID(%q) produced invalid UTF-8: %q",
				tt.input, result)
		})
	}
}

func TestNormalizeMessageID_PreservesValidContent(t *testing.T) {
	assert.Equal(t, "valid@example.com", normalizeMessageID("<valid@example.com>"))
}

func TestThreadKeyPreservesLegacyMalformedMessageIDBehavior(t *testing.T) {
	assert.Equal(t, "raw-empty", threadKey(&mime.Message{MessageID: "<>"}, "raw-empty"))
	assert.Equal(t, "legacy@example.test",
		threadKey(&mime.Message{MessageID: "<<legacy@example.test>>"}, "raw-nested"))
}

func TestIngestRawMessage_SanitizesAddressFields(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite fixtures require SQLite")
	require := require.New(t)
	assert := assert.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)

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
		t.Context(), st,
		src.ID, "test@example.com", "",
		nil, "source-msg-1", "fakehash",
		raw, time.Time{}, log,
	)
	require.NoError(err, "IngestRawMessage")

	// Verify all participant fields are valid UTF-8
	db := st.DB()
	rows, err := db.QueryContext(
		t.Context(),
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
	rows2, err := db.QueryContext(
		t.Context(),
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

func TestIngestRawMessage_SalvagesHeadersAfterFatalMIMEParse(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite fixtures require SQLite")
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)

	src, err := st.GetOrCreateSource("test", "owner@example.test")
	require.NoError(err, "get/create source")
	raw := []byte("From: Sender Example <sender@example.test>\r\n" +
		"To: Owner Example <owner@example.test>\r\n" +
		"Subject: Recovered import\r\n" +
		"Date: Tue, 02 Jan 2024 15:04:05 +0000\r\n" +
		"Message-ID: <recovered-import@example.test>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
		"--outer--\r\n")

	err = IngestRawMessage(
		t.Context(), st,
		src.ID, "owner@example.test", "",
		nil, "source-msg-recovery", "fakehash",
		raw, time.Time{}, slog.Default(),
	)
	require.NoError(err, "IngestRawMessage")

	var subject, body, sender string
	var sentAt time.Time
	err = st.DB().QueryRowContext(
		t.Context(),
		`SELECT m.subject, mb.body_text, m.sent_at, p.email_address
		 FROM messages m
		 JOIN message_bodies mb ON mb.message_id = m.id
		 JOIN participants p ON p.id = m.sender_id
		 WHERE m.source_message_id = ?`,
		"source-msg-recovery",
	).Scan(&subject, &body, &sentAt, &sender)
	require.NoError(err, "query recovered message")
	assert.Equal("Recovered import", subject)
	assert.Contains(body, "MIME parsing failed")
	assert.Equal(time.Date(2024, 1, 2, 15, 4, 5, 0, time.UTC), sentAt.UTC())
	assert.Equal("sender@example.test", sender)

	var recipients int
	err = st.DB().QueryRowContext(
		t.Context(),
		`SELECT COUNT(*)
		 FROM message_recipients mr
		 JOIN messages m ON m.id = mr.message_id
		 WHERE m.source_message_id = ? AND mr.recipient_type = 'to'`,
		"source-msg-recovery",
	).Scan(&recipients)
	require.NoError(err, "query recovered recipients")
	assert.Equal(1, recipients)
}

func TestIngestRawMessage_InvalidUTF8_RecipientLinkage(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite fixtures require SQLite")
	require := require.New(t)
	assert := assert.New(t)
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)

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
		t.Context(), st,
		src.ID, "test@example.com", "",
		nil, "source-msg-linkage", "fakehash",
		raw, time.Time{}, slog.Default(),
	)
	require.NoError(err, "IngestRawMessage")

	db := st.DB()

	// Verify sender_id is set on the message.
	var senderID sql.NullInt64
	err = db.QueryRowContext(
		t.Context(),
		`SELECT sender_id FROM messages
		 WHERE source_message_id = ?`, "source-msg-linkage",
	).Scan(&senderID)
	require.NoError(err, "query sender_id")
	assert.True(senderID.Valid, "sender_id should be set")

	// Verify message_recipients rows exist for from, to.
	for _, rtype := range []string{"from", "to"} {
		var count int
		err = db.QueryRowContext(
			t.Context(),
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
	rows, err := db.QueryContext(
		t.Context(),
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

func TestIngestRawMessage_ResolvesImplausibleDateFromReceivedChain(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite fixtures require SQLite")
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)

	src, err := st.GetOrCreateSource("test", "owner@example.com")
	require.NoError(err, "get/create source")

	raw := []byte("From: sender@example.com\r\n" +
		"To: owner@example.com\r\n" +
		"Date: Thu, 01 Jan 1970 00:00:00 +0000\r\n" +
		"Received: from relay.example.net by mx.example.net; Wed, 03 Jan 2007 15:04:05 +0000\r\n" +
		"Received: from sender.example.net by relay.example.net; Tue, 02 Jan 2007 15:04:05 +0000\r\n" +
		"Subject: date resolution\r\n\r\nbody\r\n")
	fallback := time.Date(2015, 5, 5, 12, 0, 0, 0, time.UTC)

	err = IngestRawMessage(
		t.Context(), st,
		src.ID, "owner@example.com", "",
		nil, "source-msg-date", "fakehash",
		raw, fallback, slog.Default(),
	)
	require.NoError(err, "IngestRawMessage")

	var sentAt, internalDate time.Time
	err = st.DB().QueryRowContext(
		t.Context(),
		`SELECT sent_at, internal_date FROM messages
		 WHERE source_message_id = ?`,
		"source-msg-date",
	).Scan(&sentAt, &internalDate)
	require.NoError(err, "query dates")
	assert.Equal(time.Date(2007, 1, 2, 15, 4, 5, 0, time.UTC), sentAt.UTC())
	assert.Equal(fallback, internalDate.UTC())
}

func TestIngestRawMessage_LeavesCanonicalDateUnsetWhenNoDateIsPlausible(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite fixtures require SQLite")
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)

	src, err := st.GetOrCreateSource("test", "owner@example.com")
	require.NoError(err, "get/create source")
	raw := []byte("From: sender@example.com\r\n" +
		"To: owner@example.com\r\n" +
		"Date: Thu, 01 Jan 1970 00:00:00 +0000\r\n" +
		"Subject: no plausible date\r\n\r\nbody\r\n")
	fallback := time.Date(1980, 5, 5, 12, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	err = IngestRawMessage(
		t.Context(), st,
		src.ID, "owner@example.com", "",
		nil, "source-msg-no-date", "fakehash",
		raw, fallback, log,
	)
	require.NoError(err, "IngestRawMessage")

	var sentAt, internalDate sql.NullTime
	err = st.DB().QueryRowContext(
		t.Context(),
		`SELECT sent_at, internal_date FROM messages
		 WHERE source_message_id = ?`,
		"source-msg-no-date",
	).Scan(&sentAt, &internalDate)
	require.NoError(err, "query dates")
	assert.False(sentAt.Valid)
	require.True(internalDate.Valid)
	assert.Equal(fallback, internalDate.Time.UTC())
	assert.Contains(logs.String(), "ignored implausible email Date header")
	assert.Contains(logs.String(), "replacement_source=none")
}

func TestIngestRelayedAuthors(t *testing.T) {
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("test", "reader@example.test")
	require.NoError(t, err)
	for _, item := range []struct{ id, name, login string }{{"one", "Alice", "alice"}, {"two", "Bob", "bob"}} {
		raw := []byte("From: " + item.name + " <notifications@github.com>\r\nTo: reader@example.test\r\nX-GitHub-Sender: " + item.login + "\r\nAuthentication-Results: mx.example.test; dmarc=pass header.from=github.com\r\nSubject: Notice\r\n\r\nHello")
		require.NoError(t, IngestRawMessage(t.Context(), st, src.ID, src.Identifier, "", nil, item.id, item.id, raw, time.Time{}, slog.Default()))
		var value, address string
		var messageID int64
		err := st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT m.id, pi.identifier_value, mr.email_address FROM messages m JOIN participant_identifiers pi ON pi.participant_id=m.sender_id AND pi.identifier_type='github' JOIN message_recipients mr ON mr.message_id=m.id AND mr.recipient_type='from' WHERE m.source_message_id=?`), item.id).Scan(&messageID, &value, &address)
		require.NoError(t, err)
		assert.Equal(t, item.login, value)
		assert.Equal(t, "notifications@github.com", address)
		stored, err := st.GetMessageRawContext(t.Context(), messageID)
		require.NoError(t, err)
		assert.Equal(t, raw, stored)
	}
}
