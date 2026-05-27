// Package dbtest provides shared database test helpers for seeding and querying
// test databases. It is designed to be importable from any test package without
// circular dependency issues (it does not import internal/query).
package dbtest

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"

	_ "github.com/mattn/go-sqlite3" // SQLite driver (database/sql)
	"github.com/stretchr/testify/require"
)

// StrPtr returns a pointer to a string (useful for optional fields in test opts).
//
//go:fix inline
func StrPtr(s string) *string { return new(s) }

// TestDB wraps a *sql.DB with auto-increment counters and builder helpers
// for seeding test data.
type TestDB struct {
	DB *sql.DB
	T  testing.TB

	nextParticipantID   int64
	nextMessageID       int64
	nextConversationSeq int64
}

// NewTestDB creates an in-memory SQLite database with the production schema loaded.
// schemaPath is the path to schema.sql (e.g. "../store/schema.sql" from the caller's package).
// The FTS table is dropped so tests start without it by default.
func NewTestDB(tb testing.TB, schemaPath string) *TestDB {
	tb.Helper()

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(tb, err, "open db")
	tb.Cleanup(func() { _ = db.Close() })

	schema, err := os.ReadFile(schemaPath)
	require.NoError(tb, err, "read schema.sql")

	_, err = db.Exec(string(schema))
	require.NoError(tb, err, "create schema")

	// Drop FTS table so non-FTS tests start clean.
	_, _ = db.Exec(`DROP TABLE IF EXISTS messages_fts`)

	return &TestDB{
		DB:                db,
		T:                 tb,
		nextParticipantID: 100,
		nextMessageID:     100,
	}
}

// SeedStandardDataSet inserts the standard test data set: 1 source (test@gmail.com),
// 3 participants (Alice, Bob, Carol), 1 conversation, 5 messages, 3 labels, and 3 attachments.
func (tdb *TestDB) SeedStandardDataSet() {
	tdb.T.Helper()

	testData := `
		-- Source account
		INSERT INTO sources (id, source_type, identifier, display_name) VALUES
			(1, 'gmail', 'test@gmail.com', 'Test Account');

		-- Participants
		INSERT INTO participants (id, email_address, display_name, domain) VALUES
			(1, 'alice@example.com', 'Alice Smith', 'example.com'),
			(2, 'bob@company.org', 'Bob Jones', 'company.org'),
			(3, 'carol@example.com', 'Carol White', 'example.com');

		-- Conversation
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES
			(1, 1, 'thread1', 'email_thread', 'Test Thread');

		-- Messages (3 from Alice, 2 from Bob)
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, subject, snippet, size_estimate, has_attachments, attachment_count) VALUES
			(1, 1, 1, 'msg1', 'email', '2024-01-15 10:00:00', 'Hello World', 'Preview 1', 1000, 0, 0),
			(2, 1, 1, 'msg2', 'email', '2024-01-16 11:00:00', 'Re: Hello', 'Preview 2', 2000, 1, 2),
			(3, 1, 1, 'msg3', 'email', '2024-02-01 09:00:00', 'Follow up', 'Preview 3', 1500, 0, 0),
			(4, 1, 1, 'msg4', 'email', '2024-02-15 14:00:00', 'Question', 'Preview 4', 3000, 1, 1),
			(5, 1, 1, 'msg5', 'email', '2024-03-01 16:00:00', 'Final', 'Preview 5', 500, 0, 0);

		-- Message bodies
		INSERT INTO message_bodies (message_id, body_text) VALUES
			(1, 'Message body 1'),
			(2, 'Message body 2'),
			(3, 'Message body 3'),
			(4, 'Message body 4'),
			(5, 'Message body 5');

		-- Message recipients (from)
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name) VALUES
			(1, 1, 'from', 'Alice'),
			(2, 1, 'from', 'Alice'),
			(3, 1, 'from', 'Alice'),
			(4, 2, 'from', 'Bob'),
			(5, 2, 'from', 'Bob');

		-- Message recipients (to)
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name) VALUES
			(1, 2, 'to', 'Bob'),
			(1, 3, 'to', 'Carol'),
			(2, 2, 'to', 'Bob'),
			(3, 2, 'to', 'Bob'),
			(4, 1, 'to', 'Alice'),
			(5, 1, 'to', 'Alice');

		-- Labels
		INSERT INTO labels (id, source_id, source_label_id, name, label_type) VALUES
			(1, 1, 'INBOX', 'INBOX', 'system'),
			(2, 1, 'IMPORTANT', 'IMPORTANT', 'system'),
			(3, 1, 'work', 'Work', 'user');

		-- Message labels
		INSERT INTO message_labels (message_id, label_id) VALUES
			(1, 1), (1, 3),
			(2, 1), (2, 2),
			(3, 1),
			(4, 1), (4, 3),
			(5, 1);

		-- Attachments
		INSERT INTO attachments (id, message_id, filename, mime_type, size, content_hash, storage_path) VALUES
			(1, 2, 'doc.pdf', 'application/pdf', 10000, 'hash1', 'ab/hash1'),
			(2, 2, 'image.png', 'image/png', 5000, 'hash2', 'cd/hash2'),
			(3, 4, 'report.xlsx', 'application/xlsx', 20000, 'hash3', 'ef/hash3');
	`

	_, err := tdb.DB.Exec(testData)
	require.NoError(tdb.T, err, "SeedStandardDataSet")
}

// MustLookupParticipant returns the ID of the participant with the given email,
// failing the test if not found.
func (tdb *TestDB) MustLookupParticipant(email string) int64 {
	tdb.T.Helper()
	var id int64
	err := tdb.DB.QueryRow("SELECT id FROM participants WHERE email_address = ?", email).Scan(&id)
	require.NoErrorf(tdb.T, err, "MustLookupParticipant(%q)", email)
	return id
}

// ---------------------------------------------------------------------------
// Builder helpers
// ---------------------------------------------------------------------------

// SourceOpts configures a source to insert.
type SourceOpts struct {
	Identifier  string // defaults to "other@gmail.com"
	DisplayName string // defaults to "Other Account"
	Type        string // defaults to "gmail"
}

// AddSource inserts a source and returns its ID.
func (tdb *TestDB) AddSource(opts SourceOpts) int64 {
	tdb.T.Helper()
	if opts.Type == "" {
		opts.Type = "gmail"
	}
	if opts.Identifier == "" {
		opts.Identifier = "other@gmail.com"
	}
	if opts.DisplayName == "" {
		opts.DisplayName = "Other Account"
	}
	res, err := tdb.DB.Exec(
		`INSERT INTO sources (source_type, identifier, display_name) VALUES (?, ?, ?)`,
		opts.Type, opts.Identifier, opts.DisplayName,
	)
	require.NoError(tdb.T, err, "AddSource")
	id, _ := res.LastInsertId()
	return id
}

// ConversationOpts configures a conversation to insert.
type ConversationOpts struct {
	SourceID int64  // defaults to 1
	Title    string // defaults to "Test Thread"
}

// AddConversation inserts a conversation and returns its ID.
func (tdb *TestDB) AddConversation(opts ConversationOpts) int64 {
	tdb.T.Helper()
	if opts.SourceID == 0 {
		opts.SourceID = 1
	}
	if opts.Title == "" {
		opts.Title = "Test Thread"
	}
	tdb.nextConversationSeq++
	sourceConvID := fmt.Sprintf("thread_%d_%d", opts.SourceID, tdb.nextConversationSeq)
	res, err := tdb.DB.Exec(
		`INSERT INTO conversations (source_id, source_conversation_id, conversation_type, title) VALUES (?, ?, 'email_thread', ?)`,
		opts.SourceID, sourceConvID, opts.Title,
	)
	require.NoError(tdb.T, err, "AddConversation")
	id, _ := res.LastInsertId()
	return id
}

// LabelOpts configures a label to insert.
type LabelOpts struct {
	SourceID      int64  // defaults to 1
	SourceLabelID string // defaults to Name
	Name          string // required
	Type          string // defaults to "user"
}

// AddLabel inserts a label and returns its ID.
func (tdb *TestDB) AddLabel(opts LabelOpts) int64 {
	tdb.T.Helper()
	require.NotEmpty(tdb.T, opts.Name, "AddLabel: Name is required")
	if opts.SourceID == 0 {
		opts.SourceID = 1
	}
	if opts.Type == "" {
		opts.Type = "user"
	}
	if opts.SourceLabelID == "" {
		opts.SourceLabelID = opts.Name
	}
	res, err := tdb.DB.Exec(
		`INSERT INTO labels (source_id, source_label_id, name, label_type) VALUES (?, ?, ?, ?)`,
		opts.SourceID, opts.SourceLabelID, opts.Name, opts.Type,
	)
	require.NoError(tdb.T, err, "AddLabel")
	id, _ := res.LastInsertId()
	return id
}

// AddMessageLabel associates a message with a label.
func (tdb *TestDB) AddMessageLabel(messageID, labelID int64) {
	tdb.T.Helper()
	_, err := tdb.DB.Exec(
		`INSERT INTO message_labels (message_id, label_id) VALUES (?, ?)`,
		messageID, labelID,
	)
	require.NoError(tdb.T, err, "AddMessageLabel")
}

// ParticipantOpts configures a participant to insert.
type ParticipantOpts struct {
	Email       *string // nil = NULL; use StrPtr("x") for a value
	DisplayName *string // nil = NULL
	Phone       *string // nil = NULL; E.164 format when set
	Domain      string
}

// AddParticipant inserts a participant and returns its ID.
func (tdb *TestDB) AddParticipant(opts ParticipantOpts) int64 {
	tdb.T.Helper()
	id := tdb.nextParticipantID
	tdb.nextParticipantID++

	var displayName any
	if opts.DisplayName != nil {
		displayName = *opts.DisplayName
	}

	var email any
	if opts.Email != nil {
		email = *opts.Email
	}

	var phone any
	if opts.Phone != nil {
		phone = *opts.Phone
	}

	_, err := tdb.DB.Exec(
		`INSERT INTO participants (id, email_address, display_name, phone_number, domain) VALUES (?, ?, ?, ?, ?)`,
		id, email, displayName, phone, opts.Domain,
	)
	require.NoError(tdb.T, err, "AddParticipant")
	return id
}

// LastMessageID returns the ID of the most recently added message.
func (tdb *TestDB) LastMessageID() int64 {
	return tdb.nextMessageID - 1
}

// MessageOpts configures a message to insert.
type MessageOpts struct {
	Subject        string
	SentAt         string // e.g. "2024-05-01 10:00:00"
	SizeEstimate   int
	HasAttachments bool
	FromID         int64   // participant ID for 'from' recipient; 0 = no from
	ToIDs          []int64 // participant IDs for 'to' recipients
	CcIDs          []int64 // participant IDs for 'cc' recipients
	BccIDs         []int64 // participant IDs for 'bcc' recipients
	SourceID       int64   // defaults to 1 if 0
	ConversationID int64   // defaults to 1 if 0
}

// AddMessage inserts a message with its from/to/cc recipients and returns the message ID.
func (tdb *TestDB) AddMessage(opts MessageOpts) int64 {
	tdb.T.Helper()
	id := tdb.nextMessageID
	tdb.nextMessageID++

	sourceMessageID := fmt.Sprintf("msg%d", id)
	sentAt := opts.SentAt
	if sentAt == "" {
		sentAt = "2024-05-01 10:00:00"
	}
	size := opts.SizeEstimate
	if size == 0 {
		size = 100
	}

	convID := opts.ConversationID
	if convID == 0 {
		convID = 1
	}
	srcID := opts.SourceID
	if srcID == 0 {
		// Look up the conversation's source_id to stay consistent.
		// Fall back to 1 if the conversation doesn't exist yet (e.g. FK checks off).
		err := tdb.DB.QueryRow(`SELECT source_id FROM conversations WHERE id = ?`, convID).Scan(&srcID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			srcID = 1
		case err != nil:
			require.NoErrorf(tdb.T, err, "AddMessage: lookup source_id for conversation %d", convID)
		}
	} else {
		// Verify the provided SourceID matches the conversation's source_id.
		var convSourceID int64
		err := tdb.DB.QueryRow(`SELECT source_id FROM conversations WHERE id = ?`, convID).Scan(&convSourceID)
		require.NoErrorf(tdb.T, err, "AddMessage: lookup source_id for conversation %d", convID)
		require.Equalf(tdb.T, srcID, convSourceID,
			"AddMessage: SourceID %d does not match conversation %d source_id %d", srcID, convID, convSourceID)
	}

	_, err := tdb.DB.Exec(
		`INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, subject, snippet, size_estimate, has_attachments) VALUES (?, ?, ?, ?, 'email', ?, ?, 'test', ?, ?)`,
		id, convID, srcID, sourceMessageID, sentAt, opts.Subject, size, opts.HasAttachments,
	)
	require.NoError(tdb.T, err, "AddMessage")

	if opts.FromID != 0 {
		_, err = tdb.DB.Exec(
			`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'from')`,
			id, opts.FromID,
		)
		require.NoError(tdb.T, err, "AddMessage from recipient")
	}

	for _, toID := range opts.ToIDs {
		_, err = tdb.DB.Exec(
			`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'to')`,
			id, toID,
		)
		require.NoError(tdb.T, err, "AddMessage to recipient")
	}

	for _, ccID := range opts.CcIDs {
		_, err = tdb.DB.Exec(
			`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'cc')`,
			id, ccID,
		)
		require.NoError(tdb.T, err, "AddMessage cc recipient")
	}

	for _, bccID := range opts.BccIDs {
		_, err = tdb.DB.Exec(
			`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'bcc')`,
			id, bccID,
		)
		require.NoError(tdb.T, err, "AddMessage bcc recipient")
	}

	return id
}

// EnableFTS creates the FTS5 virtual table and populates it from existing test data.
// Skips the test if FTS5 is not available in this SQLite build.
func (tdb *TestDB) EnableFTS() {
	tdb.T.Helper()
	_, _ = tdb.DB.Exec(`DROP TABLE IF EXISTS messages_fts`)

	_, err := tdb.DB.Exec(`
		CREATE VIRTUAL TABLE messages_fts USING fts5(message_id UNINDEXED, subject, body, from_addr, to_addr, cc_addr, tokenize='unicode61 remove_diacritics 1');
	`)
	if err != nil {
		tdb.T.Skipf("FTS5 not available in this SQLite build: %v", err)
	}

	_, err = tdb.DB.Exec(`
		INSERT INTO messages_fts (rowid, message_id, subject, body, from_addr, to_addr, cc_addr)
		SELECT m.id, m.id, COALESCE(m.subject, ''), COALESCE(mb.body_text, ''),
			COALESCE((SELECT p.email_address FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'from' LIMIT 1), ''),
			COALESCE((SELECT GROUP_CONCAT(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'to'), ''),
			COALESCE((SELECT GROUP_CONCAT(p.email_address, ' ') FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'cc'), '')
		FROM messages m
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
	`)
	require.NoError(tdb.T, err, "populate FTS")
}

// MarkDeletedByID marks a message as deleted by its internal ID.
func (tdb *TestDB) MarkDeletedByID(id int64) {
	tdb.T.Helper()
	_, err := tdb.DB.Exec("UPDATE messages SET deleted_from_source_at = datetime('now') WHERE id = ?", id)
	require.NoErrorf(tdb.T, err, "mark deleted by id %d", id)
}

// MarkDedupLoserByID sets deleted_at on a message, mirroring what
// store.Deduplicate does to soft-delete the losing duplicate row.
// User-facing read paths must always exclude these.
func (tdb *TestDB) MarkDedupLoserByID(id int64) {
	tdb.T.Helper()
	_, err := tdb.DB.Exec("UPDATE messages SET deleted_at = datetime('now') WHERE id = ?", id)
	require.NoErrorf(tdb.T, err, "mark dedup loser by id %d", id)
}

// MarkDeletedBySourceID marks a message as deleted by its source message ID.
func (tdb *TestDB) MarkDeletedBySourceID(sourceID string) {
	tdb.T.Helper()
	_, err := tdb.DB.Exec("UPDATE messages SET deleted_from_source_at = datetime('now') WHERE source_message_id = ?", sourceID)
	require.NoErrorf(tdb.T, err, "mark deleted by source id %s", sourceID)
}
