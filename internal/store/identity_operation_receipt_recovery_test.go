package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
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
	_, err = st.db.Exec(`UPDATE archive_metadata SET value='3' WHERE key='schema_version'`)
	requirements.NoError(err)
	requirements.ErrorContains(st.ValidateSchemaContext(t.Context()), "run setup")
	requirements.NoError(st.InitSchemaContext(t.Context()))
	requirements.NoError(st.InitSchemaContext(t.Context()), "repeated successful setup remains idempotent")
	requirements.NoError(st.ValidateSchemaContext(t.Context()))
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

func TestIdentityOperationReceiptBindsOriginalAuthority(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, a, b := newUnlinkGuardStore(t)
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-authority-bound")
	request.AuthorityFingerprint = strings.Repeat("a", 64)
	allow := func(context.Context, *IdentitySnapshot) error { return nil }
	committed, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, allow, allow)
	requirements.NoError(err)
	saved, err := st.IdentityOperationReceiptAuthorizedContext(t.Context(), request.Principal, request.IdempotencyKey, request.AuthorityFingerprint, allow)
	requirements.NoError(err)
	assertions.Equal(committed, saved)
	request.AuthorityFingerprint = strings.Repeat("b", 64)
	_, err = st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, allow, allow)
	requirements.ErrorIs(err, ErrIdentityOperationIdempotency)
	_, err = st.IdentityOperationReceiptAuthorizedContext(t.Context(), request.Principal, request.IdempotencyKey, request.AuthorityFingerprint, allow)
	requirements.ErrorIs(err, ErrIdentityOperationReceiptDenied)
}

func TestIdentityOperationReceiptSurvivesSourceCleanupAndRejectsOwnerReuse(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, a, b := newUnlinkGuardStore(t)
	source, err := st.GetOrCreateSource("gmail", "identity-source@example.test")
	requirements.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "synthetic-cleanup-thread", "Synthetic Thread")
	requirements.NoError(err)
	for i, participant := range []int64{a, b} {
		_, err = st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: strconv.Itoa(i), MessageType: "email", SenderID: sql.NullInt64{Int64: participant, Valid: true}})
		requirements.NoError(err)
	}
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-cleanup-receipt")
	request.AuthorityFingerprint = strings.Repeat("c", 64)
	allow := func(context.Context, *IdentitySnapshot) error { return nil }
	receipt, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, allow, allow)
	requirements.NoError(err)
	var encoded string
	requirements.NoError(st.db.QueryRow(`SELECT receipt_json FROM identity_operation_receipts WHERE receipt_id=?`, receipt.ID).Scan(&encoded))
	assertions.NotContains(encoded, "identity-source@example.test")
	assertions.NotContains(encoded, "Synthetic A")
	_, err = st.db.Exec(`UPDATE sources SET google_user_id=? WHERE id=?`, "synthetic-new-owner", source.ID)
	requirements.NoError(err)
	_, err = st.IdentityOperationReceiptAuthorizedContext(t.Context(), request.Principal, request.IdempotencyKey, request.AuthorityFingerprint, allow)
	requirements.ErrorIs(err, ErrIdentityOperationReceiptDenied)
	_, err = st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, allow, allow)
	requirements.ErrorIs(err, ErrIdentityOperationReceiptDenied)
	_, _, err = st.RemoveSourceSerialized(t.Context(), source.ID)
	requirements.NoError(err)
	saved, err := st.IdentityReceiptByIDContext(t.Context(), receipt.ID)
	requirements.NoError(err)
	assertions.Equal(receipt, saved)
	_, err = st.IdentityOperationReceiptAuthorizedContext(t.Context(), request.Principal, request.IdempotencyKey, request.AuthorityFingerprint, allow)
	requirements.ErrorIs(err, ErrIdentityOperationReceiptDenied)
}

func TestIdentityOperationReceiptSurvivesCalendarMetadataRefresh(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st, a, b := newUnlinkGuardStore(t)
	source, err := st.GetOrCreateSource("gcal", "synthetic-calendar-id")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(source.ID,
		`{"account_email":"owner@example.test","calendar_id":"group@group.calendar.google.com","calendar_summary":"Old title","time_zone":"America/Vancouver"}`))
	conversation, err := st.EnsureConversation(source.ID, "synthetic-calendar-thread", "Synthetic Calendar")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conversation,
		SourceMessageID: "synthetic-calendar-event", MessageType: "calendar_event",
		SenderID: sql.NullInt64{Int64: a, Valid: true}})
	requirements.NoError(err)
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink,
		identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-calendar-metadata-receipt")
	request.AuthorityFingerprint = strings.Repeat("d", 64)
	allow := func(context.Context, *IdentitySnapshot) error { return nil }
	committed, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, allow, allow)
	requirements.NoError(err)

	requirements.NoError(st.UpdateSourceSyncConfig(source.ID,
		`{"account_email":"owner@example.test","calendar_id":"group@group.calendar.google.com","calendar_summary":"Renamed title","time_zone":"Europe/London"}`))
	recovered, err := st.IdentityOperationReceiptAuthorizedContext(t.Context(), request.Principal,
		request.IdempotencyKey, request.AuthorityFingerprint, allow)
	requirements.NoError(err, "descriptive calendar metadata must not invalidate receipt authority")
	assertions.Equal(committed, recovered)
	replayed, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, allow, allow)
	requirements.NoError(err, "an exact committed retry must recover after descriptive calendar metadata changes")
	assertions.Equal(committed, replayed)

	requirements.NoError(st.UpdateSourceSyncConfig(source.ID,
		`{"account_email":"new-owner@example.test","calendar_id":"group@group.calendar.google.com","calendar_summary":"Renamed title","time_zone":"Europe/London"}`))
	_, err = st.IdentityOperationReceiptAuthorizedContext(t.Context(), request.Principal,
		request.IdempotencyKey, request.AuthorityFingerprint, allow)
	requirements.ErrorIs(err, ErrIdentityOperationReceiptDenied,
		"a change to the source owner must still invalidate saved authority")

	requirements.NoError(st.UpdateSourceSyncConfig(source.ID,
		`{"account_email":"owner@example.test","calendar_id":"replacement@group.calendar.google.com","calendar_summary":"Renamed title","time_zone":"Europe/London"}`))
	_, err = st.IdentityOperationReceiptAuthorizedContext(t.Context(), request.Principal,
		request.IdempotencyKey, request.AuthorityFingerprint, allow)
	requirements.ErrorIs(err, ErrIdentityOperationReceiptDenied,
		"a change to the calendar resource must still invalidate saved authority")
}

func TestIdentityOperationReceiptRecoversAfterComponentShrinkage(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st, a, b := newUnlinkGuardStore(t)
	c, err := st.EnsureParticipant("identity-c@example.test", "Synthetic C", "example.test")
	requirements.NoError(err)
	_, err = st.LinkParticipants(a, c)
	requirements.NoError(err)

	allowedSources := make(map[int64]bool)
	for i, participant := range []int64{a, b, c} {
		source, err := st.GetOrCreateSource("gmail", "receipt-shrink-"+strconv.Itoa(i)+"@example.test")
		requirements.NoError(err)
		allowedSources[source.ID] = true
		conversation, err := st.EnsureConversation(source.ID, "synthetic-receipt-shrink-"+strconv.Itoa(i), "Synthetic Receipt Shrink")
		requirements.NoError(err)
		_, err = st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conversation,
			SourceMessageID: "synthetic-receipt-shrink-" + strconv.Itoa(i), MessageType: "email",
			SenderID: sql.NullInt64{Int64: participant, Valid: true}})
		requirements.NoError(err)
	}
	denied := errors.New("synthetic source is not granted or lacks current evidence")
	authorize := func(_ context.Context, snapshot *IdentitySnapshot) error {
		currentSources := make(map[int64]bool, len(snapshot.Sources))
		for _, source := range snapshot.Sources {
			if !allowedSources[source.ID] {
				return denied
			}
			currentSources[source.ID] = true
		}
		covered := make(map[int64]bool, len(snapshot.Members))
		for _, contribution := range snapshot.SourceContributions {
			if !currentSources[contribution.SourceID] {
				return denied
			}
			covered[contribution.ParticipantID] = true
		}
		for _, member := range snapshot.Members {
			if !covered[member] {
				return denied
			}
		}
		return nil
	}

	request := receiptRequest(t, st, identitycontrol.OperationGraphLink,
		identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-component-shrink-receipt")
	request.AuthorityFingerprint = strings.Repeat("e", 64)
	committed, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, authorize, authorize)
	requirements.NoError(err)

	unlink := receiptRequest(t, st, identitycontrol.OperationGraphUnlink,
		identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: c}, "synthetic-component-shrink-unlink")
	_, err = st.ApplyIdentityOperationWithPreviewContext(t.Context(), unlink, authorize, authorize)
	requirements.NoError(err)

	recovered, err := st.IdentityOperationReceiptAuthorizedContext(t.Context(), request.Principal,
		request.IdempotencyKey, request.AuthorityFingerprint, authorize)
	requirements.NoError(err, "receipt lookup must retain authorized source evidence after component shrinkage")
	assertions.Equal(committed, recovered)

	replayed, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, authorize, authorize)
	requirements.NoError(err, "an exact retry must recover its committed receipt after component shrinkage")
	assertions.Equal(committed, replayed)
}
