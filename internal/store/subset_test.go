package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

// createTestSourceDB creates a source database with schema and test
// data. Returns the path to the database.
func createTestSourceDB(t *testing.T, dir string, msgCount int) string {
	t.Helper()

	dbPath := filepath.Join(dir, "msgvault.db")

	st, err := Open(dbPath)
	requirepkg.NoError(t, err, "Open")
	requirepkg.NoError(t, st.InitSchema(), "InitSchema")
	_ = st.Close()

	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=OFF")
	requirepkg.NoError(t, err, "open db")
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`INSERT INTO sources (id, source_type, identifier)
		VALUES (1, 'gmail', 'test@example.com')`)
	requirepkg.NoError(t, err, "insert source")

	_, err = db.Exec(`
		INSERT INTO participants
			(id, email_address, display_name, domain)
		VALUES
			(1, 'alice@example.com', 'Alice', 'example.com'),
			(2, 'bob@example.com', 'Bob', 'example.com'),
			(3, 'charlie@example.com', 'Charlie', 'example.com')`)
	requirepkg.NoError(t, err, "insert participants")

	_, err = db.Exec(`
		INSERT INTO participant_identifiers
			(id, participant_id, identifier_type, identifier_value)
		VALUES
			(1, 1, 'email', 'alice@example.com'),
			(2, 2, 'email', 'bob@example.com'),
			(3, 3, 'email', 'charlie@example.com')`)
	requirepkg.NoError(t, err, "insert participant_identifiers")

	_, err = db.Exec(`
		INSERT INTO conversations
			(id, source_id, conversation_type, title,
			 message_count, participant_count)
		VALUES
			(1, 1, 'email_thread', 'Thread 1', 5, 2),
			(2, 1, 'email_thread', 'Thread 2', 5, 2)`)
	requirepkg.NoError(t, err, "insert conversations")

	_, err = db.Exec(`
		INSERT INTO conversation_participants
			(conversation_id, participant_id)
		VALUES (1, 1), (1, 2), (2, 2), (2, 3)`)
	requirepkg.NoError(t, err, "insert conversation_participants")

	_, err = db.Exec(`
		INSERT INTO labels (id, source_id, name, label_type)
		VALUES
			(1, 1, 'INBOX', 'system'),
			(2, 1, 'SENT', 'system'),
			(3, 1, 'Work', 'user')`)
	requirepkg.NoError(t, err, "insert labels")

	for i := 1; i <= msgCount; i++ {
		convID := 1
		senderID := 1
		if i > msgCount/2 {
			convID = 2
			senderID = 2
		}

		_, err = db.Exec(`
			INSERT INTO messages
				(id, conversation_id, source_id, source_message_id,
				 message_type, sent_at, sender_id, subject)
			VALUES (?, ?, 1, ?,
				'email',
				datetime('2024-01-01', '+' || ? || ' hours'),
				?, ?)`,
			i, convID, fmt.Sprintf("msg_%d", i),
			i, senderID, "Subject "+string(rune('A'+i%26)))
		requirepkg.NoError(t, err, "insert message %d", i)

		_, err = db.Exec(
			`INSERT INTO message_bodies (message_id, body_text)
			 VALUES (?, ?)`,
			i, "Body of message "+string(rune('A'+i%26)))
		requirepkg.NoError(t, err, "insert message_body %d", i)

		_, err = db.Exec(
			`INSERT INTO message_recipients
				(message_id, participant_id, recipient_type)
			 VALUES (?, ?, 'from')`,
			i, senderID)
		requirepkg.NoError(t, err, "insert message_recipient from %d", i)

		toID := 2
		if senderID == 2 {
			toID = 3
		}
		_, err = db.Exec(
			`INSERT INTO message_recipients
				(message_id, participant_id, recipient_type)
			 VALUES (?, ?, 'to')`,
			i, toID)
		requirepkg.NoError(t, err, "insert message_recipient to %d", i)

		labelID := (i % 3) + 1
		_, err = db.Exec(
			`INSERT INTO message_labels (message_id, label_id)
			 VALUES (?, ?)`,
			i, labelID)
		requirepkg.NoError(t, err, "insert message_label %d", i)
	}

	return dbPath
}

func TestCopySubset_Basic(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 10)

	result, err := CopySubset(srcDB, dstDir, 5)
	require.NoError(err, "CopySubset")

	assert.Equal(int64(5), result.Messages, "Messages")

	db, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = db.Close() }()

	var count int64

	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM messages",
	).Scan(&count), "count messages")
	assert.Equal(int64(5), count, "destination messages")

	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM participants",
	).Scan(&count), "count participants")
	assert.NotZero(count, "expected participants to be copied")

	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM conversations",
	).Scan(&count), "count conversations")
	assert.NotZero(count, "expected conversations to be copied")

	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM labels",
	).Scan(&count), "count labels")
	assert.NotZero(count, "expected labels to be copied")

	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM message_labels",
	).Scan(&count), "count message_labels")
	assert.NotZero(count, "expected message_labels to be copied")

	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM message_bodies",
	).Scan(&count), "count message_bodies")
	assert.Equal(int64(5), count, "destination message_bodies")

	fkRows, err := db.Query("PRAGMA foreign_key_check")
	require.NoError(err)
	defer func() { _ = fkRows.Close() }()
	hasViolation := fkRows.Next()
	require.NoError(fkRows.Err(), "foreign_key_check rows")
	assert.False(hasViolation, "foreign key violations found in destination database")
}

func TestCopySubset_AllRows(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 5)

	result, err := CopySubset(srcDB, dstDir, 100)
	requirepkg.NoError(t, err, "CopySubset")

	assertpkg.Equal(t, int64(5), result.Messages, "Messages (all available)")
}

func TestCopySubset_FTSPopulated(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 5)

	_, err := CopySubset(srcDB, dstDir, 5)
	requirepkg.NoError(t, err, "CopySubset")

	db, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	requirepkg.NoError(t, err)
	defer func() { _ = db.Close() }()

	var count int64
	err = db.QueryRow("SELECT COUNT(*) FROM messages_fts").Scan(&count)
	if err != nil {
		t.Skip("FTS5 not available")
	}
	assertpkg.NotZero(t, count, "expected FTS index to be populated")
}

func TestCopySubset_ConversationCounts(t *testing.T) {
	require := requirepkg.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 10)

	_, err := CopySubset(srcDB, dstDir, 5)
	require.NoError(err, "CopySubset")

	db, err := sql.Open("sqlite3", filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`
		SELECT c.id, c.message_count,
			(SELECT COUNT(*) FROM messages m
			 WHERE m.conversation_id = c.id) AS actual_count
		FROM conversations c`)
	require.NoError(err)
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id, denormalized, actual int64
		require.NoError(rows.Scan(&id, &denormalized, &actual))
		assertpkg.Equal(t, actual, denormalized,
			"conversation %d: denormalized count=%d, actual=%d", id, denormalized, actual)
	}
	require.NoError(rows.Err(), "conversation rows")
}

func TestCopySubset_DestinationEmptyDir(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 5)

	require.NoError(os.MkdirAll(dstDir, 0755))

	result, err := CopySubset(srcDB, dstDir, 5)
	require.NoError(err, "CopySubset with pre-existing empty dir")

	assert.Equal(int64(5), result.Messages, "Messages")

	_, err = os.Stat(filepath.Join(dstDir, "msgvault.db"))
	assert.NoError(err, "msgvault.db not created")
}

func TestCopySubset_DestinationDBExists(t *testing.T) {
	require := requirepkg.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 5)

	require.NoError(os.MkdirAll(dstDir, 0755))
	require.NoError(os.WriteFile(
		filepath.Join(dstDir, "msgvault.db"), []byte("existing"), 0644,
	))

	_, err := CopySubset(srcDB, dstDir, 5)
	require.Error(err, "expected error when destination DB exists")
	assertpkg.ErrorContains(t, err, "destination database already exists")
}

func TestCopySubset_SQLInjectionInPath(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	quotedDir := filepath.Join(srcDir, "test'db")
	requirepkg.NoError(t, os.MkdirAll(quotedDir, 0755))
	srcDB := createTestSourceDB(t, quotedDir, 3)

	result, err := CopySubset(srcDB, dstDir, 3)
	requirepkg.NoError(t, err, "CopySubset with quoted path")
	assertpkg.Equal(t, int64(3), result.Messages, "Messages")
}

func TestCopySubset_NonPositiveRowCount(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		_, err := CopySubset("/tmp/fake.db", t.TempDir(), n)
		assertpkg.Error(t, err, "CopySubset(rowCount=%d) should error", n)
	}
}

func TestCopySubset_TimestampFallback(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	dbPath := filepath.Join(srcDir, "msgvault.db")
	st, err := Open(dbPath)
	require.NoError(err, "Open")
	require.NoError(st.InitSchema(), "InitSchema")
	_ = st.Close()

	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=OFF")
	require.NoError(err)

	_, err = db.Exec(`
		INSERT INTO sources (id, source_type, identifier)
		VALUES (1, 'gmail', 'test@example.com')`)
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO participants (id, email_address, domain)
		VALUES (1, 'alice@example.com', 'example.com')`)
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO conversations
			(id, source_id, conversation_type, title,
			 message_count, participant_count)
		VALUES (1, 1, 'email_thread', 'Thread', 3, 1)`)
	require.NoError(err)

	// msg 1: only received_at (no sent_at), most recent
	_, err = db.Exec(`
		INSERT INTO messages
			(id, conversation_id, source_id, source_message_id,
			 message_type, received_at, sender_id, subject)
		VALUES (1, 1, 1, 'msg_1', 'email', '2025-06-01', 1,
			'Received only')`)
	require.NoError(err)

	// msg 2: only internal_date (no sent_at), second most recent
	_, err = db.Exec(`
		INSERT INTO messages
			(id, conversation_id, source_id, source_message_id,
			 message_type, internal_date, sender_id, subject)
		VALUES (2, 1, 1, 'msg_2', 'email', '2025-05-01', 1,
			'Internal only')`)
	require.NoError(err)

	// msg 3: has sent_at, oldest
	_, err = db.Exec(`
		INSERT INTO messages
			(id, conversation_id, source_id, source_message_id,
			 message_type, sent_at, sender_id, subject)
		VALUES (3, 1, 1, 'msg_3', 'email', '2025-04-01', 1,
			'Sent only')`)
	require.NoError(err)

	for i := 1; i <= 3; i++ {
		_, err = db.Exec(`
			INSERT INTO message_recipients
				(message_id, participant_id, recipient_type)
			VALUES (?, 1, 'from')`, i)
		require.NoError(err)
	}
	_ = db.Close()

	// Request 2 most recent — should get msg 1 and 2 (by fallback
	// timestamps), not just msg 3 (the only one with sent_at).
	result, err := CopySubset(dbPath, dstDir, 2)
	require.NoError(err, "CopySubset")
	assert.Equal(int64(2), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	var subjects []string
	rows, err := dstDB.Query("SELECT subject FROM messages")
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s string
		require.NoError(rows.Scan(&s))
		subjects = append(subjects, s)
	}
	require.NoError(rows.Err(), "subject rows")

	for _, s := range subjects {
		assert.NotEqual("Sent only", s,
			"oldest message (sent_at only) should not be selected")
	}

	// last_message_at must use the fallback timestamp, not be NULL
	var lastMsg sql.NullString
	require.NoError(dstDB.QueryRow(
		"SELECT last_message_at FROM conversations",
	).Scan(&lastMsg))
	assert.True(lastMsg.Valid, "last_message_at is NULL; should use fallback timestamp")
}

func TestCopySubset_TieBreaker(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	dbPath := filepath.Join(srcDir, "msgvault.db")
	st, err := Open(dbPath)
	require.NoError(err, "Open")
	require.NoError(st.InitSchema(), "InitSchema")
	_ = st.Close()

	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=OFF")
	require.NoError(err)

	_, err = db.Exec(`
		INSERT INTO sources (id, source_type, identifier)
		VALUES (1, 'gmail', 'test@example.com')`)
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO participants (id, email_address, domain)
		VALUES (1, 'alice@example.com', 'example.com')`)
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO conversations
			(id, source_id, conversation_type, title,
			 message_count, participant_count)
		VALUES (1, 1, 'email_thread', 'Thread', 4, 1)`)
	require.NoError(err)

	// 4 messages with identical timestamps; higher IDs should win
	sameTime := "2025-06-01 12:00:00"
	for i := 1; i <= 4; i++ {
		_, err = db.Exec(`
			INSERT INTO messages
				(id, conversation_id, source_id, source_message_id,
				 message_type, sent_at, sender_id, subject)
			VALUES (?, 1, 1, ?, 'email', ?, 1, ?)`,
			i, fmt.Sprintf("msg_%d", i), sameTime,
			fmt.Sprintf("Msg %d", i))
		require.NoError(err, "insert message %d", i)
		_, err = db.Exec(`
			INSERT INTO message_recipients
				(message_id, participant_id, recipient_type)
			VALUES (?, 1, 'from')`, i)
		require.NoError(err)
	}
	_ = db.Close()

	// Select 2 of 4 — should get IDs 4 and 3 (highest IDs)
	result, err := CopySubset(dbPath, dstDir, 2)
	require.NoError(err, "CopySubset")
	assert.Equal(int64(2), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	rows, err := dstDB.Query(
		"SELECT id FROM messages ORDER BY id")
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(rows.Err(), "id rows")

	assert.Equal([]int64{3, 4}, ids, "selected IDs")
}

func TestCopySubset_ReplyToOrphanNulled(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	dbPath := filepath.Join(srcDir, "msgvault.db")
	st, err := Open(dbPath)
	require.NoError(err, "Open")
	require.NoError(st.InitSchema(), "InitSchema")
	_ = st.Close()

	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=OFF")
	require.NoError(err)

	_, err = db.Exec(`
		INSERT INTO sources (id, source_type, identifier)
		VALUES (1, 'gmail', 'test@example.com')`)
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO participants (id, email_address, domain)
		VALUES (1, 'alice@example.com', 'example.com')`)
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO conversations
			(id, source_id, conversation_type, title,
			 message_count, participant_count)
		VALUES (1, 1, 'email_thread', 'Thread', 2, 1)`)
	require.NoError(err)

	// Old parent message (won't be selected with limit 1)
	_, err = db.Exec(`
		INSERT INTO messages
			(id, conversation_id, source_id, source_message_id,
			 message_type, sent_at, sender_id, subject)
		VALUES (1, 1, 1, 'parent', 'email', '2020-01-01', 1,
			'Parent')`)
	require.NoError(err)

	// Recent reply referencing the parent
	_, err = db.Exec(`
		INSERT INTO messages
			(id, conversation_id, source_id, source_message_id,
			 message_type, sent_at, sender_id, subject,
			 reply_to_message_id)
		VALUES (2, 1, 1, 'reply', 'email', '2025-06-01', 1,
			'Reply', 1)`)
	require.NoError(err)

	for i := 1; i <= 2; i++ {
		_, err = db.Exec(`
			INSERT INTO message_recipients
				(message_id, participant_id, recipient_type)
			VALUES (?, 1, 'from')`, i)
		require.NoError(err)
	}
	_ = db.Close()

	// Select only 1 most recent — the reply, not the parent
	result, err := CopySubset(dbPath, dstDir, 1)
	require.NoError(err, "CopySubset")
	assert.Equal(int64(1), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	// reply_to_message_id should be nulled out since parent
	// wasn't included
	var replyTo sql.NullInt64
	require.NoError(dstDB.QueryRow(`
		SELECT reply_to_message_id FROM messages
		WHERE subject = 'Reply'`,
	).Scan(&replyTo))
	assert.False(replyTo.Valid,
		"reply_to_message_id = %d, want NULL (parent excluded)", replyTo.Int64)

	// FK integrity must pass
	fkRows, err := dstDB.Query("PRAGMA foreign_key_check")
	require.NoError(err)
	defer func() { _ = fkRows.Close() }()
	hasViolation := fkRows.Next()
	require.NoError(fkRows.Err(), "foreign_key_check rows")
	assert.False(hasViolation, "FK violations with orphaned reply_to_message_id")
}

func TestCopySubset_ExcludesSoftDeleted(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 10)

	// Soft-delete the 5 most recent messages
	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err)
	_, err = db.Exec(`
		UPDATE messages SET deleted_from_source_at = '2025-01-01'
		WHERE id IN (
			SELECT id FROM messages ORDER BY sent_at DESC LIMIT 5
		)`)
	require.NoError(err, "soft-delete messages")
	_ = db.Close()

	// Request 5 messages — should get the 5 non-deleted ones
	result, err := CopySubset(srcDB, dstDir, 5)
	require.NoError(err, "CopySubset")
	assert.Equal(int64(5), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	// None of the copied messages should be soft-deleted
	var deletedCount int64
	require.NoError(dstDB.QueryRow(`
		SELECT COUNT(*) FROM messages
		WHERE deleted_from_source_at IS NOT NULL`,
	).Scan(&deletedCount))
	assert.Equal(int64(0), deletedCount, "soft-deleted messages in subset")
}

func TestCopySubset_ReactionParticipants(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 5)

	// Add a reactor participant who is neither sender nor recipient
	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO participants
			(id, email_address, display_name, domain)
		VALUES (100, 'reactor@example.com', 'Reactor', 'example.com')`)
	require.NoError(err, "insert reactor")
	_, err = db.Exec(`
		INSERT INTO reactions
			(id, message_id, participant_id,
			 reaction_type, reaction_value)
		VALUES (1, 1, 100, 'emoji', 'thumbsup')`)
	require.NoError(err, "insert reaction")
	_ = db.Close()

	result, err := CopySubset(srcDB, dstDir, 5)
	require.NoError(err, "CopySubset")
	assert.Equal(int64(5), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	// Reactor participant must be present
	var reactorCount int64
	require.NoError(dstDB.QueryRow(`
		SELECT COUNT(*) FROM participants
		WHERE email_address = 'reactor@example.com'`,
	).Scan(&reactorCount))
	assert.Equal(int64(1), reactorCount, "reactor participant count")

	// Reaction must be present
	var rxnCount int64
	require.NoError(dstDB.QueryRow(
		"SELECT COUNT(*) FROM reactions",
	).Scan(&rxnCount))
	assert.Equal(int64(1), rxnCount, "reactions count")

	// FK integrity
	fkRows, err := dstDB.Query("PRAGMA foreign_key_check")
	require.NoError(err)
	defer func() { _ = fkRows.Close() }()
	hasViolation := fkRows.Next()
	require.NoError(fkRows.Err(), "foreign_key_check rows")
	assert.False(hasViolation, "FK violations with reaction participants")
}

// TestCopySubset_NullSourceIDLabels verifies that user-created labels
// with NULL source_id are preserved when attached to selected messages.
func TestCopySubset_NullSourceIDLabels(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 5)

	// Add a user-created label with NULL source_id and attach it
	// to message 1.
	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO labels (id, source_id, name, label_type)
		VALUES (100, NULL, 'My Custom Label', 'user')`)
	require.NoError(err, "insert null-source label")
	_, err = db.Exec(`
		INSERT INTO message_labels (message_id, label_id)
		VALUES (1, 100)`)
	require.NoError(err, "insert message_label")
	_ = db.Close()

	result, err := CopySubset(srcDB, dstDir, 5)
	require.NoError(err, "CopySubset")

	// The 3 source-scoped labels + 1 user-created label
	assert.Equal(int64(4), result.Labels, "Labels")

	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	var labelName string
	require.NoError(dstDB.QueryRow(`
		SELECT name FROM labels WHERE source_id IS NULL`,
	).Scan(&labelName), "query null-source label")
	assert.Equal("My Custom Label", labelName, "label name")

	// message_labels link must be preserved
	var mlCount int64
	require.NoError(dstDB.QueryRow(`
		SELECT COUNT(*) FROM message_labels WHERE label_id = 100`,
	).Scan(&mlCount))
	assert.Equal(int64(1), mlCount, "message_labels for label 100")

	// FK integrity
	fkRows, err := dstDB.Query("PRAGMA foreign_key_check")
	require.NoError(err)
	defer func() { _ = fkRows.Close() }()
	hasViolation := fkRows.Next()
	require.NoError(fkRows.Err(), "foreign_key_check rows")
	assert.False(hasViolation, "FK violations with null-source-id labels")
}

// TestCopySubset_SourceFKViolationIgnored verifies that pre-existing FK
// violations in the source DB (outside the copied subset) don't cause
// CopySubset to fail. This guards against the regression where src was
// still attached during PRAGMA foreign_key_check.
func TestCopySubset_SourceFKViolationIgnored(t *testing.T) {
	require := requirepkg.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	srcDB := createTestSourceDB(t, srcDir, 5)

	// Inject an FK violation in the source: a message_labels row
	// referencing a non-existent label_id.
	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err)
	_, err = db.Exec(`
		INSERT INTO message_labels (message_id, label_id)
		VALUES (1, 9999)`)
	require.NoError(err, "inject FK violation")
	_ = db.Close()

	// CopySubset should succeed — FK check must only scan destination
	result, err := CopySubset(srcDB, dstDir, 3)
	require.NoError(err, "CopySubset (source FK leak)")
	assertpkg.Equal(t, int64(3), result.Messages, "Messages")
}

func TestCopySubset_MissingSourceDB(t *testing.T) {
	assert := assertpkg.New(t)
	dstDir := filepath.Join(t.TempDir(), "dst")
	fakeSrc := filepath.Join(t.TempDir(), "nonexistent.db")

	_, err := CopySubset(fakeSrc, dstDir, 5)
	requirepkg.Error(t, err, "expected error for missing source DB")
	requirepkg.ErrorContains(t, err, "source database not found")

	// ATTACH on a missing path would create a file; verify it wasn't
	_, statErr := os.Stat(fakeSrc)
	assert.True(os.IsNotExist(statErr), "missing source path was created as a side effect")

	// Destination should be cleaned up
	_, statErr = os.Stat(dstDir)
	assert.True(os.IsNotExist(statErr), "destination directory was not cleaned up")
}

func TestCopySubset_MultiSourceScoping(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	dbPath := filepath.Join(srcDir, "msgvault.db")

	st, err := Open(dbPath)
	require.NoError(err, "Open")
	require.NoError(st.InitSchema(), "InitSchema")
	_ = st.Close()

	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=OFF")
	require.NoError(err, "open db")

	// Two sources: only source 1 will have recent messages
	_, err = db.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES
			(1, 'gmail', 'alice@example.com'),
			(2, 'gmail', 'bob@example.com')`)
	require.NoError(err, "insert sources")

	_, err = db.Exec(`
		INSERT INTO participants
			(id, email_address, display_name, domain)
		VALUES
			(1, 'alice@example.com', 'Alice', 'example.com'),
			(2, 'bob@example.com', 'Bob', 'example.com')`)
	require.NoError(err, "insert participants")

	_, err = db.Exec(`
		INSERT INTO conversations
			(id, source_id, conversation_type, title,
			 message_count, participant_count)
		VALUES
			(1, 1, 'email_thread', 'Alice thread', 2, 1),
			(2, 2, 'email_thread', 'Bob thread', 2, 1)`)
	require.NoError(err, "insert conversations")

	// Labels for both sources
	_, err = db.Exec(`
		INSERT INTO labels (id, source_id, name, label_type) VALUES
			(1, 1, 'INBOX', 'system'),
			(2, 1, 'Work', 'user'),
			(3, 2, 'INBOX', 'system'),
			(4, 2, 'Personal', 'user')`)
	require.NoError(err, "insert labels")

	// Source 1 messages: recent (will be selected)
	for i := 1; i <= 3; i++ {
		_, err = db.Exec(`
			INSERT INTO messages
				(id, conversation_id, source_id, source_message_id,
				 message_type, sent_at, sender_id, subject)
			VALUES (?, 1, 1, ?, 'email',
				datetime('2025-01-01', '+' || ? || ' hours'),
				1, ?)`,
			i, fmt.Sprintf("msg_%d", i), i,
			fmt.Sprintf("Alice msg %d", i))
		require.NoError(err, "insert alice message %d", i)
		_, err = db.Exec(
			`INSERT INTO message_recipients
				(message_id, participant_id, recipient_type)
			 VALUES (?, 1, 'from')`, i)
		require.NoError(err, "insert alice recipient %d", i)
	}

	// Source 2 messages: older (won't be selected with limit 3)
	for i := 4; i <= 6; i++ {
		_, err = db.Exec(`
			INSERT INTO messages
				(id, conversation_id, source_id, source_message_id,
				 message_type, sent_at, sender_id, subject)
			VALUES (?, 2, 2, ?, 'email',
				datetime('2020-01-01', '+' || ? || ' hours'),
				2, ?)`,
			i, fmt.Sprintf("msg_%d", i), i,
			fmt.Sprintf("Bob msg %d", i))
		require.NoError(err, "insert bob message %d", i)
		_, err = db.Exec(
			`INSERT INTO message_recipients
				(message_id, participant_id, recipient_type)
			 VALUES (?, 2, 'from')`, i)
		require.NoError(err, "insert bob recipient %d", i)
	}

	_ = db.Close()

	// Select only 3 most recent = all Alice, no Bob
	result, err := CopySubset(dbPath, dstDir, 3)
	require.NoError(err, "CopySubset")

	assert.Equal(int64(1), result.Sources, "Sources (only Alice's)")
	assert.Equal(int64(3), result.Messages, "Messages")

	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	// Only source 1 should be present
	var srcCount int64
	require.NoError(dstDB.QueryRow(
		"SELECT COUNT(*) FROM sources",
	).Scan(&srcCount))
	assert.Equal(int64(1), srcCount, "sources count")

	var identifier string
	require.NoError(dstDB.QueryRow(
		"SELECT identifier FROM sources",
	).Scan(&identifier))
	assert.Equal("alice@example.com", identifier, "source identifier")

	// Only source 1 labels should be present
	var labelCount int64
	require.NoError(dstDB.QueryRow(
		"SELECT COUNT(*) FROM labels",
	).Scan(&labelCount))
	assert.Equal(int64(2), labelCount, "labels count (Alice's labels only)")

	// No Bob conversations
	var convCount int64
	require.NoError(dstDB.QueryRow(
		"SELECT COUNT(*) FROM conversations",
	).Scan(&convCount))
	assert.Equal(int64(1), convCount, "conversations (Alice's only)")

	// FK integrity check
	fkRows, err := dstDB.Query("PRAGMA foreign_key_check")
	require.NoError(err)
	defer func() { _ = fkRows.Close() }()
	hasViolation := fkRows.Next()
	require.NoError(fkRows.Err(), "foreign_key_check rows")
	assert.False(hasViolation, "foreign key violations in multi-source subset")
}

func TestCopySubset_LegacySourceWithoutOAuthApp(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "dst")

	// Create a source DB, then drop the oauth_app column to simulate
	// a pre-oauth_app database.
	srcDB := createTestSourceDB(t, srcDir, 3)

	db, err := sql.Open("sqlite3", srcDB+"?_foreign_keys=OFF")
	require.NoError(err)
	// SQLite doesn't support DROP COLUMN before 3.35. Rebuild the
	// table without oauth_app to simulate an old schema.
	_, err = db.Exec(`
		CREATE TABLE sources_old AS
			SELECT id, source_type, identifier, display_name,
			       google_user_id, last_sync_at, sync_cursor,
			       sync_config, created_at, updated_at
			FROM sources;
		DROP TABLE sources;
		ALTER TABLE sources_old RENAME TO sources;
	`)
	require.NoError(err, "rebuild sources without oauth_app")
	_ = db.Close()

	// CopySubset should succeed with NULL oauth_app in destination
	result, err := CopySubset(srcDB, dstDir, 3)
	require.NoError(err, "CopySubset from legacy DB")
	assert.Equal(int64(3), result.Messages, "Messages")

	// Verify oauth_app is NULL in the destination
	dstDB, err := sql.Open("sqlite3",
		filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	defer func() { _ = dstDB.Close() }()

	var oauthApp sql.NullString
	require.NoError(dstDB.QueryRow(
		"SELECT oauth_app FROM sources",
	).Scan(&oauthApp), "query oauth_app")
	assert.False(oauthApp.Valid, "oauth_app = %q, want NULL", oauthApp.String)
}

func TestCopySubset_ControlCharInPath(t *testing.T) {
	dstDir := filepath.Join(t.TempDir(), "dst")
	base := t.TempDir()

	controlPaths := []string{
		filepath.Join(base, "test\ndb", "msgvault.db"),
		filepath.Join(base, "test\tdb", "msgvault.db"),
		filepath.Join(base, "test\x7Fdb", "msgvault.db"),
		filepath.Join(base, "test\x01db", "msgvault.db"),
	}
	for _, p := range controlPaths {
		_, err := CopySubset(p, dstDir, 5)
		assertpkg.Error(t, err, "CopySubset(%q) should reject control chars", p)
	}
}
