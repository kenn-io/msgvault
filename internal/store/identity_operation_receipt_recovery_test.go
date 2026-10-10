package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identitycontrol"
)

func TestIdentityOperationReceiptLookupIsPrincipalBoundAndReadOnly(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-lookup-key")
	committed, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	// Lookup does not depend on a preview token or its original revisions.
	readback, err := st.IdentityOperationReceiptContext(t.Context(), request.Principal, request.IdempotencyKey)
	requirements.NoError(err)
	assertions.Equal(committed, readback)
	_, err = st.IdentityOperationReceiptContext(t.Context(), "another-synthetic-principal", request.IdempotencyKey)
	requirements.ErrorIs(err, ErrIdentityOperationReceiptNotFound)
	revision, err := st.IdentityRevision()
	requirements.NoError(err)
	assertions.Equal(committed.AfterRevision, revision)
}

func TestIdentityOperationReceiptReadbackMatchesNativeCommittedState(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	person, _, err := st.CreatePersonFromParticipant(b)
	requirements.NoError(err)
	target := identitycontrol.IdentityTarget{ParticipantID: a, PersonID: person.ID}
	request := receiptRequest(t, st, identitycontrol.OperationPersonLink, target, "synthetic-readback-key")
	receipt, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	snapshot, err := st.IdentityOperationPreviewContext(t.Context(), request.Operation, target)
	requirements.NoError(err)
	assertions.Equal(receipt.AfterFingerprint, snapshot.Fingerprint)
	assertions.Equal(receipt.AfterRevision, snapshot.IdentityRevision)
}

func TestIdentityOperationReceiptUnchangedBindingIsNoop(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, _ := newUnlinkGuardStore(t)
	person, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	request := receiptRequest(t, st, identitycontrol.OperationPersonLink, identitycontrol.IdentityTarget{ParticipantID: a, PersonID: person.ID}, "synthetic-noop-key")
	receipt, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	assertions.False(receipt.Changed)
	assertions.Equal(receipt.BeforeRevision, receipt.AfterRevision)
	assertions.Equal(receipt.BeforeFingerprint, receipt.AfterFingerprint)
}

func TestIdentityOperationReceiptStalePersonRevisionDoesNotBind(t *testing.T) {
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	person, _, err := st.CreatePersonFromParticipant(b)
	requirements.NoError(err)
	request := receiptRequest(t, st, identitycontrol.OperationPersonLink, identitycontrol.IdentityTarget{ParticipantID: a, PersonID: person.ID}, "synthetic-person-stale-key")
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, new("Changed Synthetic Person"))
	requirements.NoError(err)
	_, err = st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.ErrorIs(err, ErrIdentityOperationStale)
	current, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assert.Equal(t, []int64{b}, current.ParticipantIDs)
}

func TestIdentityOperationReceiptNewOutOfGrantOccurrenceDeniesMutation(t *testing.T) {
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	source, err := st.GetOrCreateSource("gmail", "new-out-of-grant@example.test")
	requirements.NoError(err)
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-new-source-key")
	conversation, err := st.EnsureConversation(source.ID, "new-source-thread", "Synthetic New Source")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "new-source-message", MessageType: "email", SenderID: sql.NullInt64{Int64: a, Valid: true}})
	requirements.NoError(err)
	denied := errors.New("synthetic source not granted")
	_, err = st.ApplyIdentityOperationContext(t.Context(), request, func(_ context.Context, snapshot *IdentitySnapshot) error {
		for _, ownership := range snapshot.SourceContributions {
			if ownership.SourceID == source.ID {
				return denied
			}
		}
		return nil
	})
	requirements.ErrorIs(err, denied)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assert.Empty(t, edges)
}

func TestIdentityOperationReceiptSurvivesStoreRestart(t *testing.T) {
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-restart-key")
	committed, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	databasePath := st.dbPath
	requirements.NoError(st.Close())
	reopened, err := OpenForTest(databasePath)
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	readback, err := reopened.IdentityOperationReceiptContext(t.Context(), request.Principal, request.IdempotencyKey)
	requirements.NoError(err)
	assert.Equal(t, committed, readback)
}

func TestIdentityOperationReceiptOwnerLookupAfterPrincipalDenial(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-owner-reconcile-key")
	committed, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	denied := errors.New("synthetic issuing principal revoked")
	_, err = st.ApplyIdentityOperationContext(t.Context(), request, func(context.Context, *IdentitySnapshot) error { return denied })
	requirements.ErrorIs(err, denied)
	// The trusted native owner service can read the outcome after a current
	// principal denial. HTTP owner admission is tested at the service boundary.
	readback, err := st.IdentityReceiptByIDContext(t.Context(), committed.ID)
	requirements.NoError(err)
	assertions.Equal(committed, readback)
	revision, err := st.IdentityRevision()
	requirements.NoError(err)
	assertions.Equal(committed.AfterRevision, revision)
}

func TestIdentityOperationReceiptSchemaUpgradePreservesArchive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	// Model an existing archive before this feature, then use the production
	// schema initializer to add the receipt table while preserving archive rows.
	_, err := st.db.Exec(`DROP TABLE identity_operation_receipts`)
	requirements.NoError(err)
	requirements.NoError(st.InitSchemaContext(t.Context()))
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-schema-upgrade-key")
	receipt, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	readback, err := st.IdentityOperationReceiptContext(t.Context(), request.Principal, request.IdempotencyKey)
	requirements.NoError(err)
	assertions.Equal(receipt, readback)
	var participants int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM participants`).Scan(&participants))
	assertions.Equal(2, participants)
}

func TestIdentityOperationReceiptPreviewLeavesReceiptsEmpty(t *testing.T) {
	st, a, b := newUnlinkGuardStore(t)
	_, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b})
	require.NoError(t, err)
	var count int
	require.NoError(t, st.db.QueryRow(`SELECT COUNT(*) FROM identity_operation_receipts`).Scan(&count))
	assert.Zero(t, count)
}
