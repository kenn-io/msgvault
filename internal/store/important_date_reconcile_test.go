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
	var guards int
	err = st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*) FROM important_date_occurrence_guards WHERE reconciliation_uid = ? AND resolved_subject = ? AND meaning = 'birthday' AND occurrence_year = 2027`), id, target).Scan(&guards)
	must.NoError(err)
	checks.Equal(2, guards)
	active, err := st.HasImportantDateConsentContext(ctx, target, "private-example", "owner/agent")
	must.NoError(err)
	checks.False(active)
	stable, err := st.ImportantDateIdentityStableContext(ctx, merged.Person.ID)
	must.NoError(err)
	checks.False(stable, "recording decisions alone must not release quarantine")
}
