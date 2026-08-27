package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestPersonSweepTriggerMigrationRepairsOldDefinition(t *testing.T) {
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	_, err := f.store.DB().Exec(f.store.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "person_sweep_change_triggers_v5")
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

func TestPersonSweepTriggerMigrationV5RepairsV4DocumentLifecycleDefinition(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f, personID := newPersonSweepDocumentFixture(t)
	_, err := f.Store.SetPersonTrackingContext(t.Context(), personID, true)
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		INSERT INTO applied_migrations (name) VALUES (?)
		ON CONFLICT (name) DO NOTHING`), "person_sweep_change_triggers_v4")
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "person_sweep_change_triggers_v5")
	requirements.NoError(err)
	if f.Store.IsPostgreSQL() {
		_, err = f.Store.DB().Exec(`
			CREATE OR REPLACE FUNCTION msgvault_append_person_sweep_document_changes(
			    _person_id BIGINT, _source_id BIGINT, _message_id BIGINT,
			    _change_kind TEXT, _evidence_effect TEXT)
			RETURNS VOID AS $$ BEGIN RETURN; END $$ LANGUAGE plpgsql`)
		requirements.NoError(err)
	} else {
		_, err = f.Store.DB().Exec(`DROP TRIGGER IF EXISTS trg_person_sweep_documents_message_update`)
		requirements.NoError(err)
	}
	requirements.NoError(f.Store.InitSchema())

	var messageID, sourceID, attachmentID int64
	var occurrenceKey string
	requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), `
		SELECT message_id,source_id,attachment_id,occurrence_key
		FROM document_occurrences LIMIT 1`).Scan(
		&messageID, &sourceID, &attachmentID, &occurrenceKey))
	before := latestPersonSweepSequence(t, f.Store)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
	requirements.NoError(err)

	var documentChanges []peoplesweep.ArchiveChange
	for _, change := range personSweepChangesAfter(t, f.Store, personID, before) {
		if change.SourceLane == peoplesweep.SourceDocumentText {
			documentChanges = append(documentChanges, change)
		}
	}
	requirements.Len(documentChanges, 1)
	checks.Equal(peoplesweep.EvidenceEffectSourceDeleted, documentChanges[0].EvidenceEffect)
	checks.Equal(sourceID, documentChanges[0].SourceID)
	checks.Equal(messageID, documentChanges[0].MessageID)
	checks.Equal(attachmentID, documentChanges[0].AttachmentID)
	checks.Equal(occurrenceKey, documentChanges[0].OccurrenceKey)
}
