package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

// TestEnsureRecipientEnvelopeUniqueIndex_LegacyTableRebuild simulates an
// upgraded SQLite archive whose message_recipients still carries the
// table-level UNIQUE(message_id, participant_id, recipient_type) — enforced
// through an undroppable sqlite_autoindex — and proves the migration:
//
//  1. rebuilds the table without the table-level constraint, preserving
//     rows and ids,
//  2. recreates the plain indexes the DROP TABLE removed,
//  3. creates idx_message_recipients_envelope, under which the same
//     participant may carry several alias snapshots per message while an
//     exact (case-insensitive) repeat is still rejected,
//  4. marks the migration applied so the next run is a no-op.
func TestEnsureRecipientEnvelopeUniqueIndex_LegacyTableRebuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// SQLite-only: this test rebuilds the legacy table shape through
	// sqlite_master-visible DDL. On PostgreSQL the migration only drops a
	// nameable table constraint, and the alias-row behavior it enables is
	// covered by the merge and discovery tests on both backends.
	dbPath := filepath.Join(t.TempDir(), "envelope_unique.db")
	st, err := Open(dbPath)
	require.NoError(err, "Open")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "InitSchema")

	// Seed rows the rebuilt table's foreign keys will point at.
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(source.ID, "thread-envelope", "")
	require.NoError(err, "EnsureConversation")
	msgID, err := st.UpsertMessage(&Message{
		ConversationID:  convID,
		SourceID:        source.ID,
		SourceMessageID: "msg-envelope",
		MessageType:     "email",
		SizeEstimate:    100,
	})
	require.NoError(err, "UpsertMessage")
	participantID, err := st.EnsureParticipant("primary@example.test", "Primary", "example.test")
	require.NoError(err, "EnsureParticipant")

	// Roll back to the legacy shape: clear the ledger entry, then swap the
	// table for one declaring the old table-level UNIQUE. The plain indexes
	// vanish with the dropped table, exactly as they do during the real
	// migration's swap, so their recreation is exercised too.
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationRecipientEnvelopeUnique)
	require.NoError(err, "clear migration sentinel")
	for _, stmt := range []string{
		`ALTER TABLE message_recipients RENAME TO message_recipients_current`,
		`CREATE TABLE message_recipients (
			id INTEGER PRIMARY KEY,
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
			recipient_type TEXT NOT NULL,
			display_name TEXT,
			email_address TEXT,
			UNIQUE(message_id, participant_id, recipient_type)
		)`,
		`INSERT INTO message_recipients (id, message_id, participant_id, recipient_type, display_name, email_address)
			SELECT id, message_id, participant_id, recipient_type, display_name, email_address
			FROM message_recipients_current`,
		`DROP TABLE message_recipients_current`,
	} {
		_, err = st.db.Exec(stmt)
		require.NoError(err, "rebuild legacy table: %q", stmt)
	}

	var seededID int64
	require.NoError(st.db.QueryRow(`
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name, email_address)
		VALUES (?, ?, 'to', 'Primary', 'primary@example.test')
		RETURNING id
	`, msgID, participantID).Scan(&seededID), "seed legacy recipient row")

	// Prove the reconstructed constraint behaves like the legacy one: a
	// second envelope address for the same participant is rejected.
	_, err = st.db.Exec(`
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name, email_address)
		VALUES (?, ?, 'to', 'Primary', 'alias@example.test')
	`, msgID, participantID)
	require.Error(err, "legacy table-level UNIQUE must reject an alias row")

	// Run the migration we are testing.
	require.NoError(st.ensureRecipientEnvelopeUniqueIndex(context.Background()),
		"ensureRecipientEnvelopeUniqueIndex")

	// 1) The table-level UNIQUE (its autoindex) is gone and the expected
	//    indexes exist.
	var autoindexCount int
	require.NoError(st.db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'message_recipients' AND name LIKE 'sqlite_autoindex%'
	`).Scan(&autoindexCount), "count autoindexes")
	assert.Equal(0, autoindexCount, "table-level UNIQUE must be rebuilt away")
	for _, index := range []string{
		"idx_message_recipients_message",
		"idx_message_recipients_participant",
		"idx_message_recipients_envelope",
	} {
		var indexCount int
		require.NoError(st.db.QueryRow(`
			SELECT COUNT(*) FROM sqlite_master
			WHERE type = 'index' AND tbl_name = 'message_recipients' AND name = ?
		`, index).Scan(&indexCount), "count index %s", index)
		assert.Equal(1, indexCount, "index %s must exist after the rebuild", index)
	}

	// 2) The seeded row survived the rebuild with its id and data intact.
	var email string
	require.NoError(st.db.QueryRow(
		`SELECT email_address FROM message_recipients WHERE id = ?`, seededID,
	).Scan(&email), "read seeded row")
	assert.Equal("primary@example.test", email, "seeded row must survive the rebuild")

	// 3) An alias snapshot for the same participant now inserts, while a
	//    case variant of an existing snapshot is still rejected.
	_, err = st.db.Exec(`
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name, email_address)
		VALUES (?, ?, 'to', 'Primary', 'alias@example.test')
	`, msgID, participantID)
	require.NoError(err, "alias envelope row must insert after the migration")
	_, err = st.db.Exec(`
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name, email_address)
		VALUES (?, ?, 'to', 'Primary', 'ALIAS@example.test')
	`, msgID, participantID)
	require.Error(err, "case variant of an existing snapshot must still be rejected")

	// 4) Ledger marked; a second run is a no-op that changes nothing.
	applied, err := st.IsMigrationApplied(migrationRecipientEnvelopeUnique)
	require.NoError(err, "read migration ledger")
	assert.True(applied, "migration must be marked applied")
	require.NoError(st.ensureRecipientEnvelopeUniqueIndex(context.Background()),
		"second run must be a no-op")
	var rowCount int
	require.NoError(st.db.QueryRow(
		`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`, msgID,
	).Scan(&rowCount), "count recipient rows")
	assert.Equal(2, rowCount, "no-op rerun must not change row count")
}

func TestInitSchema_RepairsDanglingLegacyRecipients(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "dangling_recipients.db")

	seed, err := Open(dbPath)
	require.NoError(err, "open seed archive")
	require.NoError(seed.InitSchema(), "initialize seed archive")
	source, err := seed.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(err, "create source")
	conversationID, err := seed.EnsureConversation(source.ID, "thread-dangling-recipients", "")
	require.NoError(err, "create conversation")
	messageID, err := seed.UpsertMessage(&Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "msg-dangling-recipients",
		MessageType:     "email",
		SizeEstimate:    100,
	})
	require.NoError(err, "create message")
	participantID, err := seed.EnsureParticipant(
		"primary@example.test", "Primary", "example.test",
	)
	require.NoError(err, "create participant")

	var validRecipientID int64
	require.NoError(seed.db.QueryRow(`
		INSERT INTO message_recipients
			(message_id, participant_id, recipient_type, display_name, email_address)
		VALUES (?, ?, 'to', 'Primary', 'primary@example.test')
		RETURNING id
	`, messageID, participantID).Scan(&validRecipientID), "create valid recipient")
	require.NoError(seed.Close(), "close seed archive")

	legacy, err := sql.Open(
		sqliteutil.DriverName(), dbPath+"?_foreign_keys=OFF",
	)
	require.NoError(err, "open legacy archive with FK enforcement disabled")
	for _, stmt := range []string{
		`DELETE FROM applied_migrations
		 WHERE name = 'message_recipients_envelope_unique_index'`,
		`DROP INDEX IF EXISTS idx_message_recipients_envelope`,
		`ALTER TABLE message_recipients RENAME TO message_recipients_current`,
		`CREATE TABLE message_recipients (
			id INTEGER PRIMARY KEY,
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
			recipient_type TEXT NOT NULL,
			display_name TEXT,
			email_address TEXT,
			UNIQUE(message_id, participant_id, recipient_type)
		)`,
		`INSERT INTO message_recipients
			(id, message_id, participant_id, recipient_type, display_name, email_address)
		 SELECT id, message_id, participant_id, recipient_type, display_name, email_address
		 FROM message_recipients_current`,
		`DROP TABLE message_recipients_current`,
	} {
		_, err = legacy.Exec(stmt)
		require.NoError(err, "restore legacy recipient schema: %q", stmt)
	}

	missingMessageID := messageID + 1000
	missingParticipantID := participantID + 1000
	for _, recipient := range []struct {
		id            int64
		messageID     int64
		participantID int64
	}{
		{id: 9101, messageID: missingMessageID, participantID: participantID},
		{id: 9102, messageID: messageID, participantID: missingParticipantID},
		{id: 9103, messageID: missingMessageID + 1, participantID: missingParticipantID + 1},
	} {
		_, err = legacy.Exec(`
			INSERT INTO message_recipients
				(id, message_id, participant_id, recipient_type, display_name, email_address)
			VALUES (?, ?, ?, 'to', 'Dangling', NULL)
		`, recipient.id, recipient.messageID, recipient.participantID)
		require.NoError(err, "create dangling recipient %d", recipient.id)
	}
	require.NoError(legacy.Close(), "close legacy archive")

	logs := captureSlog(t)
	upgraded, err := Open(dbPath)
	require.NoError(err, "open legacy archive for upgrade")
	t.Cleanup(func() { _ = upgraded.Close() })
	require.NoError(upgraded.InitSchema(), "upgrade archive with dangling recipients")

	var (
		gotMessageID     int64
		gotParticipantID int64
		gotDisplayName   string
		gotEmailAddress  string
	)
	require.NoError(upgraded.db.QueryRow(`
		SELECT message_id, participant_id, display_name, email_address
		FROM message_recipients
		WHERE id = ?
	`, validRecipientID).Scan(
		&gotMessageID, &gotParticipantID, &gotDisplayName, &gotEmailAddress,
	), "read preserved recipient")
	assert.Equal(messageID, gotMessageID)
	assert.Equal(participantID, gotParticipantID)
	assert.Equal("Primary", gotDisplayName)
	assert.Equal("primary@example.test", gotEmailAddress)

	var danglingCount int
	require.NoError(upgraded.db.QueryRow(`
		SELECT COUNT(*) FROM message_recipients WHERE id IN (9101, 9102, 9103)
	`).Scan(&danglingCount), "count dangling recipients")
	assert.Zero(danglingCount)

	var autoindexCount int
	require.NoError(upgraded.db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index'
		  AND tbl_name = 'message_recipients'
		  AND name LIKE 'sqlite_autoindex%'
	`).Scan(&autoindexCount), "count legacy autoindexes")
	assert.Zero(autoindexCount)
	for _, index := range []string{
		"idx_message_recipients_message",
		"idx_message_recipients_participant",
		"idx_message_recipients_envelope",
	} {
		var indexCount int
		require.NoError(upgraded.db.QueryRow(`
			SELECT COUNT(*) FROM sqlite_master
			WHERE type = 'index' AND tbl_name = 'message_recipients' AND name = ?
		`, index).Scan(&indexCount), "count index %s", index)
		assert.Equal(1, indexCount, "index %s", index)
	}

	fkRows, err := upgraded.db.Query(`PRAGMA foreign_key_check`)
	require.NoError(err, "check foreign keys")
	require.False(fkRows.Next(), "upgraded archive must have no FK violations")
	require.NoError(fkRows.Err(), "iterate FK violations")
	require.NoError(fkRows.Close(), "close FK check")

	logOutput := logs.String()
	assert.Contains(logOutput, `"msg":"removed dangling message recipients during schema migration"`)
	assert.Contains(logOutput, `"table":"message_recipients"`)
	assert.Contains(logOutput, `"missing_message_rows":2`)
	assert.Contains(logOutput, `"missing_participant_rows":1`)

	require.NoError(upgraded.InitSchema(), "repeat schema initialization")
	assert.Equal(1, strings.Count(
		logs.String(), "removed dangling message recipients during schema migration",
	), "cleanup warning must be emitted only for the repairing run")
}

func TestInitSchema_LegacyRecipientRebuildRestoresActivityTriggers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "legacy_recipient_activity.db")

	seed, err := Open(dbPath)
	require.NoError(err, "open seed archive")
	require.NoError(seed.InitSchema(), "initialize seed archive")
	source, err := seed.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(err, "create source")
	conversationID, err := seed.EnsureConversation(
		source.ID, "thread-legacy-recipient-activity", "",
	)
	require.NoError(err, "create conversation")
	firstMessageID, err := seed.UpsertMessage(&Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "msg-legacy-recipient-activity-first",
		MessageType:     "email",
	})
	require.NoError(err, "create first message")
	secondMessageID, err := seed.UpsertMessage(&Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "msg-legacy-recipient-activity-second",
		MessageType:     "email",
	})
	require.NoError(err, "create second message")
	participantID, err := seed.EnsureParticipant(
		"recipient@example.test", "Recipient", "example.test",
	)
	require.NoError(err, "create participant")
	require.NoError(seed.Close(), "close seed archive")

	legacy, err := sql.Open(
		sqliteutil.DriverName(), dbPath+"?_foreign_keys=OFF",
	)
	require.NoError(err, "open legacy archive")
	for _, stmt := range []string{
		`DELETE FROM applied_migrations
		 WHERE name = 'message_recipients_envelope_unique_index'`,
		`DROP INDEX IF EXISTS idx_message_recipients_envelope`,
		`ALTER TABLE message_recipients RENAME TO message_recipients_current`,
		`CREATE TABLE message_recipients (
			id INTEGER PRIMARY KEY,
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
			recipient_type TEXT NOT NULL,
			display_name TEXT,
			email_address TEXT,
			UNIQUE(message_id, participant_id, recipient_type)
		)`,
		`INSERT INTO message_recipients
			(id, message_id, participant_id, recipient_type, display_name, email_address)
		 SELECT id, message_id, participant_id, recipient_type, display_name, email_address
		 FROM message_recipients_current`,
		`DROP TABLE message_recipients_current`,
	} {
		_, err = legacy.Exec(stmt)
		require.NoError(err, "restore legacy recipient schema: %q", stmt)
	}
	require.NoError(legacy.Close(), "close legacy archive")

	upgraded, err := Open(dbPath)
	require.NoError(err, "open legacy archive for upgrade")
	t.Cleanup(func() { _ = upgraded.Close() })
	require.NoError(upgraded.InitSchema(), "upgrade legacy archive")
	_, err = upgraded.db.Exec(`DELETE FROM activity_projection_queue`)
	require.NoError(err, "clear pre-upgrade activity work")

	revision := func(messageID int64) int64 {
		var got int64
		require.NoError(upgraded.db.QueryRow(`
			SELECT COALESCE((
				SELECT revision FROM activity_projection_queue WHERE message_id = ?
			), 0)
		`, messageID).Scan(&got), "read activity revision for message %d", messageID)
		return got
	}

	var recipientID int64
	require.NoError(upgraded.db.QueryRow(`
		INSERT INTO message_recipients
			(message_id, participant_id, recipient_type, display_name, email_address)
		VALUES (?, ?, 'to', 'Recipient', 'recipient@example.test')
		RETURNING id
	`, firstMessageID, participantID).Scan(&recipientID), "insert recipient")
	assert.Equal(int64(1), revision(firstMessageID), "insert must enqueue the new message")
	assert.Zero(revision(secondMessageID), "insert must not enqueue an unrelated message")

	_, err = upgraded.db.Exec(`
		UPDATE message_recipients SET message_id = ? WHERE id = ?
	`, secondMessageID, recipientID)
	require.NoError(err, "move recipient")
	assert.Equal(int64(2), revision(firstMessageID), "update must enqueue the old message")
	assert.Equal(int64(1), revision(secondMessageID), "update must enqueue the new message")

	_, err = upgraded.db.Exec(`DELETE FROM message_recipients WHERE id = ?`, recipientID)
	require.NoError(err, "delete recipient")
	assert.Equal(int64(2), revision(secondMessageID), "delete must enqueue the old message")
}

// TestEnsureRecipientEnvelopeUniqueIndex_PGLegacyConstraintDrop simulates an
// upgraded PostgreSQL archive whose message_recipients still carries the
// table-level UNIQUE constraint, and proves the migration discovers it by
// catalog name, drops it, and creates idx_message_recipients_envelope — after
// which one participant may carry several alias snapshots per message while a
// case variant of an existing snapshot is still rejected.
func TestEnsureRecipientEnvelopeUniqueIndex_PGLegacyConstraintDrop(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbURL := skipUnlessPostgresInternal(t)
	st := newPGStoreInternal(t, dbURL)

	// Roll back to the legacy shape: clear the ledger entry, drop the new
	// unique index, and reattach the old table-level UNIQUE. The constraint
	// name deliberately differs from the server-derived default so the test
	// proves the migration drops whatever name the catalog reports rather
	// than a hardcoded one.
	_, err := st.db.Exec(st.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), migrationRecipientEnvelopeUnique)
	require.NoError(err, "clear migration sentinel")
	_, err = st.db.Exec(`DROP INDEX IF EXISTS idx_message_recipients_envelope`)
	require.NoError(err, "drop envelope unique index")
	_, err = st.db.Exec(`
		ALTER TABLE message_recipients
		ADD CONSTRAINT legacy_recipients_unique UNIQUE (message_id, participant_id, recipient_type)
	`)
	require.NoError(err, "reattach legacy unique constraint")

	// Run the migration we are testing.
	require.NoError(st.ensureRecipientEnvelopeUniqueIndex(context.Background()),
		"ensureRecipientEnvelopeUniqueIndex")

	var constraintCount int
	require.NoError(st.db.QueryRow(`
		SELECT COUNT(*) FROM pg_constraint
		WHERE conrelid = 'message_recipients'::regclass AND contype = 'u'
	`).Scan(&constraintCount), "count unique constraints")
	assert.Equal(0, constraintCount, "legacy table-level UNIQUE must be dropped")
	var indexCount int
	require.NoError(st.db.QueryRow(`
		SELECT COUNT(*) FROM pg_indexes
		WHERE schemaname = current_schema()
		  AND tablename = 'message_recipients'
		  AND indexname = 'idx_message_recipients_envelope'
	`).Scan(&indexCount), "count envelope index")
	assert.Equal(1, indexCount, "envelope unique index must exist")

	// Alias snapshots for one participant now coexist; a case variant of an
	// existing snapshot is still rejected.
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(source.ID, "thread-envelope-pg", "")
	require.NoError(err, "EnsureConversation")
	msgID, err := st.UpsertMessage(&Message{
		ConversationID:  convID,
		SourceID:        source.ID,
		SourceMessageID: "msg-envelope-pg",
		MessageType:     "email",
		SizeEstimate:    100,
	})
	require.NoError(err, "UpsertMessage")
	participantID, err := st.EnsureParticipant("primary@example.test", "Primary", "example.test")
	require.NoError(err, "EnsureParticipant")

	insertRecipient := func(email string) error {
		_, err := st.db.Exec(st.Rebind(`
			INSERT INTO message_recipients (message_id, participant_id, recipient_type, display_name, email_address)
			VALUES (?, ?, 'to', 'Primary', ?)
		`), msgID, participantID, email)
		return err
	}
	require.NoError(insertRecipient("primary@example.test"), "insert primary snapshot")
	require.NoError(insertRecipient("alias@example.test"), "alias envelope row must insert after the migration")
	require.Error(insertRecipient("ALIAS@example.test"),
		"case variant of an existing snapshot must still be rejected")

	applied, err := st.IsMigrationApplied(migrationRecipientEnvelopeUnique)
	require.NoError(err, "read migration ledger")
	assert.True(applied, "migration must be marked applied")
}
