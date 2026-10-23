package store_test

import (
	testassert "github.com/stretchr/testify/assert"
	testrequire "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personobs"
	"testing"
)

func TestObservationRepeatedRejectReplacesSuppressionDecision(t *testing.T) {
	assert := testassert.New(t)
	require := testrequire.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	id := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] swims")
	before := observationGet(t, f.store, id)
	row, err := f.store.DecidePersonObservation(t.Context(), personobs.Decision{ObservationID: id, Action: personobs.DecisionActionReject, Reason: "outdated", Actor: "owner", ExpectedUpdatedAt: before.UpdatedAt})
	require.NoError(err)
	rows, err := f.store.ListObservationSuppressions(t.Context(), in.PersonID)
	require.NoError(err)
	require.Len(rows, 1)
	rejectedAt := rows[0].RejectedAt
	_, err = f.store.DecidePersonObservation(t.Context(), personobs.Decision{ObservationID: id, Action: personobs.DecisionActionReject, Reason: "wrong", Actor: "owner", ExpectedUpdatedAt: row.UpdatedAt})
	require.NoError(err)
	rows, err = f.store.ListObservationSuppressions(t.Context(), in.PersonID)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal("wrong", rows[0].Reason)
	assert.False(rows[0].RejectedAt.Before(rejectedAt))
	observationDecide(t, f.store, id, personobs.DecisionActionForget)
	rows, err = f.store.ListObservationSuppressions(t.Context(), in.PersonID)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal("owner-forgotten", rows[0].Reason)
}
