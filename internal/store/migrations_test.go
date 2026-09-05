package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestIsMigrationApplied_NotApplied(t *testing.T) {
	f := storetest.New(t)

	applied, err := f.Store.IsMigrationApplied("test_migration")
	require.NoError(t, err, "IsMigrationApplied")
	assert.False(t, applied, "migration should not be applied yet")
}

func TestMarkAndCheckMigrationApplied(t *testing.T) {
	f := storetest.New(t)

	require.NoError(t, f.Store.MarkMigrationApplied("test_migration"), "MarkMigrationApplied")

	applied, err := f.Store.IsMigrationApplied("test_migration")
	require.NoError(t, err, "IsMigrationApplied")
	assert.True(t, applied, "migration should be marked as applied")
}

func TestMarkMigrationApplied_Idempotent(t *testing.T) {
	f := storetest.New(t)

	for range 2 {
		require.NoError(t, f.Store.MarkMigrationApplied("test_migration"), "MarkMigrationApplied")
	}

	applied, err := f.Store.IsMigrationApplied("test_migration")
	require.NoError(t, err, "IsMigrationApplied")
	assert.True(t, applied, "migration should be marked as applied after two calls")
}

func TestMigrationLedgerVersionLifecycle(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)

	const name = "versioned_migration"
	requirements.NoError(f.Store.MarkMigrationApplied(name, 3))

	for _, tc := range []struct {
		minimum int
		want    bool
	}{
		{minimum: 1, want: true},
		{minimum: 3, want: true},
		{minimum: 4, want: false},
	} {
		applied, err := f.Store.IsMigrationApplied(name, tc.minimum)
		requirements.NoError(err, "check minimum version %d", tc.minimum)
		assertions.Equal(tc.want, applied, "minimum version %d", tc.minimum)
	}

	var before string
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT applied_at FROM applied_migrations WHERE name = ?`), name).Scan(&before))
	requirements.NoError(f.Store.MarkMigrationApplied(name, 3), "same-version mark")
	var after string
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT applied_at FROM applied_migrations WHERE name = ?`), name).Scan(&after))
	assertions.Equal(before, after, "same-version mark must not rewrite applied_at")

	requirements.NoError(f.Store.MarkMigrationApplied(name, 2), "lower-version mark")
	var version int
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT version FROM applied_migrations WHERE name = ?`), name).Scan(&version))
	assertions.Equal(3, version, "lower-version mark must not downgrade the ledger")
}

func TestMigrationLedgerVersionRejectsInvalidValues(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{name: "zero check", call: func() error {
			_, err := f.Store.IsMigrationApplied("invalid_zero", 0)
			return err
		}},
		{name: "negative mark", call: func() error {
			return f.Store.MarkMigrationApplied("invalid_negative", -1)
		}},
		{name: "multiple versions", call: func() error {
			return f.Store.MarkMigrationApplied("invalid_multiple", 1, 2)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			assertions.Error(tc.call())
		})
	}

	before, err := f.Store.DerivedDataRevision()
	requirements.NoError(err)
	err = f.Store.MarkMigrationAppliedWithDerivedDataRevision("invalid_revision", 0)
	requirements.Error(err)
	after, err := f.Store.DerivedDataRevision()
	requirements.NoError(err)
	assertions.Equal(before, after, "invalid version must not bump derived-data revision")
}

func TestMigrationLedgerVersionWithDerivedDataRevision(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	const name = "derived_versioned_migration"

	before, err := f.Store.DerivedDataRevision()
	requirements.NoError(err)
	requirements.NoError(f.Store.MarkMigrationAppliedWithDerivedDataRevision(name, 4))
	after, err := f.Store.DerivedDataRevision()
	requirements.NoError(err)
	assertions.Equal(before+1, after, "successful derived-data mark must bump revision")

	var version int
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT version FROM applied_migrations WHERE name = ?`), name).Scan(&version))
	assertions.Equal(4, version)

	// Force the ledger write to fail after the revision bump has run. The
	// transaction must roll back both changes together.
	_, err = f.Store.DB().Exec(`DROP TABLE applied_migrations`)
	requirements.NoError(err)
	err = f.Store.MarkMigrationAppliedWithDerivedDataRevision("atomic_failure", 2)
	requirements.Error(err)
	rolledBack, err := f.Store.DerivedDataRevision()
	requirements.NoError(err)
	assertions.Equal(after, rolledBack, "failed ledger mark must roll back the revision bump")
}

func TestNameOnlyLedgerWriterDoesNotRegressVersion(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	const name = "direct_writer_migration"

	requirements.NoError(f.Store.MarkMigrationApplied(name, 7))
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`INSERT INTO applied_migrations (name) VALUES (?) ON CONFLICT DO NOTHING`), name)
	requirements.NoError(err, "direct version-1 writer")

	var version int
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT version FROM applied_migrations WHERE name = ?`), name).Scan(&version))
	assertions.Equal(7, version, "a name-only writer must not regress a higher version")
}

func TestArchiveIdentityMigrationIsRecorded(t *testing.T) {
	f := storetest.New(t)

	applied, err := f.Store.IsMigrationApplied("archive_identity_v1")
	require.NoError(t, err)
	assert.True(t, applied, "archive identity initialization must be a recorded migration")
}

func TestArchiveIdentityExistsInConfiguredDialect(t *testing.T) {
	f := storetest.New(t)
	uid, err := f.Store.ArchiveUID()
	require.NoError(t, err)
	assert.Len(t, uid, 64)
}

// TestInitSchemaMigratesListIDColumn simulates an archive created before
// messages.list_id existed, then proves the regular schema upgrade restores
// the column for the production upsert path.
func TestInitSchemaMigratesListIDColumn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, err := store.OpenForTest(filepath.Join(t.TempDir(), "legacy-list-id.db"))
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "initialize current schema")
	for _, trigger := range []string{
		"trg_messages_content_changed_at",
		"trg_person_sweep_messages_update",
	} {
		_, err = st.DB().Exec(`DROP TRIGGER IF EXISTS ` + trigger)
		require.NoErrorf(err, "remove trigger that references the legacy column: %s", trigger)
	}
	_, err = st.DB().Exec(`ALTER TABLE messages DROP COLUMN list_id`)
	require.NoError(err,
		"remove column to simulate a legacy archive")

	require.NoError(st.InitSchema(), "migrate legacy archive")

	source, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(err, "create source")
	conversationID, err := st.EnsureConversation(source.ID, "legacy-list-thread", "List announcement")
	require.NoError(err, "ensure conversation")
	_, err = st.UpsertMessage(&store.Message{
		SourceID:        source.ID,
		SourceMessageID: "legacy-list-message",
		ConversationID:  conversationID,
		MessageType:     store.MessageTypeEmail,
		ListID:          sql.NullString{String: "<announce.example.org>", Valid: true},
	})
	require.NoError(err, "upsert migrated message")

	var listID sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT list_id FROM messages WHERE source_id = ? AND source_message_id = ?`),
		source.ID, "legacy-list-message",
	).Scan(&listID), "read migrated list ID")
	assert.True(listID.Valid, "list ID should be present")
	assert.Equal("<announce.example.org>", listID.String, "list ID")
}

// previousV9MessagesContentChangedAtTrigger is the SQLite message watermark
// trigger before List-ID became content. Existing archives recorded the v9
// migration after installing this definition, so InitSchema must replace it
// through a newer trigger migration rather than only repairing fresh archives.
const previousV9MessagesContentChangedAtTrigger = `
CREATE TRIGGER trg_messages_content_changed_at
    AFTER UPDATE OF source_message_id, conversation_id, sender_id, message_type,
                    sent_at, received_at, internal_date, subject, snippet,
                    metadata, size_estimate, has_attachments, attachment_count,
                    deleted_at, deleted_from_source_at ON messages FOR EACH ROW
    WHEN OLD.content_changed_at IS NEW.content_changed_at AND
         (OLD.source_message_id IS NOT NEW.source_message_id OR
          OLD.conversation_id IS NOT NEW.conversation_id OR
          OLD.sender_id IS NOT NEW.sender_id OR
          OLD.message_type IS NOT NEW.message_type OR
          OLD.sent_at IS NOT NEW.sent_at OR
          OLD.received_at IS NOT NEW.received_at OR
          OLD.internal_date IS NOT NEW.internal_date OR
          OLD.subject IS NOT NEW.subject OR
          OLD.snippet IS NOT NEW.snippet OR
          OLD.metadata IS NOT NEW.metadata OR
          OLD.size_estimate IS NOT NEW.size_estimate OR
          OLD.has_attachments IS NOT NEW.has_attachments OR
          OLD.attachment_count IS NOT NEW.attachment_count OR
          OLD.deleted_at IS NOT NEW.deleted_at OR
          OLD.deleted_from_source_at IS NOT NEW.deleted_from_source_at)
    BEGIN
        UPDATE messages SET content_changed_at = STRFTIME('%Y-%m-%d %H:%M:%f', 'now')
        WHERE id = NEW.id;
    END`

func TestInitSchemaUpgradesV9WatermarkTriggersForListID(t *testing.T) {
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	messageID := seedMessage(t, st, 1)

	_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS trg_messages_content_changed_at`)
	require.NoError(err, "remove current message watermark trigger")
	_, err = st.DB().Exec(previousV9MessagesContentChangedAtTrigger)
	require.NoError(err, "install prior v9 message watermark trigger")
	_, err = st.DB().Exec(`
		DELETE FROM applied_migrations
		WHERE name IN ('message_and_attachment_triggers_v9', 'message_and_attachment_triggers_v10')`)
	require.NoError(err, "clear current watermark trigger migration records")
	_, err = st.DB().Exec(`
		INSERT INTO applied_migrations (name) VALUES ('message_and_attachment_triggers_v9')`)
	require.NoError(err, "record the prior watermark trigger migration")

	require.NoError(st.InitSchema(), "upgrade v9 watermark trigger definitions")

	before := stampContentChangedAt(t, st, messageID)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET list_id = ? WHERE id = ?`),
		"<upgraded-list.example.test>", messageID)
	require.NoError(err, "update List-ID after trigger migration")
	assert.NotEqual(t, before, readContentChangedAt(t, st, messageID),
		"upgrading a v9 archive must make List-ID updates advance content_changed_at")
}

// TestPostgresInitSchemaMigratesListIDColumn catches a PostgreSQL upgrade that
// omits messages.list_id: the normal email upsert must work after InitSchema
// restores the removed legacy column.
func TestPostgresInitSchemaMigratesListIDColumn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL legacy list ID migration regression")
	}

	// A pre-List-ID archive cannot have the current UPDATE OF list_id
	// triggers. Remove them and the migration record before removing the
	// column, so InitSchema must restore both the legacy column and the current
	// trigger definitions.
	for _, trigger := range []string{
		"trg_messages_content_changed_at",
		"trg_person_sweep_changes_message_update",
	} {
		_, err := f.Store.DB().Exec(`DROP TRIGGER IF EXISTS ` + trigger + ` ON messages`)
		require.NoErrorf(err, "remove trigger that references the legacy column: %s", trigger)
	}
	_, err := f.Store.DB().Exec(`
		DELETE FROM applied_migrations
		WHERE name = 'message_and_attachment_triggers_v10'`)
	require.NoError(err, "restore the pre-List-ID trigger migration state")
	_, err = f.Store.DB().Exec(`ALTER TABLE messages DROP COLUMN list_id`)
	require.NoError(err, "remove column to simulate a legacy PostgreSQL archive")
	require.NoError(f.Store.InitSchema(), "migrate legacy PostgreSQL archive")

	_, err = f.Store.UpsertMessage(&store.Message{
		SourceID:        f.Source.ID,
		SourceMessageID: "legacy-postgres-list-message",
		ConversationID:  f.ConvID,
		MessageType:     store.MessageTypeEmail,
		ListID:          sql.NullString{String: "<announce.example.org>", Valid: true},
	})
	require.NoError(err, "upsert migrated PostgreSQL message")

	var listID sql.NullString
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT list_id FROM messages WHERE source_id = ? AND source_message_id = ?`),
		f.Source.ID, "legacy-postgres-list-message",
	).Scan(&listID), "read migrated PostgreSQL list ID")
	assert.True(listID.Valid, "list ID should be present")
	assert.Equal("<announce.example.org>", listID.String, "list ID")
}
