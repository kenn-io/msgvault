package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportantDateIdentityRequiresExistingPerson(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	id := newTestPerson(t, st)
	stable, err := st.ImportantDateIdentityStableContext(t.Context(), id)
	must.NoError(err)
	checks.True(stable)
	stable, err = st.ImportantDateIdentityStableContext(t.Context(), -1)
	must.NoError(err)
	checks.False(stable)
}

func TestImportantDateIdentityQuarantinesMerge(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	a, err := st.EnsureParticipantByIdentifier("email", "first@example.test", "First Example")
	must.NoError(err)
	b, err := st.EnsureParticipantByIdentifier("email", "second@example.test", "Second Example")
	must.NoError(err)
	first, _, err := st.CreatePersonFromParticipantContext(ctx, a)
	must.NoError(err)
	second, _, err := st.CreatePersonFromParticipantContext(ctx, b)
	must.NoError(err)
	_, err = st.AddPersonDateContext(ctx, second.ID, store.PersonDateInput{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: new(10), Day: new(23)}, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	must.NoError(err)
	second, err = st.GetPersonContext(ctx, second.ID)
	must.NoError(err)
	owned, err := st.CreateImportantDateSubjectContext(ctx, store.ImportantDateSubjectInput{OwnerUID: second.VCardUID, Kind: "pet", DisplayName: "Example pet", Date: store.PartialDate{Month: new(10), Day: new(23)}})
	must.NoError(err)
	changed, err := st.UpdateImportantDateSubjectContext(ctx, owned.UID, owned.Revision, "Example pet", store.PartialDate{Month: new(11), Day: new(5)}, false)
	must.NoError(err)
	must.True(changed)
	key := "person/" + second.VCardUID + ":birthday:2027"
	claimed, err := st.ClaimImportantDateDeliveryContext(ctx, key, "example-fingerprint")
	must.NoError(err)
	must.True(claimed)
	otherKey := "person/" + first.VCardUID + ":birthday:2027"
	claimed, err = st.ClaimImportantDateDeliveryContext(ctx, otherKey, "example-fingerprint")
	must.NoError(err)
	must.True(claimed)
	// A UID can contain delimiters; an unrelated subject whose UID begins
	// with a lineage UID must not appear in the inspection.
	unrelated := "person/" + first.VCardUID + ":extra:birthday:2027"
	claimed, err = st.ClaimImportantDateDeliveryContext(ctx, unrelated, "example-fingerprint")
	must.NoError(err)
	must.True(claimed)
	merged, err := st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: first.ID, AbsorbedID: second.ID, ExpectedSurvivorRevision: first.Revision, ExpectedAbsorbedRevision: second.Revision, IdempotencyKey: "example-merge", Actor: "test"})
	must.NoError(err)
	stable, err := st.ImportantDateIdentityStableContext(ctx, first.ID)
	must.NoError(err)
	checks.False(stable)
	split, err := st.SplitPersonMergeContext(ctx, store.PersonSplitRequest{SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID, ParticipantIDs: []int64{b}, ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "example-split", Actor: "test"})
	must.NoError(err)
	stable, err = st.ImportantDateIdentityStableContext(ctx, split.SourcePerson.ID)
	must.NoError(err)
	checks.False(stable)
	stable, err = st.ImportantDateIdentityStableContext(ctx, split.NewPerson.ID)
	must.NoError(err)
	checks.False(stable)
	state, _, err := st.ImportantDateDeliveryContext(ctx, key)
	must.NoError(err)
	checks.Equal(store.ImportantDateClaimed, state)
	lineage, err := st.ImportantDateLineageContext(ctx, split.NewPerson.ID)
	must.NoError(err)
	checks.NotEmpty(lineage.Digest)
	checks.Contains(lineage.MissingPersonIDs, second.ID)
	checks.Contains(lineage.PersonUIDs, first.VCardUID)
	checks.Contains(lineage.PersonUIDs, second.VCardUID)
	checks.Contains(lineage.PersonUIDs, split.NewPerson.VCardUID)
	checks.Contains(lineage.MergeIDs, merged.Merge.ID)
	must.Len(lineage.MergeSnapshots, 1)
	checks.Equal(merged.Merge.SnapshotSHA256, lineage.MergeSnapshots[0].SHA256)
	checks.Contains(lineage.SplitIDs, split.Split.ID)
	var retainedDates int
	for _, person := range lineage.People {
		retainedDates += len(person.Dates)
	}
	checks.Positive(retainedDates)
	must.Len(lineage.OwnedSubjects, 1)
	checks.Equal(owned.UID, lineage.OwnedSubjects[0].UID)
	checks.Equal(second.VCardUID, lineage.OwnedSubjects[0].OwnerUID)
	checks.Equal(owned.Revision+1, lineage.OwnedSubjects[0].Revision)
	must.Len(lineage.OwnedSubjects[0].History, 1)
	checks.Equal(owned.Date, lineage.OwnedSubjects[0].History[0].Date)
	must.Len(lineage.Deliveries, 2)
	var keys []string
	for _, delivery := range lineage.Deliveries {
		keys = append(keys, delivery.OccurrenceKey)
	}
	checks.ElementsMatch([]string{key, otherKey}, keys)
	again, err := st.ImportantDateLineageContext(ctx, split.NewPerson.ID)
	must.NoError(err)
	checks.Equal(lineage.Digest, again.Digest)
	changed, err = st.ResolveImportantDateDeliveryContext(ctx, key, store.ImportantDateCancelled, "")
	must.NoError(err)
	must.True(changed)
	again, err = st.ImportantDateLineageContext(ctx, split.NewPerson.ID)
	must.NoError(err)
	checks.NotEqual(lineage.Digest, again.Digest)
	previousDigest := again.Digest
	count, err := st.AcceptImportantDateCorrectionContext(ctx, "person/"+second.VCardUID+":birthday:", "corrected-fingerprint")
	must.NoError(err)
	checks.Equal(int64(1), count)
	again, err = st.ImportantDateLineageContext(ctx, split.NewPerson.ID)
	must.NoError(err)
	checks.NotEqual(previousDigest, again.Digest)
}

func TestRetiringAliasChangesIdentityRevision(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	personID := newTestPerson(t, st)
	before, err := st.IdentityRevisionContext(ctx)
	must.NoError(err)
	_, err = st.RetirePersonUIDAliasContext(ctx, "retired-example-uid", &personID, "test")
	must.NoError(err)
	after, err := st.IdentityRevisionContext(ctx)
	must.NoError(err)
	checks.Equal(before+1, after)
}
