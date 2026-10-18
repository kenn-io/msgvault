package store

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identitycontrol"
)

func TestIdentityOperationReceiptInterruptedUpgradeKeepsPredecessor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, a, b := newUnlinkGuardStore(t)
	person, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	// Recording references are published archive4 data. Preserve their actual
	// native Store state across the identity receipt upgrade and its recovery.
	source, err := st.GetOrCreateSource("email", "schema-owner@example.test")
	requirements.NoError(err)
	conversation, err := st.EnsureConversationWithType(source.ID, "schema-recording", "email_thread", "Synthetic recording")
	requirements.NoError(err)
	message, err := st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "schema-recording", MessageType: "email"})
	requirements.NoError(err)
	requirements.NoError(st.ReconcileRecordingReferences(t.Context(), "synthetic-destination", message, true, []RecordingReferenceInput{{
		RouteKey: "synthetic-route", RefSHA256: "synthetic-reference-hash", OccurrenceJSON: `{"ref":"synthetic-recording"}`,
	}}))
	referenceTime := time.Now().Add(time.Hour)
	references, err := st.ClaimRecordingReferences(t.Context(), "synthetic-destination", referenceTime, 10)
	requirements.NoError(err)
	requirements.Len(references, 1)
	recordingTime := references[0].NextActionAt.UTC().Truncate(time.Millisecond)
	recordingResult := RecordingReferenceResult{
		State: "uncertain", ErrorCode: "synthetic-receipt-error", SourceID: "synthetic-recording-source",
		OccurrenceID: "synthetic-recording-occurrence", Outcome: "access_required", CoverageState: "unprocessed",
		LastSendAt: &recordingTime, RetryCount: 2, NextActionAt: recordingTime,
	}
	finished, err := st.FinishRecordingReference(t.Context(), references[0], recordingResult, true, nil)
	requirements.NoError(err)
	requirements.True(finished)
	references, err = st.ClaimRecordingReferences(t.Context(), "synthetic-destination", referenceTime, 10)
	requirements.NoError(err)
	requirements.Len(references, 1)
	wantRecordingMetadata := [4]string{recordingResult.SourceID, recordingResult.OccurrenceID, recordingResult.Outcome, recordingResult.CoverageState}
	var recordingMetadata [4]string
	var archiveUID string
	requirements.NoError(st.db.QueryRow(`SELECT value FROM archive_metadata WHERE key='archive_uid'`).Scan(&archiveUID))
	_, err = st.db.Exec(`DROP TABLE identity_operation_receipts`)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE archive_metadata SET value='4' WHERE key='schema_version'`)
	requirements.NoError(err)
	requirements.ErrorContains(st.ValidateSchemaContext(t.Context()), "run setup", "archive4 must not be certified as the identity receipt schema")
	// Interrupt the real initializer at its final completion marker, after its
	// native DDL has run. This leaves an actual partially upgraded predecessor.
	interrupt := `CREATE TRIGGER synthetic_interrupt_upgrade BEFORE UPDATE ON archive_metadata
 WHEN NEW.key='schema_version' BEGIN SELECT RAISE(ABORT, 'synthetic interrupted upgrade'); END`
	repair := `DROP TRIGGER synthetic_interrupt_upgrade`
	if st.IsPostgreSQL() {
		interrupt = `CREATE FUNCTION synthetic_interrupt_upgrade() RETURNS trigger LANGUAGE plpgsql AS $$
  BEGIN RAISE EXCEPTION 'synthetic interrupted upgrade'; END $$;
  CREATE TRIGGER synthetic_interrupt_upgrade BEFORE UPDATE ON archive_metadata
  FOR EACH ROW WHEN (NEW.key='schema_version') EXECUTE FUNCTION synthetic_interrupt_upgrade()`
		repair = `DROP TRIGGER synthetic_interrupt_upgrade ON archive_metadata; DROP FUNCTION synthetic_interrupt_upgrade()`
	}
	_, err = st.db.Exec(interrupt)
	requirements.NoError(err)
	requirements.ErrorContains(st.InitSchemaContext(t.Context()), "synthetic interrupted upgrade")
	version, err := st.SchemaVersionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(4, version, "partial DDL must not certify the new archive version")
	requirements.ErrorContains(st.ValidateSchemaContext(t.Context()), "run setup")
	var gotUID string
	requirements.NoError(st.db.QueryRow(`SELECT value FROM archive_metadata WHERE key='archive_uid'`).Scan(&gotUID))
	assertions.Equal(archiveUID, gotUID)
	current, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Equal(person.VCardUID, current.VCardUID)
	assertions.Equal([]int64{a}, current.ParticipantIDs)
	var receipts int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM identity_operation_receipts`).Scan(&receipts))
	assertions.Zero(receipts, "the receipt DDL preceded the interrupted completion marker")
	currentReferences, err := st.ClaimRecordingReferences(t.Context(), "synthetic-destination", referenceTime, 10)
	requirements.NoError(err)
	assertions.Equal(references, currentReferences)
	requirements.NoError(st.db.QueryRow(st.Rebind(`SELECT source_id,occurrence_id,outcome,coverage_state FROM recording_references WHERE destination_key=? AND message_id=? AND route_key=?`), references[0].DestinationKey, message, references[0].RouteKey).
		Scan(&recordingMetadata[0], &recordingMetadata[1], &recordingMetadata[2], &recordingMetadata[3]))
	assertions.Equal(wantRecordingMetadata, recordingMetadata)
	_, err = st.db.Exec(repair)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	requirements.NoError(st.ValidateSchemaContext(t.Context()))
	version, err = st.SchemaVersionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(SchemaVersion, version)
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-interrupted-upgrade")
	committed, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	recovered, err := st.IdentityOperationReceiptContext(t.Context(), request.Principal, request.IdempotencyKey)
	requirements.NoError(err)
	assertions.Equal(committed, recovered)
	currentReferences, err = st.ClaimRecordingReferences(t.Context(), "synthetic-destination", referenceTime, 10)
	requirements.NoError(err)
	assertions.Equal(references, currentReferences)
	requirements.NoError(st.db.QueryRow(st.Rebind(`SELECT source_id,occurrence_id,outcome,coverage_state FROM recording_references WHERE destination_key=? AND message_id=? AND route_key=?`), references[0].DestinationKey, message, references[0].RouteKey).
		Scan(&recordingMetadata[0], &recordingMetadata[1], &recordingMetadata[2], &recordingMetadata[3]))
	assertions.Equal(wantRecordingMetadata, recordingMetadata)
	requirements.NoError(st.db.QueryRow(`SELECT value FROM archive_metadata WHERE key='archive_uid'`).Scan(&gotUID))
	assertions.Equal(archiveUID, gotUID)
}

func TestIdentityOperationReceiptFutureArchiveRefusedBeforeDDL(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, a, _ := newUnlinkGuardStore(t)
	person, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	var archiveUID string
	requirements.NoError(st.db.QueryRow(`SELECT value FROM archive_metadata WHERE key='archive_uid'`).Scan(&archiveUID))
	_, err = st.db.Exec(`DROP TABLE identity_operation_receipts`)
	requirements.NoError(err)
	future := strconv.Itoa(SchemaVersion + 1)
	_, err = st.db.Exec(st.Rebind(`UPDATE archive_metadata SET value=? WHERE key='schema_version'`), future)
	requirements.NoError(err)
	_, err = st.db.Exec(`CREATE TABLE synthetic_upgrade_sentinel (value TEXT); INSERT INTO synthetic_upgrade_sentinel VALUES ('keep')`)
	requirements.NoError(err)
	requirements.ErrorContains(st.InitSchemaContext(t.Context()), "newer")
	query := `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='identity_operation_receipts'`
	if st.IsPostgreSQL() {
		query = `SELECT COUNT(*) FROM pg_class WHERE relname='identity_operation_receipts' AND relnamespace=current_schema()::regnamespace AND relkind='r'`
	}
	var tables int
	requirements.NoError(st.db.QueryRow(query).Scan(&tables))
	assertions.Zero(tables, "future archives must be rejected before feature DDL")
	var sentinel, marker, gotUID string
	requirements.NoError(st.db.QueryRow(`SELECT value FROM synthetic_upgrade_sentinel`).Scan(&sentinel))
	assertions.Equal("keep", sentinel)
	requirements.NoError(st.db.QueryRow(`SELECT value FROM archive_metadata WHERE key='schema_version'`).Scan(&marker))
	assertions.Equal(future, marker)
	requirements.NoError(st.db.QueryRow(`SELECT value FROM archive_metadata WHERE key='archive_uid'`).Scan(&gotUID))
	assertions.Equal(archiveUID, gotUID)
	current, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Equal(person.VCardUID, current.VCardUID)
	assertions.Equal([]int64{a}, current.ParticipantIDs)
}
