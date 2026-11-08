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
	key := "person/" + second.VCardUID + ":birthday:2027"
	claimed, err := st.ClaimImportantDateDeliveryContext(ctx, key, "example-fingerprint")
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
}
