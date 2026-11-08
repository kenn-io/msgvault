package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportantDateIdentityRequiresExistingPerson(t *testing.T) {
	st := storetest.New(t).Store
	id := newTestPerson(t, st)
	stable, err := st.ImportantDateIdentityStableContext(t.Context(), id)
	require.NoError(t, err)
	assert.True(t, stable)
	stable, err = st.ImportantDateIdentityStableContext(t.Context(), -1)
	require.NoError(t, err)
	assert.False(t, stable)
}

func TestImportantDateIdentityQuarantinesMerge(t *testing.T) {
	st := storetest.New(t).Store
	ctx := t.Context()
	a, err := st.EnsureParticipantByIdentifier("email", "first@example.test", "First Example")
	require.NoError(t, err)
	b, err := st.EnsureParticipantByIdentifier("email", "second@example.test", "Second Example")
	require.NoError(t, err)
	first, _, err := st.CreatePersonFromParticipantContext(ctx, a)
	require.NoError(t, err)
	second, _, err := st.CreatePersonFromParticipantContext(ctx, b)
	require.NoError(t, err)
	_, err = st.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: first.ID, AbsorbedID: second.ID, ExpectedSurvivorRevision: first.Revision, ExpectedAbsorbedRevision: second.Revision, IdempotencyKey: "example-merge", Actor: "test"})
	require.NoError(t, err)
	stable, err := st.ImportantDateIdentityStableContext(ctx, first.ID)
	require.NoError(t, err)
	assert.False(t, stable)
}
