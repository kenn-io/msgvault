package store_test

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonSweepTriggerMigrationRepairsOldDefinition(t *testing.T) {
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	_, err := f.store.DB().Exec(f.store.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "person_sweep_change_triggers_v6")
	requirements.NoError(err)
	if f.store.IsPostgreSQL() {
		_, err = f.store.DB().Exec(`DROP TRIGGER IF EXISTS trg_person_sweep_changes_recipients ON message_recipients`)
		requirements.NoError(err)
		_, err = f.store.DB().Exec(`
			CREATE OR REPLACE FUNCTION msgvault_person_sweep_changes_recipients() RETURNS trigger AS $$
			BEGIN RETURN NEW; END $$ LANGUAGE plpgsql;
			CREATE TRIGGER trg_person_sweep_changes_recipients
			AFTER INSERT OR UPDATE OR DELETE ON message_recipients
			FOR EACH ROW EXECUTE FUNCTION msgvault_person_sweep_changes_recipients()`)
		requirements.NoError(err)
	} else {
		_, err = f.store.DB().Exec(`DROP TRIGGER IF EXISTS trg_person_sweep_recipients_insert`)
		requirements.NoError(err)
		_, err = f.store.DB().Exec(`
			CREATE TRIGGER trg_person_sweep_recipients_insert
			AFTER INSERT ON message_recipients FOR EACH ROW BEGIN SELECT 1; END`)
		requirements.NoError(err)
	}
	requirements.NoError(f.store.InitSchema())

	messageID := f.insertMessage(t, "trigger-repair", "email", f.bobID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	before := latestPersonSweepSequence(t, f.store)
	_, err = f.store.DB().Exec(f.store.Rebind(`
		INSERT INTO message_recipients (message_id, participant_id, recipient_type, email_address)
		VALUES (?, ?, 'to', ?)`), messageID, f.aliceID, "alice@example.test")
	requirements.NoError(err)

	changes := personSweepChangesAfter(t, f.store, f.alicePersonID, before)
	requirements.Len(changes, 1)
	assert.Equal(t, messageID, changes[0].MessageID)
}

func TestDropDocumentMessageTriggersInterruptedUpgradeKeepsMessageWrites(t *testing.T) {
	require := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL trigger function dependency")
	}
	id := f.insertMessage(t, "interrupted-document-upgrade", "email", f.aliceID, time.Now().UTC())
	schema, err := os.ReadFile("testdata/person_sweep_document_triggers_postgres.sql")
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), string(schema))
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), `
		DELETE FROM applied_migrations WHERE name IN ('drop_document_message_triggers_v1', 'person_sweep_change_triggers_v6');
		UPDATE archive_metadata SET value='11' WHERE key='schema_version';
		CREATE FUNCTION interrupt_document_upgrade() RETURNS trigger AS $$
		BEGIN
			IF NEW.name = 'drop_document_message_triggers_v1' THEN RAISE EXCEPTION 'interrupt after document trigger removal'; END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER interrupt_document_upgrade BEFORE INSERT ON applied_migrations
		FOR EACH ROW EXECUTE FUNCTION interrupt_document_upgrade();`)
	require.NoError(err)
	// The old caller works before the upgrade, with a tracked sender in scope.
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`), id)
	require.NoError(err)
	err = f.store.InitSchemaContext(t.Context())
	require.ErrorContains(err, "interrupt after document trigger removal")
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages SET deleted_at=NULL, snippet=? WHERE id=?`), "after", id)
	require.NoError(err, "message writes survive interruption between migrations")
	var snippet string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT snippet FROM messages WHERE id=?`), id).Scan(&snippet))
	assert.Equal(t, "after", snippet)
}
