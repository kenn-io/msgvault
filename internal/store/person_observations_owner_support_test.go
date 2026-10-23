package store_test

import (
	testassert "github.com/stretchr/testify/assert"
	testrequire "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personobs"
	"testing"
)

func TestObservationClearConfirmationRechecksWithdrawnSupport(t *testing.T) {
	for _, action := range []personobs.DecisionAction{personobs.DecisionActionClear, personobs.DecisionActionUnsure} {
		t.Run(string(action), func(t *testing.T) {
			f, in, generation := observationBatchFixture(t, "I run")
			id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
			observationDecide(t, f.store, id, personobs.DecisionActionConfirm)
			_, err := f.store.ApplyPersonFactGenerationContext(t.Context(), observationStatusInput(t, f.store, in.PersonID, id, false, 1), nil)
			testrequire.NoError(t, err)
			testassert.Equal(t, personobs.SupportFlagLapsed, observationGet(t, f.store, id).SupportFlag)
			current := observationDecide(t, f.store, id, action)
			testassert.Equal(t, personobs.StatusUnsupported, current.Status)
		})
	}
}

func TestAddOwnerObservationAdoptionDropsCitationCounts(t *testing.T) {
	assert := testassert.New(t)
	require := testrequire.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	require.Equal(1, observationGet(t, f.store, id).MentionCount)
	row, err := f.store.AddOwnerObservation(t.Context(), personobs.OwnerInput{PersonID: in.PersonID, Content: in.Proposals[0].Content, Actor: "owner"})
	require.NoError(err)
	assert.Zero(row.MentionCount)
	assert.Nil(row.FirstSeenAt)
	assert.Nil(row.LastSeenAt)
	assert.Zero(row.SupportedCount)
}
