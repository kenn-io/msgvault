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

func TestIdentityOperationReceiptAuthorizationRetainsSourceEvidenceAfterRosterRemoval(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, a, b := newUnlinkGuardStore(t)
	source, err := st.GetOrCreateSource("gmail", "synthetic-roster@example.test")
	requirements.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "synthetic-roster-thread", "Synthetic Roster Thread")
	requirements.NoError(err)
	requirements.NoError(st.ReplaceConversationParticipantsContext(t.Context(), conversation, []ConversationParticipantRef{
		{ParticipantID: a, Role: "member"},
		{ParticipantID: b, Role: "member"},
	}))
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink,
		identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-roster-receipt")
	request.AuthorityFingerprint = strings.Repeat("d", 64)
	missingEvidence := errors.New("synthetic source evidence missing")
	authorize := func(_ context.Context, snapshot *IdentitySnapshot) error {
		var sourceEvidence bool
		var participantA, participantB bool
		for _, candidate := range snapshot.Sources {
			if candidate.ID == source.ID {
				sourceEvidence = true
			}
		}
		for _, contribution := range snapshot.SourceContributions {
			if contribution.SourceID != source.ID {
				continue
			}
			participantA = participantA || contribution.ParticipantID == a
			participantB = participantB || contribution.ParticipantID == b
		}
		if !sourceEvidence || !participantA || !participantB {
			return missingEvidence
		}
		return nil
	}
	receipt, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, authorize, authorize)
	requirements.NoError(err)
	requirements.NoError(st.ReplaceConversationParticipantsContext(t.Context(), conversation, nil))

	recovered, err := st.IdentityOperationReceiptAuthorizedContext(t.Context(), request.Principal, request.IdempotencyKey, request.AuthorityFingerprint, authorize)
	requirements.NoError(err)
	assertions.Equal(receipt, recovered)

	retried, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, authorize, authorize)
	requirements.NoError(err)
	assertions.Equal(receipt, retried)
}

func TestIdentityOperationReceiptCardDAVResourcesIgnoreRevisionChanges(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, a, b := newUnlinkGuardStore(t)
	person, _, err := st.CreatePersonFromParticipant(b)
	requirements.NoError(err)
	allowed := true
	bookURL := "https://contacts.example.test/books/synthetic/receipt/"
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example.test/dav", Username: "synthetic",
		PrincipalURL: "https://contacts.example.test/principal/synthetic/",
		HomeURL:      "https://contacts.example.test/books/synthetic/",
		Books:        []CardDAVDiscoveredBook{{CanonicalURL: bookURL, DisplayName: "Synthetic Receipt Contacts", CanCreate: &allowed, CanUpdate: &allowed, CanDelete: &allowed}},
	})
	requirements.NoError(err)
	requirements.Len(books, 1)
	requirements.NoError(st.SetCardDAVBookRolesContext(t.Context(), books[0].ID, CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true}))
	local, err := st.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	requirements.NoError(err)
	_, err = st.PrepareCardDAVPublicationContext(t.Context(), CardDAVPublicationPlan{
		PersonID: person.ID, Desired: true, AddressBookID: books[0].ID,
		Href:                 bookURL + person.VCardUID + ".vcf",
		OutgoingBody:         []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + person.VCardUID + "\r\nFN:Synthetic Person\r\nEND:VCARD\r\n"),
		OutgoingSemanticHash: "synthetic-receipt-semantic", LocalHash: local.Fingerprint,
	})
	requirements.NoError(err)
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink,
		identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-carddav-revision-receipt")
	request.AuthorityFingerprint = strings.Repeat("e", 64)
	authorize := func(context.Context, *IdentitySnapshot) error { return nil }
	receipt, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, authorize, authorize)
	requirements.NoError(err)

	var observed []*IdentitySnapshot
	authorize = func(_ context.Context, snapshot *IdentitySnapshot) error {
		observed = append(observed, snapshot)
		return nil
	}
	readback, err := st.IdentityOperationReceiptAuthorizedContext(t.Context(), request.Principal, request.IdempotencyKey, request.AuthorityFingerprint, authorize)
	requirements.NoError(err)
	assertions.Equal(receipt, readback)
	requirements.Len(observed, 2)
	requirements.Len(observed[0].Accounts, 1)
	requirements.Len(observed[0].Books, 1)
	requirements.Zero(observed[0].Accounts[0].ConnectionGeneration)
	requirements.Zero(observed[0].Books[0].SyncRevision)
	assertions.Positive(observed[1].Accounts[0].ConnectionGeneration)
	assertions.Positive(observed[1].Books[0].SyncRevision)

	retried, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, authorize, authorize)
	requirements.NoError(err)
	assertions.Equal(receipt, retried)
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

func TestIdentityOperationReceiptIgnoresDescriptiveSourceConfigAndRejectsOwnerChange(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, a, b := newUnlinkGuardStore(t)

	calendar, err := st.GetOrCreateSource("gcal", "team@example.test")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(calendar.ID,
		`{"account_email":"owner@example.test","calendar_id":"team@example.test","calendar_summary":"Original Team Calendar","access_role":"owner","time_zone":"UTC"}`))
	beeper, err := st.GetOrCreateSource("beeper", "synthetic-device")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(beeper.ID,
		`{"repair_version":"v7","repair_after_id":10}`))

	for _, source := range []struct {
		id  int64
		key string
	}{
		{calendar.ID, "calendar"},
		{beeper.ID, "beeper"},
	} {
		conversation, err := st.EnsureConversation(source.id, "synthetic-identity-"+source.key+"-thread", "Synthetic Identity Thread")
		requirements.NoError(err)
		for i, participant := range []int64{a, b} {
			_, err = st.UpsertMessage(&Message{
				SourceID: source.id, ConversationID: conversation, SourceMessageID: source.key + "-" + strconv.Itoa(i),
				MessageType: "email", SenderID: sql.NullInt64{Int64: participant, Valid: true},
			})
			requirements.NoError(err)
		}
	}

	request := receiptRequest(t, st, identitycontrol.OperationGraphLink,
		identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-config-recovery")
	request.AuthorityFingerprint = strings.Repeat("f", 64)
	allow := func(context.Context, *IdentitySnapshot) error { return nil }
	receipt, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, allow, allow)
	requirements.NoError(err)

	requirements.NoError(st.UpdateSourceSyncConfig(calendar.ID,
		`{"account_email":"owner@example.test","calendar_id":"team@example.test","calendar_summary":"Renamed Team Calendar","access_role":"owner","time_zone":"America/Vancouver"}`))
	requirements.NoError(st.UpdateSourceSyncConfig(beeper.ID,
		`{"repair_version":"v8","repair_after_id":20}`))

	recovered, err := st.IdentityOperationReceiptAuthorizedContext(
		t.Context(), request.Principal, request.IdempotencyKey, request.AuthorityFingerprint, allow,
	)
	requirements.NoError(err)
	assertions.Equal(receipt, recovered, "descriptive calendar metadata and repair checkpoints must not break recovery")

	retried, err := st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, allow, allow)
	requirements.NoError(err)
	assertions.Equal(receipt, retried, "exact retry must survive descriptive and operational config changes")

	requirements.NoError(st.UpdateSourceSyncConfig(calendar.ID,
		`{"account_email":"different-owner@example.test","calendar_id":"team@example.test","calendar_summary":"Renamed Team Calendar","access_role":"owner","time_zone":"America/Vancouver"}`))
	_, err = st.IdentityOperationReceiptAuthorizedContext(
		t.Context(), request.Principal, request.IdempotencyKey, request.AuthorityFingerprint, allow,
	)
	requirements.ErrorIs(err, ErrIdentityOperationReceiptDenied, "a different token account changes source ownership")
	_, err = st.ApplyIdentityOperationWithPreviewContext(t.Context(), request, allow, allow)
	requirements.ErrorIs(err, ErrIdentityOperationReceiptDenied, "exact retries must recheck source ownership")
}
