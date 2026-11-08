package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportantDateReconciliationRetainsMergeCollisions(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	makePerson := func(address string) *store.Person {
		participant, err := st.EnsureParticipantByIdentifier("email", address, "Example Person")
		must.NoError(err)
		person, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
		must.NoError(err)
		return person
	}
	first := makePerson("first-reconcile@example.test")
	second := makePerson("second-reconcile@example.test")
	owned, err := st.CreateImportantDateSubjectContext(ctx, store.ImportantDateSubjectInput{OwnerUID: second.VCardUID, Kind: "pet", DisplayName: "Example pet", Date: store.PartialDate{Month: new(10), Day: new(23)}})
	must.NoError(err)
	must.NoError(st.SetImportantDateConsentContext(ctx, "owned/"+owned.UID, "private-example", "owner/agent", true))
	for _, person := range []*store.Person{first, second} {
		subject := "person/" + person.VCardUID
		must.NoError(st.SetImportantDateConsentContext(ctx, subject, "private-example", "owner/agent", true))
		key := subject + ":birthday:2027"
		claimed, err := st.ClaimImportantDateDeliveryContext(ctx, key, "fingerprint-example")
		must.NoError(err)
		must.True(claimed)
		changed, err := st.ResolveImportantDateDeliveryContext(ctx, key, store.ImportantDateCancelled, "")
		must.NoError(err)
		must.True(changed)
	}
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: first.ID, AbsorbedID: second.ID, ExpectedSurvivorRevision: first.Revision, ExpectedAbsorbedRevision: second.Revision, IdempotencyKey: "example-reconcile-merge", Actor: "test"})
	must.NoError(err)
	lineage, err := st.ImportantDateLineageContext(ctx, merged.Person.ID)
	must.NoError(err)
	target := "person/" + merged.Person.VCardUID
	request := store.ImportantDateReconcileRequest{PersonID: merged.Person.ID, IdentityRevision: lineage.IdentityRevision, LineageDigest: lineage.Digest, IdempotencyKey: "example-reconcile", Actor: "test", InspectedSnapshots: true}
	for _, uid := range lineage.PersonUIDs {
		for _, meaning := range []string{"birthday", "anniversary"} {
			request.Decisions = append(request.Decisions, store.ImportantDateIdentityDecision{OriginalSubject: "person/" + uid, Meaning: meaning, ResolvedSubjects: []string{target}})
		}
	}
	request.Decisions = append(request.Decisions, store.ImportantDateIdentityDecision{OriginalSubject: "owned/" + owned.UID, Meaning: "birthday", ResolvedSubjects: []string{"owned/" + owned.UID}, EffectiveOwner: target})
	incomplete := request
	incomplete.IdempotencyKey = "incomplete-reconcile"
	incomplete.Decisions = request.Decisions[:len(request.Decisions)-1]
	_, err = st.ReconcileImportantDateIdentityContext(ctx, incomplete)
	must.Error(err)
	stale := request
	stale.IdempotencyKey = "stale-reconcile"
	stale.LineageDigest = "stale-digest"
	_, err = st.ReconcileImportantDateIdentityContext(ctx, stale)
	must.Error(err)
	stillGranted, err := st.HasImportantDateConsentContext(ctx, target, "private-example", "owner/agent")
	must.NoError(err)
	checks.True(stillGranted, "rejected decisions must not revoke consent")
	id, err := st.ReconcileImportantDateIdentityContext(ctx, request)
	must.NoError(err)
	checks.NotEmpty(id)
	replayed, err := st.ReconcileImportantDateIdentityContext(ctx, request)
	must.NoError(err)
	checks.Equal(id, replayed)
	outdated := request
	outdated.IdempotencyKey = "outdated-reconcile"
	_, err = st.ReconcileImportantDateIdentityContext(ctx, outdated)
	must.Error(err, "a prior inspection cannot silently supersede a newer decision")
	var guards int
	err = st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*) FROM important_date_occurrence_guards WHERE reconciliation_uid = ? AND resolved_subject = ? AND meaning = 'birthday' AND occurrence_year = 2027`), id, target).Scan(&guards)
	must.NoError(err)
	checks.Equal(2, guards)
	active, err := st.HasImportantDateConsentContext(ctx, target, "private-example", "owner/agent")
	must.NoError(err)
	checks.False(active)
	active, err = st.HasImportantDateConsentContext(ctx, "owned/"+owned.UID, "private-example", "owner/agent")
	must.NoError(err)
	checks.False(active)
	subjects, err := st.ListImportantDateSubjectsContext(ctx, merged.Person.VCardUID)
	must.NoError(err)
	must.Len(subjects, 1)
	checks.Equal(owned.UID, subjects[0].UID)
	stable, err := st.ImportantDateIdentityStableContext(ctx, merged.Person.ID)
	must.NoError(err)
	checks.True(stable, "a reviewed unchanged lineage may be eligible after fresh consent")
	must.NoError(st.SetImportantDateConsentContext(ctx, target, "private-example", "owner/agent", true))
	must.NoError(st.SetImportantDateConsentContext(ctx, "owned/"+owned.UID, "private-example", "owner/agent", true))
	active, err = st.HasImportantDateConsentContext(ctx, target, "private-example", "owner/agent")
	must.NoError(err)
	checks.True(active)
	_, err = st.RetirePersonUIDAliasContext(ctx, "new-retired-example", &merged.Person.ID, "test")
	must.NoError(err)
	stable, err = st.ImportantDateIdentityStableContext(ctx, merged.Person.ID)
	must.NoError(err)
	checks.False(stable, "an identity mutation invalidates the reviewed topology")
}

func TestImportantDateSplitGuardSuppressesHistoricOccurrence(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	firstParticipant, err := st.EnsureParticipantByIdentifier("email", "split-first@example.test", "Example Person")
	must.NoError(err)
	secondParticipant, err := st.EnsureParticipantByIdentifier("email", "split-second@example.test", "Example Person")
	must.NoError(err)
	first, _, err := st.CreatePersonFromParticipantContext(ctx, firstParticipant)
	must.NoError(err)
	second, _, err := st.CreatePersonFromParticipantContext(ctx, secondParticipant)
	must.NoError(err)
	oldKey := "person/" + second.VCardUID + ":birthday:2027"
	claimed, err := st.ClaimImportantDateDeliveryContext(ctx, oldKey, "old-fingerprint")
	must.NoError(err)
	must.True(claimed)
	changed, err := st.ResolveImportantDateDeliveryContext(ctx, oldKey, store.ImportantDateCancelled, "")
	must.NoError(err)
	must.True(changed)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: first.ID, AbsorbedID: second.ID, ExpectedSurvivorRevision: first.Revision, ExpectedAbsorbedRevision: second.Revision, IdempotencyKey: "example-split-guard-merge", Actor: "test"})
	must.NoError(err)
	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID, ParticipantIDs: []int64{secondParticipant}, ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "example-split-guard-split", Actor: "test"})
	must.NoError(err)
	lineage, err := st.ImportantDateLineageContext(ctx, split.NewPerson.ID)
	must.NoError(err)
	newSubject := "person/" + split.NewPerson.VCardUID
	firstSubject := "person/" + split.SourcePerson.VCardUID
	request := store.ImportantDateReconcileRequest{PersonID: split.NewPerson.ID, IdentityRevision: lineage.IdentityRevision, LineageDigest: lineage.Digest, IdempotencyKey: "example-split-guard", Actor: "test", InspectedSnapshots: true}
	for _, uid := range lineage.PersonUIDs {
		target := newSubject
		if uid == split.SourcePerson.VCardUID {
			target = firstSubject
		}
		for _, meaning := range []string{"birthday", "anniversary"} {
			request.Decisions = append(request.Decisions, store.ImportantDateIdentityDecision{OriginalSubject: "person/" + uid, Meaning: meaning, ResolvedSubjects: []string{target}})
		}
	}
	for i := range request.Decisions {
		if request.Decisions[i].OriginalSubject == "person/"+second.VCardUID && request.Decisions[i].Meaning == "birthday" {
			request.Decisions[i].ResolvedSubjects = []string{firstSubject, newSubject}
		}
	}
	_, err = st.ReconcileImportantDateIdentityContext(ctx, request)
	must.NoError(err)
	stable, err := st.ImportantDateIdentityStableContext(ctx, split.NewPerson.ID)
	must.NoError(err)
	checks.False(stable, "ambiguous mappings must remain quarantined")
	stable, err = st.ImportantDateIdentityStableContext(ctx, split.SourcePerson.ID)
	must.NoError(err)
	checks.False(stable)
	claimed, err = st.ClaimImportantDateDeliveryContext(ctx, firstSubject+":birthday:2027", "old-fingerprint")
	must.NoError(err)
	checks.False(claimed, "an ambiguous historical occurrence guards both possible people")
	claimed, err = st.ClaimImportantDateDeliveryContext(ctx, firstSubject+":birthday:2028", "old-fingerprint")
	must.NoError(err)
	must.True(claimed)
	claimed, err = st.ClaimImportantDateDeliveryContext(ctx, newSubject+":birthday:2028", "old-fingerprint")
	must.NoError(err)
	checks.False(claimed, "new occurrences also deduplicate across ambiguous mappings")
	changed, err = st.ResolveImportantDateDeliveryContext(ctx, firstSubject+":birthday:2028", store.ImportantDateCancelled, "")
	must.NoError(err)
	must.True(changed)
	_, err = st.AcceptImportantDateCorrectionContext(ctx, newSubject+":birthday:", "changed-fingerprint")
	must.Error(err)
	// An explicit later decision narrows attribution without deleting history.
	lineage, err = st.ImportantDateLineageContext(ctx, split.NewPerson.ID)
	must.NoError(err)
	request.LineageDigest = lineage.Digest
	request.IdentityRevision = lineage.IdentityRevision
	request.IdempotencyKey = "example-split-guard-resolved"
	for i := range request.Decisions {
		if request.Decisions[i].OriginalSubject == "person/"+second.VCardUID && request.Decisions[i].Meaning == "birthday" {
			request.Decisions[i].ResolvedSubjects = []string{newSubject}
		}
	}
	_, err = st.ReconcileImportantDateIdentityContext(ctx, request)
	must.NoError(err)
	stable, err = st.ImportantDateIdentityStableContext(ctx, split.NewPerson.ID)
	must.NoError(err)
	checks.True(stable)
	claimed, err = st.ClaimImportantDateDeliveryContext(ctx, newSubject+":birthday:2028", "old-fingerprint")
	must.NoError(err)
	checks.False(claimed, "narrowing a later mapping must retain earlier possible-subject guards")
	claimed, err = st.ClaimImportantDateDeliveryContext(ctx, newSubject+":birthday:2027", "old-fingerprint")
	must.NoError(err)
	checks.False(claimed, "the old occurrence must guard its mapped split identity")
	pending, err := st.ImportantDateHasOtherPendingContext(ctx, newSubject+":birthday:", newSubject+":birthday:2028", "changed-fingerprint")
	must.NoError(err)
	checks.True(pending, "a correction requires review of historical mapped issues")
	count, err := st.AcceptImportantDateCorrectionContext(ctx, newSubject+":birthday:", "changed-fingerprint")
	must.NoError(err)
	checks.Equal(int64(2), count)
	pending, err = st.ImportantDateHasOtherPendingContext(ctx, newSubject+":birthday:", newSubject+":birthday:2028", "changed-fingerprint")
	must.NoError(err)
	checks.False(pending)
	pending, err = st.ImportantDateHasOtherPendingContext(ctx, firstSubject+":birthday:", firstSubject+":birthday:2029", "changed-fingerprint")
	must.NoError(err)
	checks.True(pending, "approving one subject must not approve the other")
	claimed, err = st.ClaimImportantDateDeliveryContext(ctx, newSubject+":birthday:2029", "changed-fingerprint")
	must.NoError(err)
	must.True(claimed)
	changed, err = st.ResolveImportantDateDeliveryContext(ctx, newSubject+":birthday:2029", store.ImportantDateCancelled, "")
	must.NoError(err)
	must.True(changed)
	lineage, err = st.ImportantDateLineageContext(ctx, split.NewPerson.ID)
	must.NoError(err)
	request.LineageDigest = lineage.Digest
	request.IdentityRevision = lineage.IdentityRevision
	request.IdempotencyKey = "example-split-guard-reassigned"
	for i := range request.Decisions {
		if request.Decisions[i].OriginalSubject == "person/"+second.VCardUID && request.Decisions[i].Meaning == "birthday" {
			request.Decisions[i].ResolvedSubjects = []string{firstSubject}
		}
	}
	_, err = st.ReconcileImportantDateIdentityContext(ctx, request)
	must.NoError(err)
	exists, err := st.ImportantDateDeliveryExistsContext(ctx, firstSubject+":birthday:2029")
	must.NoError(err)
	checks.True(exists, "reassignment must retain occurrences created under the old single-target mapping")
}
