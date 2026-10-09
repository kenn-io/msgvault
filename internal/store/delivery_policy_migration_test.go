package store_test

import (
	"context"
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"testing"
	"time"
)

const deliveryEmailEvidenceBackfillMigration = "delivery_source_email_evidence_v1"

func TestDeliveryPolicyMigrationAndBindingEpoch(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	st := f.Store
	var policies int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM delivery_policy_persons`).Scan(&policies))
	assertions.Zero(policies, "existing people inherit draft_only without stored approval")
	requirements.NoError(st.InitSchema())
	var singleton int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM delivery_admission_lock`).Scan(&singleton))
	assertions.Equal(1, singleton)
	epoch := func() int64 {
		var n int64
		requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT version FROM delivery_binding_versions WHERE kind='source' AND target_id=?`), f.Source.ID).Scan(&n))
		return n
	}
	before := epoch()
	_, err := st.DB().Exec(st.Rebind(`UPDATE sources SET last_sync_at=CURRENT_TIMESTAMP WHERE id=?`), f.Source.ID)
	requirements.NoError(err)
	assertions.Equal(before, epoch(), "freshness alone does not change source identity")
	_, err = st.DB().Exec(st.Rebind(`UPDATE sources SET identifier='alternate@example.test' WHERE id=?`), f.Source.ID)
	requirements.NoError(err)
	assertions.Greater(epoch(), before)
	_, err = st.DB().Exec(st.Rebind(`UPDATE sources SET identifier='test@example.com' WHERE id=?`), f.Source.ID)
	requirements.NoError(err)
	assertions.Greater(epoch(), before+1, "away/back must not revive permission")
}

func TestDeliveryPolicySQLiteReinstallsNativeEpochTriggers(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	st := f.Store
	if st.IsPostgreSQL() {
		t.Skip("SQLite trigger body replacement")
	}

	for _, statement := range []string{
		`DROP TRIGGER IF EXISTS delivery_sources_update_epoch`,
		`CREATE TRIGGER delivery_sources_update_epoch AFTER UPDATE ON sources BEGIN SELECT 1; END`,
		`DROP TRIGGER IF EXISTS delivery_marker_insert_epoch`,
		`CREATE TRIGGER delivery_marker_insert_epoch AFTER INSERT ON archive_metadata BEGIN SELECT 1; END`,
	} {
		_, err := st.DB().Exec(statement)
		requirements.NoError(err)
	}
	requirements.NoError(st.InitSchema())

	sourceEpoch := func() int64 {
		var version int64
		err := st.DB().QueryRow(st.Rebind(`
			SELECT COALESCE((SELECT version FROM delivery_binding_versions WHERE kind='source' AND target_id=?), 0)
		`), f.Source.ID).Scan(&version)
		requirements.NoError(err)
		return version
	}
	beforeSourceChange := sourceEpoch()
	_, err := st.DB().Exec(st.Rebind(`UPDATE sources SET identifier='replacement@example.test' WHERE id=?`), f.Source.ID)
	requirements.NoError(err)
	assertions.Greater(sourceEpoch(), beforeSourceChange, "InitSchema must replace a stale source epoch trigger")

	beforeMarker := sourceEpoch()
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO archive_metadata(key,value) VALUES (?,?)`),
		store.BeeperReanchorMarkerKey(f.Source.ID), "true")
	requirements.NoError(err)
	assertions.Greater(sourceEpoch(), beforeMarker, "InitSchema must replace a stale marker epoch trigger")
}

func TestDeliveryPolicyUpgradesArchiveWithoutFeatureTables(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f := storetest.New(t)
	st := f.Store
	if st.IsPostgreSQL() {
		t.Skip("SQLite legacy archive; PostgreSQL fresh and repeat initialization covered separately")
	}
	p, _, err := st.CreatePersonFromParticipant(f.EnsureParticipant("legacy@example.test", "Legacy Example Person", "example.test"))
	requirements.NoError(err)
	rows, err := st.DB().Query(`SELECT name FROM sqlite_master WHERE type='trigger' AND name LIKE 'delivery_%'`)
	requirements.NoError(err)
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		requirements.NoError(rows.Scan(&name))
		names = append(names, name)
	}
	requirements.NoError(rows.Err())
	for _, name := range names {
		_, err = st.DB().Exec(`DROP TRIGGER "` + name + `"`)
		requirements.NoError(err)
	}
	for _, table := range []string{"delivery_policy_audit", "delivery_policy_overrides", "delivery_policy_persons", "delivery_binding_versions", "delivery_admission_lock"} {
		_, err = st.DB().Exec(`DROP TABLE ` + table)
		requirements.NoError(err)
	}
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), deliveryEmailEvidenceBackfillMigration)
	requirements.NoError(err)
	requirements.NoError(st.InitSchema())
	state, err := st.GetDeliveryPolicyContext(t.Context(), store.DeliveryPolicyQuery{PersonUID: p.VCardUID})
	requirements.NoError(err)
	assertions.Equal(store.DeliveryDraftOnly, state.EffectivePolicy)
	assertions.Nil(state.StoredPolicy)
	assertions.Equal(p.ID, state.PersonID)
}

func TestDeliveryPolicyEmailEvidenceBackfillRunsOnce(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	participantID := f.EnsureParticipant("sender@example.test", "Synthetic Sender", "example.test")
	_, err := f.Store.UpsertMessage(&store.Message{
		SourceID:        f.Source.ID,
		SourceMessageID: "delivery-email-evidence-backfill",
		ConversationID:  f.ConvID,
		MessageType:     "email",
		SenderID:        sql.NullInt64{Int64: participantID, Valid: true},
	})
	requirements.NoError(err)

	clearMarker := func() {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
			`DELETE FROM applied_migrations WHERE name = ?`), deliveryEmailEvidenceBackfillMigration)
		requirements.NoError(err)
	}
	deleteEvidence := func() {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
			`DELETE FROM delivery_source_email_evidence WHERE participant_id = ? AND source_id = ?`), participantID, f.Source.ID)
		requirements.NoError(err)
	}
	assertEvidenceCount := func(want int) {
		var got int
		err := f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
			`SELECT COUNT(*) FROM delivery_source_email_evidence WHERE participant_id = ? AND source_id = ?`), participantID, f.Source.ID).Scan(&got)
		requirements.NoError(err)
		assertions.Equal(want, got)
	}
	assertEvidenceCount(1)
	if !f.Store.IsPostgreSQL() {
		var messageInsertTriggers int
		err := f.Store.DB().QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM sqlite_master
			WHERE type='trigger' AND name='delivery_source_email_message_insert'
		`).Scan(&messageInsertTriggers)
		requirements.NoError(err)
		assertions.Zero(messageInsertTriggers, "message inserts must not compile a SQLite trigger subprogram")
	}

	deleteEvidence()
	clearMarker()
	requirements.NoError(f.Store.InitSchema())
	assertEvidenceCount(1)

	deleteEvidence()
	requirements.NoError(f.Store.InitSchema())
	assertEvidenceCount(0)

	clearMarker()
	requirements.NoError(f.Store.InitSchema())
	assertEvidenceCount(1)
}

func TestDeliveryPolicyPostgresEmailEvidenceTriggerScopesUpdateColumns(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL trigger catalog contract")
	}

	rows, err := f.Store.DB().QueryContext(t.Context(), `
SELECT event_object_column
FROM information_schema.triggered_update_columns
WHERE trigger_schema = current_schema()
  AND trigger_name = 'delivery_source_email_message_write'
  AND event_object_table = 'messages'
ORDER BY event_object_column`)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()

	var columns []string
	for rows.Next() {
		var column string
		requirements.NoError(rows.Scan(&column))
		columns = append(columns, column)
	}
	requirements.NoError(rows.Err())
	assertions.Equal([]string{"deleted_from_source_at", "message_type", "sender_id", "source_id"}, columns,
		"unrelated message updates must not run the delivery evidence callback")
}

func TestDeliveryPolicyPostgresNonEmailWritesSkipEmailEvidenceRefresh(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL trigger lock contract")
	}

	participantID := f.EnsureParticipant("chat-member@example.test", "Synthetic Chat Member", "example.test")
	insertMessage := func(ctx context.Context, sourceMessageID string, senderID int64) (int64, error) {
		var messageID int64
		var senderIDArg any
		if senderID > 0 {
			senderIDArg = senderID
		}
		err := f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`
			INSERT INTO messages (conversation_id, source_id, source_message_id, message_type, sender_id)
			VALUES (?, ?, ?, 'beeper', ?)
			RETURNING id
		`), f.ConvID, f.Source.ID, sourceMessageID, senderIDArg).Scan(&messageID)
		return messageID, err
	}
	assertCompletesWithoutEvidenceTableRead := func(action func(context.Context) error) {
		t.Helper()
		blocker, err := f.Store.DB().BeginTx(t.Context(), nil)
		requirements.NoError(err)
		defer func() { _ = blocker.Rollback() }()
		_, err = blocker.ExecContext(t.Context(), `LOCK TABLE delivery_source_email_evidence IN ACCESS EXCLUSIVE MODE`)
		requirements.NoError(err)

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- action(ctx) }()
		select {
		case err := <-done:
			requirements.NoError(err)
		case <-time.After(5 * time.Second):
			requirements.NoError(blocker.Rollback())
			requirements.NoError(<-done, "release the evidence lock so a failed assertion cannot leak the write")
			requirements.FailNow("non-email writes must not query the delivery email evidence table")
		}
	}

	assertCompletesWithoutEvidenceTableRead(func(ctx context.Context) error {
		_, err := insertMessage(ctx, "non-email-sender", participantID)
		return err
	})

	messageID, err := insertMessage(t.Context(), "non-email-recipient", 0)
	requirements.NoError(err)
	assertCompletesWithoutEvidenceTableRead(func(ctx context.Context) error {
		_, err := f.Store.DB().ExecContext(ctx, f.Store.Rebind(`
			INSERT INTO message_recipients (message_id, participant_id, recipient_type)
			VALUES (?, ?, 'to')
		`), messageID, participantID)
		return err
	})
}

func TestDeliveryPolicyEmailEvidenceTracksMessageTypeTransitions(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	senderID := f.EnsureParticipant("transition-sender@example.test", "Synthetic Sender", "example.test")
	recipientID := f.EnsureParticipant("transition-recipient@example.test", "Synthetic Recipient", "example.test")
	messageID, err := f.Store.UpsertMessage(&store.Message{
		SourceID:        f.Source.ID,
		SourceMessageID: "delivery-message-type-transition",
		ConversationID:  f.ConvID,
		MessageType:     "beeper",
		SenderID:        sql.NullInt64{Int64: senderID, Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(f.Store.ReplaceMessageRecipients(messageID, "to", []int64{recipientID}, []string{"Synthetic Recipient"}))

	evidence := func(participantID int64) (bool, bool) {
		var rows, presentRows int
		err := f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
			SELECT COUNT(*), COALESCE(SUM(CASE WHEN evidence_present THEN 1 ELSE 0 END), 0)
			FROM delivery_source_email_evidence
			WHERE participant_id = ? AND source_id = ?
		`), participantID, f.Source.ID).Scan(&rows, &presentRows)
		requirements.NoError(err)
		return presentRows > 0, rows > 0
	}
	for _, participantID := range []int64{senderID, recipientID} {
		_, found := evidence(participantID)
		assertions.False(found, "non-email rows do not establish email evidence")
	}

	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type = 'email' WHERE id = ?`), messageID)
	requirements.NoError(err)
	for _, participantID := range []int64{senderID, recipientID} {
		present, found := evidence(participantID)
		assertions.True(found)
		assertions.True(present, "transition into email establishes sender and recipient evidence")
	}

	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type = 'beeper' WHERE id = ?`), messageID)
	requirements.NoError(err)
	for _, participantID := range []int64{senderID, recipientID} {
		present, found := evidence(participantID)
		assertions.True(found)
		assertions.False(present, "transition out of email removes sender and recipient evidence")
	}
}

func TestDeliveryPolicyConversationMetadataFenceUsesNativeColumnType(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := storetest.New(t)
	id, err := f.Store.EnsureConversationWithType(f.Source.ID, "synthetic-route", "dm", "Example conversation")
	requirements.NoError(err)
	epoch := func() int64 {
		var value int64
		requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT version FROM delivery_binding_versions WHERE kind='conversation' AND target_id=?`), id).Scan(&value))
		return value
	}
	requirements.NoError(f.Store.SetConversationMetadata(id, sql.NullString{String: `{"messaging_route":{"network":"matrix","observed_at":"2026-01-01T00:00:00Z"}}`, Valid: true}))
	before := epoch()
	requirements.NoError(f.Store.SetConversationMetadata(id, sql.NullString{String: `{"messaging_route":{"network":"matrix","observed_at":"2026-01-02T00:00:00Z"}}`, Valid: true}))
	assertions.Equal(before, epoch())
	requirements.NoError(f.Store.SetConversationMetadata(id, sql.NullString{String: `{"messaging_route":"unusable"}`, Valid: true}))
	assertions.Greater(epoch(), before)
}
