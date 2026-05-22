package importer

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"

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
			if !utf8.ValidString(result) {
				t.Errorf(
					"normalizeMessageID(%q) produced invalid UTF-8: %q",
					tt.input, result,
				)
			}
		})
	}
}

func TestNormalizeMessageID_PreservesValidContent(t *testing.T) {
	result := normalizeMessageID("<valid@example.com>")
	if result != "valid@example.com" {
		t.Errorf("got %q, want %q", result, "valid@example.com")
	}
}

func TestIngestRawMessage_SanitizesAddressFields(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	src, err := st.GetOrCreateSource("test", "test@example.com")
	if err != nil {
		t.Fatalf("get/create source: %v", err)
	}

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
	if err != nil {
		t.Fatalf("IngestRawMessage: %v", err)
	}

	// Verify all participant fields are valid UTF-8
	db := st.DB()
	rows, err := db.Query(
		"SELECT email_address, display_name, domain FROM participants",
	)
	if err != nil {
		t.Fatalf("query participants: %v", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var emailAddr string
		var displayName sql.NullString
		var domain string
		if err := rows.Scan(
			&emailAddr, &displayName, &domain,
		); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !utf8.ValidString(emailAddr) {
			t.Errorf("invalid UTF-8 in email_address: %q", emailAddr)
		}
		if displayName.Valid && !utf8.ValidString(displayName.String) {
			t.Errorf(
				"invalid UTF-8 in display_name: %q",
				displayName.String,
			)
		}
		if !utf8.ValidString(domain) {
			t.Errorf("invalid UTF-8 in domain: %q", domain)
		}
	}

	// Verify conversation source_conversation_id is valid UTF-8
	rows2, err := db.Query(
		"SELECT source_conversation_id FROM conversations",
	)
	if err != nil {
		t.Fatalf("query conversations: %v", err)
	}
	defer func() { _ = rows2.Close() }()

	for rows2.Next() {
		var srcID string
		if err := rows2.Scan(&srcID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !utf8.ValidString(srcID) {
			t.Errorf(
				"invalid UTF-8 in source_conversation_id: %q", srcID,
			)
		}
	}
}

func TestIngestRawMessage_InvalidUTF8_RecipientLinkage(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	src, err := st.GetOrCreateSource("test", "test@example.com")
	if err != nil {
		t.Fatalf("get/create source: %v", err)
	}

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
	if err != nil {
		t.Fatalf("IngestRawMessage: %v", err)
	}

	db := st.DB()

	// Verify sender_id is set on the message.
	var senderID sql.NullInt64
	err = db.QueryRow(
		`SELECT sender_id FROM messages
		 WHERE source_message_id = ?`, "source-msg-linkage",
	).Scan(&senderID)
	if err != nil {
		t.Fatalf("query sender_id: %v", err)
	}
	if !senderID.Valid {
		t.Error("sender_id should be set, got NULL")
	}

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
		if err != nil {
			t.Fatalf("query recipients (%s): %v", rtype, err)
		}
		if count == 0 {
			t.Errorf(
				"expected at least 1 %s recipient, got 0",
				rtype,
			)
		}
	}

	// Verify display names are valid UTF-8 (sanitized).
	rows, err := db.Query(
		`SELECT display_name FROM participants
		 WHERE display_name IS NOT NULL`,
	)
	if err != nil {
		t.Fatalf("query display names: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !utf8.ValidString(name) {
			t.Errorf("invalid UTF-8 in display_name: %q", name)
		}
	}
}
