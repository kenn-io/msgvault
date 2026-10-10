package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identitycontrol"
)

func TestIdentityOperationReceiptDisclosesCandidateEdgeConfirmation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), IdentityMatchCandidateInput{
		LeftKind: IdentityMatchParticipant, LeftID: a, RightKind: IdentityMatchParticipant, RightID: b,
		Basis: IdentityMatchStableProviderID, NormalizedValue: new("synthetic-receipt-provider-id"), State: IdentityMatchStateCandidate, Source: ProvenanceArchiveObservation,
	})
	requirements.NoError(err)
	_, _, err = st.AcceptIdentityMatchCandidateContext(t.Context(), candidate.ID, "system", nil)
	requirements.NoError(err)
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-confirm-candidate-key")
	receipt, err := st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.NoError(err)
	requirements.NotNil(receipt.Edge)
	assertions.True(receipt.Changed)
	assertions.True(receipt.Edge.BeforePresent)
	assertions.True(receipt.Edge.AfterPresent)
	assertions.Equal(candidate.ID, receipt.Edge.BeforeCandidateID)
	assertions.Zero(receipt.Edge.AfterCandidateID)
	var owner sql.NullInt64
	requirements.NoError(st.db.QueryRow(`SELECT identity_match_candidate_id FROM participant_links WHERE participant_a=? AND participant_b=?`, a, b).Scan(&owner))
	assertions.False(owner.Valid)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Len(edges, 1, "confirmation changes ownership without creating another edge")
}

func TestIdentityOperationReceiptRejectsChangedComponent(t *testing.T) {
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	request := receiptRequest(t, st, identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, "synthetic-component-stale-key")
	other, err := st.EnsureParticipant("changed-component@example.test", "Other Synthetic Participant", "example.test")
	requirements.NoError(err)
	_, err = st.LinkParticipants(a, other)
	requirements.NoError(err)
	_, err = st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.ErrorIs(err, ErrIdentityOperationStale)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assert.Len(t, edges, 1)
}

func TestIdentityOperationReceiptRejectsBothMergeOriginDetaches(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	survivor, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(b)
	requirements.NoError(err)
	merged, err := st.MergePersonsContext(t.Context(), PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "synthetic-receipt-merge", Actor: "synthetic-test"})
	requirements.NoError(err)
	for _, member := range []int64{a, b} {
		request := receiptRequest(t, st, identitycontrol.OperationPersonUnlink, identitycontrol.IdentityTarget{ParticipantID: member, PersonID: merged.Person.ID}, "synthetic-merge-detach-key")
		_, err = st.ApplyIdentityOperationContext(t.Context(), request, nil)
		requirements.ErrorIs(err, ErrIdentityOperationBlocked)
	}
	current, err := st.GetPerson(merged.Person.ID)
	requirements.NoError(err)
	assertions.ElementsMatch([]int64{a, b}, current.ParticipantIDs)
	var lineages int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM person_merge_participants WHERE merge_id=? AND split_id IS NULL`, merged.Merge.ID).Scan(&lineages))
	assertions.Equal(2, lineages)
}

func TestIdentityOperationReceiptRefusesDifferentCuratedPerson(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	left, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	right, _, err := st.CreatePersonFromParticipant(b)
	requirements.NoError(err)
	request := receiptRequest(t, st, identitycontrol.OperationPersonLink, identitycontrol.IdentityTarget{ParticipantID: a, PersonID: right.ID}, "synthetic-other-person-key")
	_, err = st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.ErrorIs(err, ErrIdentityOperationBlocked)
	current, err := st.GetPerson(left.ID)
	requirements.NoError(err)
	assertions.Equal([]int64{a}, current.ParticipantIDs)
	current, err = st.GetPerson(right.ID)
	requirements.NoError(err)
	assertions.Equal([]int64{b}, current.ParticipantIDs)
}

func TestIdentityOperationReceiptKeepsPublicationOnDeniedDetach(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, _ := newUnlinkGuardStore(t)
	person, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	allowed := true
	bookURL := "https://contacts.example.test/books/synthetic/receipt/"
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), CardDAVDiscoveryInput{BaseURL: "https://contacts.example.test/dav", Username: "synthetic", PrincipalURL: "https://contacts.example.test/principal/synthetic/", HomeURL: "https://contacts.example.test/books/synthetic/", Books: []CardDAVDiscoveredBook{{CanonicalURL: bookURL, DisplayName: "Synthetic Receipt Contacts", CanCreate: &allowed, CanUpdate: &allowed, CanDelete: &allowed}}})
	requirements.NoError(err)
	requirements.Len(books, 1)
	requirements.NoError(st.SetCardDAVBookRolesContext(t.Context(), books[0].ID, CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true}))
	local, err := st.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	requirements.NoError(err)
	_, err = st.PrepareCardDAVPublicationContext(t.Context(), CardDAVPublicationPlan{PersonID: person.ID, Desired: true, AddressBookID: books[0].ID, Href: bookURL + person.VCardUID + ".vcf", OutgoingBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + person.VCardUID + "\r\nFN:Synthetic Person\r\nEND:VCARD\r\n"), OutgoingSemanticHash: "synthetic-receipt-semantic", LocalHash: local.Fingerprint})
	requirements.NoError(err)
	request := receiptRequest(t, st, identitycontrol.OperationPersonUnlink, identitycontrol.IdentityTarget{ParticipantID: a, PersonID: person.ID}, "synthetic-publication-detach-key")
	_, err = st.ApplyIdentityOperationContext(t.Context(), request, nil)
	requirements.ErrorIs(err, ErrIdentityOperationBlocked)
	var publications int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM carddav_publications WHERE person_id=? AND desired=TRUE`, person.ID).Scan(&publications))
	assertions.Equal(1, publications)
	current, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Equal([]int64{a}, current.ParticipantIDs)
}
