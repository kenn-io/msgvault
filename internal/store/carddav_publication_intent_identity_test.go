package store_test

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestCardDAVPendingPublicationPersistsIntentIdentityAcrossRestart(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, _, _, mapping := seededCardDAVConflictMapping(t)
	pending := prepareSyntheticReviewedMappedUpdate(t, st, mapping, "Synthetic Corrected")
	originalJSON, err := json.Marshal(pending)
	requirements.NoError(err)
	var identity struct{ PendingIntentID string }
	requirements.NoError(json.Unmarshal(originalJSON, &identity))
	requirements.NotEmpty(identity.PendingIntentID, "native pending intent needs a persisted identity independent of row-local revision and clock")
	reopened, err := store.OpenContext(t.Context(), store.DBPathForTest(st))
	requirements.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	requirements.NoError(reopened.InitSchema())
	again, err := reopened.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
	requirements.NoError(err)
	againJSON, err := json.Marshal(again)
	requirements.NoError(err)
	assertions.JSONEq(string(originalJSON), string(againJSON), "a new native store retains the exact original intent identity")
}

func TestLegacyCardDAVPendingPublicationKeepsOwnerRecoveryAfterIntentMigration(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, _, _, mapping := seededCardDAVConflictMapping(t)
	pending := prepareSyntheticReviewedMappedUpdate(t, st, mapping, "Synthetic Legacy")
	_, err := st.DB().ExecContext(t.Context(), `ALTER TABLE carddav_publications DROP COLUMN pending_intent_id`)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE archive_metadata SET value='3' WHERE key='schema_version'`)
	requirements.NoError(err)
	reopened, err := store.OpenContext(t.Context(), store.DBPathForTest(st))
	requirements.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	for range 2 {
		requirements.NoError(reopened.InitSchema())
	}
	legacy, err := reopened.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
	requirements.NoError(err)
	requirements.Empty(legacy.PendingIntentID, "migration must not invent identity for pre-existing ambiguous intent")
	pending.PendingIntentID = ""
	expectedJSON, err := json.Marshal(pending)
	requirements.NoError(err)
	legacyJSON, err := json.Marshal(legacy)
	requirements.NoError(err)
	assertions.JSONEq(string(expectedJSON), string(legacyJSON), "schema migration preserves all native legacy recovery evidence")
	backend, ok := any(reopened).(authorizedPublicationRefreshStore)
	requirements.True(ok)
	seen := false
	_, err = backend.RefreshCardDAVPublicationFenceAuthorizedContext(t.Context(), *legacy, func(_ context.Context, _ *store.IdentityGrantSelection) error { seen = true; return nil })
	requirements.ErrorIs(err, store.ErrCardDAVStalePlan)
	assertions.False(seen, "scoped recovery must not admit unidentified legacy intent")
	unchanged, err := reopened.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
	requirements.NoError(err)
	unchangedJSON, err := json.Marshal(unchanged)
	requirements.NoError(err)
	assertions.JSONEq(string(legacyJSON), string(unchangedJSON))
	refreshed, err := reopened.RefreshCardDAVPublicationFenceContext(t.Context(), pending.PersonID)
	requirements.NoError(err)
	requirements.NoError(reopened.RollbackCardDAVPublicationContext(t.Context(), refreshed))
	settled, err := reopened.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
	requirements.NoError(err)
	assertions.Empty(settled.PendingOperation, "owner can reconcile legacy pending evidence")
}

func TestCardDAVPublicationReceiptsUpgradePublishedArchive4(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, _, _, mapping := seededCardDAVConflictMapping(t)
	pending := prepareSyntheticReviewedMappedUpdate(t, st, mapping, "Synthetic Published Archive")
	source, err := st.GetOrCreateSource("gmail", "migration@example.test")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "synthetic-upgrade-thread", "Synthetic Upgrade")
	requirements.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversationID, SourceMessageID: "synthetic-recording-message", MessageType: "email"})
	requirements.NoError(err)
	input := store.RecordingReferenceInput{RouteKey: "synthetic-route", RefSHA256: "synthetic-reference-hash", OccurrenceJSON: `{"reference":"synthetic-archive"}`}
	requirements.NoError(st.ReconcileRecordingReferences(t.Context(), "synthetic-destination", messageID, true, []store.RecordingReferenceInput{input}))
	now := time.Now().UTC().Truncate(time.Millisecond)
	claims, err := st.ClaimRecordingReferences(t.Context(), "synthetic-destination", now.Add(time.Hour), 1)
	requirements.NoError(err)
	requirements.Len(claims, 1)
	sent, err := st.MarkRecordingReferenceSending(t.Context(), claims[0], now)
	requirements.NoError(err)
	requirements.True(sent)
	before, err := st.ClaimRecordingReferences(t.Context(), "synthetic-destination", now.Add(time.Hour), 1)
	requirements.NoError(err)
	requirements.Len(before, 1)
	requirements.Equal("uncertain", before[0].State)
	_, err = st.DB().ExecContext(t.Context(), `DROP TABLE carddav_publication_receipts`)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `ALTER TABLE carddav_publications DROP COLUMN pending_intent_id`)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE archive_metadata SET value='4' WHERE key='schema_version'`)
	requirements.NoError(err)
	reopened, err := store.OpenContext(t.Context(), store.DBPathForTest(st))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(reopened.Close()) })
	for range 2 {
		requirements.NoError(reopened.InitSchema())
	}
	version, err := reopened.SchemaVersionContext(t.Context())
	requirements.NoError(err)
	assertions.Greater(version, 4, "receipt storage must advance beyond the published recording-reference schema")
	assertions.Equal(store.SchemaVersion, version)
	var receipts, recordingReferences int
	requirements.NoError(reopened.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM carddav_publication_receipts`).Scan(&receipts))
	requirements.NoError(reopened.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM recording_references`).Scan(&recordingReferences))
	assertions.Zero(receipts)
	assertions.Equal(1, recordingReferences)
	after, err := reopened.ClaimRecordingReferences(t.Context(), "synthetic-destination", now.Add(time.Hour), 1)
	requirements.NoError(err)
	assertions.Equal(before, after, "published recording recovery evidence and its archive message survive the consumer upgrade")
	legacy, err := reopened.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
	requirements.NoError(err)
	assertions.Empty(legacy.PendingIntentID, "migration cannot invent receipt identity for an ambiguous legacy write")
	pending.PendingIntentID = ""
	expectedJSON, err := json.Marshal(pending)
	requirements.NoError(err)
	legacyJSON, err := json.Marshal(legacy)
	requirements.NoError(err)
	assertions.JSONEq(string(expectedJSON), string(legacyJSON))
	refreshed, err := reopened.RefreshCardDAVPublicationFenceContext(t.Context(), pending.PersonID)
	requirements.NoError(err)
	requirements.NoError(reopened.RollbackCardDAVPublicationContext(t.Context(), refreshed))
	settled, err := reopened.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
	requirements.NoError(err)
	assertions.Empty(settled.PendingOperation)
}

func TestCardDAVPublicationReceiptsUpgradeIdentityArchive5(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, _, _, mapping := seededCardDAVConflictMapping(t)
	pending := prepareSyntheticReviewedMappedUpdate(t, st, mapping, "Synthetic Identity Archive")
	identityReceipt := `{"id":"synthetic-identity-receipt","state":"applied"}`
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO identity_operation_receipts
		(receipt_id, principal, idempotency_key, request_hash, receipt_json)
		VALUES ('synthetic-identity-receipt', 'owner', 'synthetic-identity-key', 'synthetic-request-hash', ?)`), identityReceipt)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `DROP TABLE carddav_publication_receipts`)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `ALTER TABLE carddav_publications DROP COLUMN pending_intent_id`)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE archive_metadata SET value='5' WHERE key='schema_version'`)
	requirements.NoError(err)
	reopened, err := store.OpenContext(t.Context(), store.DBPathForTest(st))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(reopened.Close()) })
	for range 2 {
		requirements.NoError(reopened.InitSchemaContext(t.Context()))
	}
	requirements.NoError(reopened.ValidateSchemaContext(t.Context()))
	var retained string
	requirements.NoError(reopened.DB().QueryRowContext(t.Context(),
		`SELECT receipt_json FROM identity_operation_receipts WHERE receipt_id='synthetic-identity-receipt'`).Scan(&retained))
	assertions.Equal(identityReceipt, retained, "the predecessor's identity receipt survives publication migration unchanged")
	var receipts int
	requirements.NoError(reopened.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM carddav_publication_receipts`).Scan(&receipts))
	assertions.Zero(receipts)
	legacy, err := reopened.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
	requirements.NoError(err)
	requirements.NotNil(legacy)
	assertions.Empty(legacy.PendingIntentID, "migration must not invent approval evidence for a legacy pending update")
}
