package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identitycontrol"
)

func TestIdentityOperationPreviewIsReadOnlyWithoutCandidate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	_, err := st.db.Exec(`DELETE FROM archive_metadata WHERE key = ?`, identityRevisionKey)
	requirements.NoError(err)
	var metadataBefore, metadataAfter, bindingsBefore, bindingsAfter int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM archive_metadata`).Scan(&metadataBefore))
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM person_participants`).Scan(&bindingsBefore))
	target := identitycontrol.IdentityTarget{ParticipantID: b, OtherParticipantID: a}
	before := target
	snapshot, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.NoError(err)
	requirements.NotNil(snapshot)
	assertions.Equal(identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}, snapshot.Target)
	assertions.Equal([]int64{a, b}, snapshot.Members)
	assertions.Equal(int64(0), snapshot.IdentityRevision)
	assertions.NotEmpty(snapshot.Fingerprint)
	assertions.Equal(before, target)
	repeated, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, before)
	requirements.NoError(err)
	assertions.Equal(snapshot.Fingerprint, repeated.Fingerprint)
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM archive_metadata`).Scan(&metadataAfter))
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM person_participants`).Scan(&bindingsAfter))
	assertions.Equal(metadataBefore, metadataAfter, "readonly preview must not seed identity metadata")
	assertions.Equal(bindingsBefore, bindingsAfter)
	var people, candidates int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM persons`).Scan(&people))
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM identity_match_candidates`).Scan(&candidates))
	assertions.Zero(people, "preview must not promote an identity to a person")
	assertions.Zero(candidates, "an explicit pair does not require a candidate")
}

func TestIdentityOperationPreviewRejectsIncompleteLargeComponent(t *testing.T) {
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	last := a
	for i := 1; i < 99; i++ {
		member, err := st.EnsureParticipant(fmt.Sprintf("bounded-%03d@example.test", i), "Synthetic Member", "example.test")
		requirements.NoError(err)
		_, err = st.LinkParticipants(last, member)
		requirements.NoError(err)
		last = member
	}
	target := identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}
	bounded, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.NoError(err)
	requirements.Len(bounded.Members, 100)
	extra, err := st.EnsureParticipant("bounded-extra@example.test", "Synthetic Extra", "example.test")
	requirements.NoError(err)
	_, err = st.LinkParticipants(last, extra)
	requirements.NoError(err)
	oversized, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.ErrorIs(err, ErrIdentityOperationTooLarge)
	assert.Nil(t, oversized, "scope over 100 members must reject rather than truncate")
}

func TestIdentityOperationPreviewGraphUnlinkPreservesDurableBinding(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	_, err := st.LinkParticipants(a, b)
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	before, err := st.GetPersonContext(t.Context(), person.ID)
	requirements.NoError(err)
	graph, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphUnlink,
		identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b})
	requirements.NoError(err)
	requirements.NotNil(graph)
	binding, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationPersonUnlink,
		identitycontrol.IdentityTarget{ParticipantID: a, PersonID: person.ID})
	requirements.NoError(err)
	requirements.NotNil(binding)
	assertions.NotEqual(graph.Fingerprint, binding.Fingerprint, "graph and binding operations have separate effects")
	after, err := st.GetPersonContext(t.Context(), person.ID)
	requirements.NoError(err)
	assertions.Equal(before, after)
	edges, err := st.ClusterEdges(a)
	requirements.NoError(err)
	assertions.Len(edges, 1, "preview must preserve the native graph")
}

func TestIdentityOperationPreviewRetainsHiddenMessageSourceAuthority(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	source, err := st.GetOrCreateSource("gmail", "hidden-support@example.test")
	requirements.NoError(err)
	_, err = st.GetOrCreateSource("gmail", "unrelated-source@example.test")
	requirements.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "hidden-support-thread", "Synthetic Thread")
	requirements.NoError(err)
	for _, key := range []string{"survivor", "duplicate"} {
		_, err = st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conversation,
			SourceMessageID: key, MessageType: "email", SenderID: sql.NullInt64{Int64: a, Valid: true}})
		requirements.NoError(err)
	}
	requirements.NoError(st.MarkMessageDeleted(source.ID, "survivor"))
	requirements.NoError(st.MarkMessageDeleted(source.ID, "duplicate"))
	target := identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}
	before, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.NoError(err)
	requirements.Len(before.Sources, 1)
	assertions.Equal(source.ID, before.Sources[0].ID)
	assertions.Equal(source.SourceType, before.Sources[0].Type)
	assertions.Equal(source.Identifier, before.Sources[0].Identifier)
	requirements.NoError(st.UpdateSourceIdentifier(source.ID, "changed-support@example.test"))
	after, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.NoError(err)
	assertions.Equal(before.IdentityRevision, after.IdentityRevision)
	assertions.NotEqual(before.Fingerprint, after.Fingerprint, "source scope changes invalidate the exact preview even without a graph revision change")
}

func TestIdentityOperationPreviewUsesContactObservationSourceAuthority(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	source, err := st.GetOrCreateSource("beeper", "synthetic-contact-owner")
	requirements.NoError(err)
	_, err = st.RecordContactObservationContext(t.Context(), a, ParticipantContactObservationInput{
		SourceID: &source.ID, AddressKind: ContactAddressEmail, OriginalValue: "contact-only@example.test",
		Envelope: ValueEnvelopeInput{Source: ProvenanceArchiveObservation},
	})
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(b)
	requirements.NoError(err)
	snapshot, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationPersonLink,
		identitycontrol.IdentityTarget{ParticipantID: a, PersonID: person.ID})
	requirements.NoError(err)
	requirements.Len(snapshot.Sources, 1)
	assertions.Equal(source.ID, snapshot.Sources[0].ID)
	var messages int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assertions.Zero(messages)
	current, err := st.GetPerson(person.ID)
	requirements.NoError(err)
	assertions.Equal([]int64{b}, current.ParticipantIDs, "contact-only preview must not bind the participant")
}

func TestIdentityOperationPreviewRejectsMissingNativeTargets(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	const missingID int64 = 9_007_199_254_740_990
	snapshot, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink,
		identitycontrol.IdentityTarget{ParticipantID: missingID, OtherParticipantID: b})
	requirements.ErrorIs(err, ErrParticipantNotFound)
	assertions.Nil(snapshot)
	snapshot, err = st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationPersonLink,
		identitycontrol.IdentityTarget{ParticipantID: a, PersonID: missingID})
	requirements.ErrorIs(err, ErrPersonNotFound)
	assertions.Nil(snapshot)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	snapshot, err = st.IdentityOperationPreviewContext(ctx, identitycontrol.OperationGraphLink,
		identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b})
	requirements.ErrorIs(err, context.Canceled)
	assertions.Nil(snapshot)
}

func TestIdentityOperationPreviewDisclosesBothNativeMergeOrigins(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	survivor, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	absorbed, _, err := st.CreatePersonFromParticipant(b)
	requirements.NoError(err)
	merged, err := st.MergePersonsContext(t.Context(), PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey: "synthetic-preview-merge", Actor: "synthetic-test",
	})
	requirements.NoError(err)
	for _, member := range []int64{a, b} {
		snapshot, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationPersonUnlink,
			identitycontrol.IdentityTarget{ParticipantID: member, PersonID: merged.Person.ID})
		requirements.NoError(err)
		assertions.Contains(snapshot.Blockers, "active-merge-lineage", "both survivor and absorbed origins require native recovery")
	}
	current, err := st.GetPerson(merged.Person.ID)
	requirements.NoError(err)
	assertions.ElementsMatch([]int64{a, b}, current.ParticipantIDs)
}

func TestIdentityOperationPreviewUsesNativeCardDAVAccountAndBindingRestriction(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	person, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	allowed := true
	bookURL := "https://contacts.example.test/books/synthetic/personal/"
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example.test/dav", Username: "synthetic",
		PrincipalURL: "https://contacts.example.test/principal/synthetic/",
		HomeURL:      "https://contacts.example.test/books/synthetic/",
		Books: []CardDAVDiscoveredBook{{CanonicalURL: bookURL, DisplayName: "Synthetic Contacts",
			CanCreate: &allowed, CanUpdate: &allowed, CanDelete: &allowed}},
	})
	requirements.NoError(err)
	requirements.Len(books, 1)
	requirements.NoError(st.SetCardDAVBookRolesContext(t.Context(), books[0].ID,
		CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true, IsLookupSource: true}))
	local, err := st.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	requirements.NoError(err)
	_, err = st.PrepareCardDAVPublicationContext(t.Context(), CardDAVPublicationPlan{
		PersonID: person.ID, Desired: true, AddressBookID: books[0].ID,
		Href:                 bookURL + person.VCardUID + ".vcf",
		OutgoingBody:         []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + person.VCardUID + "\r\nFN:Synthetic Person\r\nEND:VCARD\r\n"),
		OutgoingSemanticHash: "synthetic-semantic", LocalHash: local.Fingerprint,
	})
	requirements.NoError(err)
	binding, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationPersonUnlink,
		identitycontrol.IdentityTarget{ParticipantID: a, PersonID: person.ID})
	requirements.NoError(err)
	requirements.Len(binding.Accounts, 1)
	assertions.Equal(account.ID, binding.Accounts[0].ID)
	assertions.Contains(binding.Blockers, "carddav-publication")
	graph, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphUnlink,
		identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b})
	requirements.NoError(err)
	assertions.NotContains(graph.Blockers, "carddav-publication", "binding limitation must not become a universal native graph ban")
	var messages int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assertions.Zero(messages)
}

func TestIdentityOperationPreviewDisclosesManualCandidateConfirmation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), IdentityMatchCandidateInput{
		LeftKind: IdentityMatchParticipant, LeftID: a, RightKind: IdentityMatchParticipant, RightID: b,
		Basis: IdentityMatchStableProviderID, NormalizedValue: new("synthetic-stable-provider"),
		State: IdentityMatchStateCandidate, Source: ProvenanceArchiveObservation,
	})
	requirements.NoError(err)
	_, _, err = st.AcceptIdentityMatchCandidateContext(t.Context(), candidate.ID, "system", nil)
	requirements.NoError(err)
	target := identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}
	snapshot, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.NoError(err)
	assertions.Equal(candidate.ID, snapshot.ManualConfirmCandidateID)
	assertions.False(snapshot.Noop, "confirming a candidate-owned edge changes durable provenance")
	var owner sql.NullInt64
	requirements.NoError(st.db.QueryRow(`SELECT identity_match_candidate_id FROM participant_links
  WHERE participant_a=? AND participant_b=?`, a, b).Scan(&owner))
	assertions.Equal(sql.NullInt64{Int64: candidate.ID, Valid: true}, owner, "preview must retain automated ownership")
	_, err = st.LinkParticipants(a, b)
	requirements.NoError(err)
	manual, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.NoError(err)
	assertions.Zero(manual.ManualConfirmCandidateID)
	assertions.True(manual.Noop)
}

func TestIdentityOperationPreviewRequiresNativeMergeForDifferentPeople(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	left, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	right, _, err := st.CreatePersonFromParticipant(b)
	requirements.NoError(err)
	graph, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink,
		identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b})
	requirements.NoError(err)
	assertions.Contains(graph.Blockers, "person-merge-required")
	binding, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationPersonLink,
		identitycontrol.IdentityTarget{ParticipantID: a, PersonID: right.ID})
	requirements.NoError(err)
	assertions.Contains(binding.Blockers, "person-merge-required")
	afterLeft, err := st.GetPerson(left.ID)
	requirements.NoError(err)
	afterRight, err := st.GetPerson(right.ID)
	requirements.NoError(err)
	assertions.Equal(left, afterLeft)
	assertions.Equal(right, afterRight)
}

func TestIdentityOperationPreviewSeparatesParticipantSourceContributions(t *testing.T) {
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	want := map[int64][]int64{}
	for i, member := range []int64{a, b} {
		source, err := st.GetOrCreateSource("gmail", fmt.Sprintf("scope-%d@example.test", i))
		requirements.NoError(err)
		conversation, err := st.EnsureConversation(source.ID, "synthetic-scope-thread", "Synthetic Thread")
		requirements.NoError(err)
		_, err = st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conversation,
			SourceMessageID: "synthetic-scope-message", MessageType: "email", SenderID: sql.NullInt64{Int64: member, Valid: true}})
		requirements.NoError(err)
		want[member] = []int64{source.ID}
	}
	snapshot, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink,
		identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b})
	requirements.NoError(err)
	got := map[int64][]int64{}
	for _, contribution := range snapshot.SourceContributions {
		got[contribution.ParticipantID] = append(got[contribution.ParticipantID], contribution.SourceID)
	}
	assert.Equal(t, want, got, "a participant must not inherit another endpoint's source authority")
}

func TestIdentityOperationPreviewBindsParticipantMetadataWithoutGraphRevision(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, _, b := newUnlinkGuardStore(t)
	a, err := st.EnsureParticipant("preview-display@example.test", "", "example.test")
	requirements.NoError(err)
	target := identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}
	before, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.NoError(err)
	updated, err := st.UpdateParticipantDisplayNameByEmail("preview-display@example.test", "Synthetic Preview Name")
	requirements.NoError(err)
	requirements.True(updated)
	after, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.NoError(err)
	assertions.Equal(before.IdentityRevision, after.IdentityRevision)
	assertions.NotEqual(before.Fingerprint, after.Fingerprint, "preview must retain the exact participant metadata the caller reviewed")
	requirements.NoError(st.SetParticipantIdentifier(a, "email", "preview-additional@example.test"))
	identifiers, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.NoError(err)
	assertions.Equal(after.IdentityRevision, identifiers.IdentityRevision)
	assertions.NotEqual(after.Fingerprint, identifiers.Fingerprint, "new non-owner identifier evidence must invalidate the reviewed identity snapshot")
}

func TestIdentityOperationPreviewRetainsOneNativeReadSnapshot(t *testing.T) {
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	source, err := st.GetOrCreateSource("gmail", "snapshot-before@example.test")
	requirements.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "snapshot-thread", "Synthetic Thread")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "snapshot-message", MessageType: "email", SenderID: sql.NullInt64{Int64: a, Valid: true}})
	requirements.NoError(err)
	target := identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}.Canonical(identitycontrol.OperationGraphLink)
	requirements.NoError(st.withReadSnapshotContext(t.Context(), func(tx *loggedTx) error {
		before, err := st.identityOperationSnapshotTx(t.Context(), tx, identitycontrol.OperationGraphLink, target)
		require.NoError(t, err)
		require.NoError(t, st.UpdateSourceIdentifier(source.ID, "snapshot-after@example.test"))
		inside, err := st.identityOperationSnapshotTx(t.Context(), tx, identitycontrol.OperationGraphLink, target)
		require.NoError(t, err)
		assert.Equal(t, before, inside)
		return nil
	}))
	after, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.NoError(err)
	requirements.Len(after.Sources, 1)
	assert.Equal(t, "snapshot-after@example.test", after.Sources[0].Identifier)
}

func TestIdentityOperationPreviewIncludesSplitDurablePersonMembership(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	_, err := st.LinkParticipants(a, b)
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	_, err = st.UnlinkParticipants(a, b)
	requirements.NoError(err)
	snapshot, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationPersonUnlink,
		identitycontrol.IdentityTarget{ParticipantID: a, PersonID: person.ID})
	requirements.NoError(err)
	assertions.Equal([]int64{a}, snapshot.ComponentMembers, "binding effect is one selected component")
	assertions.Equal([]int64{a, b}, snapshot.Members, "authorization must also cover the complete affected durable person")
	assertions.Len(snapshot.Bindings, 2)
}

func TestIdentityOperationPreviewRejectsConflictingBindingEvidence(t *testing.T) {
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	person, _, err := st.CreatePersonFromParticipant(b)
	requirements.NoError(err)
	_, _, err = st.UpsertIdentityMatchCandidateContext(t.Context(), IdentityMatchCandidateInput{
		LeftKind: IdentityMatchParticipant, LeftID: a, RightKind: IdentityMatchParticipant, RightID: b,
		Basis: IdentityMatchEmail, NormalizedValue: new("conflicting@example.test"),
		State: IdentityMatchStateConflict, Source: ProvenanceArchiveObservation,
	})
	requirements.NoError(err)
	snapshot, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationPersonLink,
		identitycontrol.IdentityTarget{ParticipantID: a, PersonID: person.ID})
	requirements.NoError(err)
	assert.Contains(t, snapshot.Blockers, "conflicting-identity-evidence")
}

func TestIdentityOperationPreviewBindsContactObservationEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, b := newUnlinkGuardStore(t)
	source, err := st.GetOrCreateSource("beeper", "synthetic-observation-source")
	requirements.NoError(err)
	input := ParticipantContactObservationInput{SourceID: &source.ID, AddressKind: ContactAddressEmail,
		OriginalValue: "observation-one@example.test", Envelope: ValueEnvelopeInput{Source: ProvenanceArchiveObservation}}
	_, err = st.RecordContactObservationContext(t.Context(), a, input)
	requirements.NoError(err)
	target := identitycontrol.IdentityTarget{ParticipantID: a, OtherParticipantID: b}
	before, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.NoError(err)
	input.OriginalValue = "observation-two@example.test"
	_, err = st.RecordContactObservationContext(t.Context(), a, input)
	requirements.NoError(err)
	after, err := st.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, target)
	requirements.NoError(err)
	assertions.Equal(before.IdentityRevision, after.IdentityRevision)
	assertions.Equal(before.SourceContributions, after.SourceContributions)
	assertions.NotEqual(before.Fingerprint, after.Fingerprint, "contact observations remain part of the verified identity evidence even when source authority stays the same")
}
