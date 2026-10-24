package store_test

import (
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
