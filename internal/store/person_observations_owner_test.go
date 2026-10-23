package store_test

import (
	"strings"
	"testing"
	"time"

	testassert "github.com/stretchr/testify/assert"
	testrequire "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func observationDecide(t *testing.T, st *store.Store, id int64, action personobs.DecisionAction) personobs.Observation {
	t.Helper()
	row := observationGet(t, st, id)
	result, err := st.DecidePersonObservation(t.Context(), personobs.Decision{ObservationID: id, Action: action, Actor: "owner", ExpectedUpdatedAt: row.UpdatedAt})
	testrequire.NoError(t, err)
	return result
}
func TestObservationSuppressionBlocksMachineButNotOwner(t *testing.T) {
	assert := testassert.New(t)
	require := testrequire.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	observationDecide(t, f.store, id, personobs.DecisionActionReject)
	result := applyObservationInput(t, f.store, in, generation)
	assert.Equal("suppressed", result.Items[0].Outcome)
	owner, err := f.store.AddOwnerObservation(t.Context(), personobs.OwnerInput{PersonID: in.PersonID, Content: in.Proposals[0].Content, Actor: "owner"})
	require.NoError(err)
	assert.Equal(personobs.OriginOwner, owner.Origin)
	assert.Equal(personfacts.DirectOther, owner.Directness)
	assert.Nil(owner.GenerationID)
	assert.Empty(owner.ProgramFingerprint)
	assert.Equal(1000, owner.ConfidenceBP)
}
func TestAddOwnerObservationClearsSuppression(t *testing.T) {
	assert := testassert.New(t)
	require := testrequire.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	id := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] swims")
	observationDecide(t, f.store, id, personobs.DecisionActionForget)
	row, err := f.store.AddOwnerObservation(t.Context(), personobs.OwnerInput{PersonID: in.PersonID, Content: "[PERSON] swims", Actor: "owner"})
	require.NoError(err)
	suppressions, err := f.store.ListObservationSuppressions(t.Context(), in.PersonID)
	require.NoError(err)
	assert.Empty(suppressions)
	detail := observationGet(t, f.store, row.ID)
	var lifted int
	for _, event := range detail.Events {
		if event.Event == "suppression-lifted" {
			lifted++
			assert.Equal("owner-readded", event.Detail)
		}
	}
	assert.Equal(1, lifted)
}
func TestDecideObservationRejectsStaleRevision(t *testing.T) {
	assert := testassert.New(t)
	require := testrequire.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	id := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] swims")
	before := observationGet(t, f.store, id)
	observationDecide(t, f.store, id, personobs.DecisionActionPin)
	_, err := f.store.DecidePersonObservation(t.Context(), personobs.Decision{ObservationID: id, Action: personobs.DecisionActionReject, ExpectedUpdatedAt: before.UpdatedAt, Actor: "owner"})
	require.ErrorIs(err, store.ErrObservationConflict)
	assert.Equal(personobs.StatusActive, observationGet(t, f.store, id).Status)
}
func TestForgetObservationKeepsHashOnlySuppression(t *testing.T) {
	assert := testassert.New(t)
	require := testrequire.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	id := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] has a private detail")
	before := observationGet(t, f.store, id)
	observationDecide(t, f.store, id, personobs.DecisionActionForget)
	_, err := f.store.GetPersonObservation(t.Context(), id)
	require.Error(err)
	rows, err := f.store.ListObservationSuppressions(t.Context(), in.PersonID)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(before.ContentKey, rows[0].ContentKey)
	assert.Equal("owner-forgotten", rows[0].Reason)
	var events int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_observation_events WHERE observation_id=?`), id).Scan(&events))
	assert.Zero(events)
}
func TestRejectObservationNotifiesDependents(t *testing.T) {
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	child := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] exercises", id)
	observationDecide(t, f.store, id, personobs.DecisionActionReject)
	testassert.Equal(t, personobs.StatusUnsupported, observationGet(t, f.store, child).Status)
}
func TestForgetObservationWithdrawsDeductionWithRemainingPremise(t *testing.T) {
	assert := testassert.New(t)
	require := testrequire.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	first := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] runs")
	second := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] swims")
	child := observationDerived(t, f.store, in, generation, personobs.LevelDeductive, "[PERSON] does both", first, second)
	observationDecide(t, f.store, first, personobs.DecisionActionForget)
	assert.Equal(personobs.StatusUnsupported, observationGet(t, f.store, child).Status)
	assert.NotNil(observationGet(t, f.store, child).PremiseChangedAt)
	require.NoError(f.store.RefreshObservationForTest(t.Context(), in.PersonID, child))
	assert.Equal(personobs.StatusUnsupported, observationGet(t, f.store, child).Status)
}
func TestAddOwnerObservationWithoutParticipants(t *testing.T) {
	assert := testassert.New(t)
	require := testrequire.New(t)
	st := testutil.NewTestStore(t)
	var personID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `INSERT INTO persons(vcard_uid) VALUES('owner-only-observation') RETURNING id`).Scan(&personID))
	row, err := st.AddOwnerObservation(t.Context(), personobs.OwnerInput{PersonID: personID, Content: "[person] has a mortgage", Categories: []personobs.Category{personobs.CategoryHealth}, Actor: "owner"})
	require.NoError(err)
	assert.Equal("[PERSON] has a mortgage", row.Content)
	assert.ElementsMatch([]personobs.Category{personobs.CategoryFinances, personobs.CategoryHealth}, row.Categories)
	assert.True(row.Sensitive)
	assert.Equal(personobs.StabilitySourceOwner, row.StabilitySource)
}
func TestOwnerObservationDecisions(t *testing.T) {
	assert := testassert.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	id := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] swims")
	assert.Equal(personobs.OwnerStateConfirmed, observationDecide(t, f.store, id, personobs.DecisionActionConfirm).OwnerState)
	assert.Equal(personobs.OwnerStateUnsure, observationDecide(t, f.store, id, personobs.DecisionActionUnsure).OwnerState)
	assert.Empty(observationDecide(t, f.store, id, personobs.DecisionActionClear).OwnerState)
	assert.True(observationDecide(t, f.store, id, personobs.DecisionActionPin).Pinned)
	assert.False(observationDecide(t, f.store, id, personobs.DecisionActionUnpin).Pinned)
	ended := observationDecide(t, f.store, id, personobs.DecisionActionEnd)
	assert.Equal(personobs.StatusSuperseded, ended.Status)
	assert.Equal(personobs.StatusReasonOwnerSuperseded, ended.StatusReason)
	assert.NotNil(ended.ValidUntil)
}
func TestOwnerObservationClearSuppression(t *testing.T) {
	assert := testassert.New(t)
	require := testrequire.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	id := observationOwnerRow(t, f.store, in.PersonID, "[PERSON] swims")
	key := observationGet(t, f.store, id).ContentKey
	observationDecide(t, f.store, id, personobs.DecisionActionReject)
	require.NoError(f.store.ClearObservationSuppression(t.Context(), in.PersonID, key, "owner"))
	rows, err := f.store.ListObservationSuppressions(t.Context(), in.PersonID)
	require.NoError(err)
	assert.Empty(rows)
	require.Error(f.store.ClearObservationSuppression(t.Context(), in.PersonID, strings.Repeat("z", 64), "owner"))
}
func TestAddOwnerObservationAdoptsExistingMachineRow(t *testing.T) {
	assert := testassert.New(t)
	require := testrequire.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	status := observationStatusInput(t, f.store, in.PersonID, id, false, 1)
	row, err := f.store.AddOwnerObservation(t.Context(), personobs.OwnerInput{PersonID: in.PersonID, Content: in.Proposals[0].Content, Actor: "owner"})
	require.NoError(err)
	assert.Equal(id, row.ID)
	assert.Equal(personobs.OriginOwner, row.Origin)
	assert.Nil(row.GenerationID)
	assert.Equal(1000, row.ConfidenceBP)
	_, err = f.store.ApplyPersonFactGenerationContext(t.Context(), status, nil)
	require.NoError(err)
	assert.Equal(personobs.StatusActive, observationGet(t, f.store, id).Status)
}
func TestOwnerObservationEndRejectsReversedValidity(t *testing.T) {
	f, in, _ := observationBatchFixture(t, "I run")
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	row, err := f.store.AddOwnerObservation(t.Context(), personobs.OwnerInput{PersonID: in.PersonID, Content: "[PERSON] will swim", ValidFrom: &start, Actor: "owner"})
	testrequire.NoError(t, err)
	end := start.Add(-time.Hour)
	_, err = f.store.DecidePersonObservation(t.Context(), personobs.Decision{ObservationID: row.ID, Action: personobs.DecisionActionEnd, ExpectedUpdatedAt: row.UpdatedAt, EndAt: &end, Actor: "owner"})
	testrequire.Error(t, err)
}
