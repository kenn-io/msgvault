package store_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
)

func TestOwnerObservationAdoptionUsesOwnerMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	until := from.AddDate(1, 0, 0)
	row, err := f.store.AddOwnerObservation(t.Context(), personobs.OwnerInput{PersonID: in.PersonID, Content: in.Proposals[0].Content, Kind: personobs.KindPlan, Stability: personobs.StabilityFixed, ValidFrom: &from, ValidUntil: &until, Actor: "owner"})
	require.NoError(err)
	assert.Equal(id, row.ID)
	assert.Equal(personobs.KindPlan, row.Kind)
	assert.Equal(personobs.StabilityFixed, row.Stability)
	assert.Equal(&from, row.ValidFrom)
	assert.Equal(&until, row.ValidUntil)
}
func TestOwnerObservationRejectsInvalidCalendarDate(t *testing.T) {
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	for _, date := range []*personfacts.PartialDateValue{{Year: 2026, Month: 2, Day: 30}, {Year: 2026, Month: 13}, {Year: 0}, {Year: 2026, Day: 3}} {
		_, err := f.store.AddOwnerObservation(t.Context(), personobs.OwnerInput{PersonID: in.PersonID, Content: "[PERSON] will swim", OccursOn: date, Actor: "owner"})
		require.Error(err, date)
	}
}

func TestOwnerObservationConfirmedAndProtectedFromMachine(t *testing.T) {
	for _, adopt := range []bool{false, true} {
		t.Run(strconv.FormatBool(adopt), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, in, generation := observationBatchFixture(t, "I run")
			if adopt {
				id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
				observationDecide(t, f.store, id, personobs.DecisionActionUnsure)
			}
			row, err := f.store.AddOwnerObservation(t.Context(), personobs.OwnerInput{PersonID: in.PersonID, Content: in.Proposals[0].Content})
			require.NoError(err)
			assert.Equal(personobs.OwnerStateConfirmed, row.OwnerState)
			in.Proposals[0].Content = "[PERSON] runs every morning"
			in.Proposals[0].SupersedesIDs = []int64{row.ID}
			result := applyObservationInput(t, f.store, in, generation)
			assert.Equal("supersede-blocked", result.Items[0].Outcome)
			detail := observationGet(t, f.store, row.ID)
			assert.Equal(personobs.StatusActive, detail.Status)
		})
	}
}
func TestOwnerObservationOffsetValidity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, in, _ := observationBatchFixture(t, "I run")
	from := time.Date(2026, 10, 22, 9, 0, 0, 0, time.FixedZone("west", -7*3600))
	until := from.Add(2 * time.Hour)
	row, err := f.store.AddOwnerObservation(t.Context(), personobs.OwnerInput{PersonID: in.PersonID, Content: "[PERSON] runs", ValidFrom: &from, ValidUntil: &until})
	require.NoError(err)
	for _, tc := range []struct{ hour, total int }{{15, 0}, {17, 1}, {19, 0}} {
		asof := time.Date(2026, 10, 22, tc.hour, 0, 0, 0, time.UTC)
		page, err := f.store.ListPersonObservations(t.Context(), in.PersonID, personobs.Filter{AsOf: &asof})
		require.NoError(err)
		assert.Equal(tc.total, page.Total, tc.hour)
	}
	end := from.Add(time.Hour)
	_, err = f.store.DecidePersonObservation(t.Context(), personobs.Decision{ObservationID: row.ID, Action: personobs.DecisionActionEnd, EndAt: &end, ExpectedUpdatedAt: row.UpdatedAt})
	require.NoError(err)
	asof := time.Date(2026, 10, 22, 16, 30, 0, 0, time.UTC)
	page, err := f.store.ListPersonObservations(t.Context(), in.PersonID, personobs.Filter{AsOf: &asof})
	require.NoError(err)
	assert.Equal(1, page.Total)
	// This interval is ordered in absolute time but has reversed local clock times.
	mixedFrom := time.Date(2026, 10, 22, 23, 0, 0, 0, time.FixedZone("east", 2*3600))
	mixedUntil := time.Date(2026, 10, 22, 20, 0, 0, 0, time.FixedZone("west", -2*3600))
	_, err = f.store.AddOwnerObservation(t.Context(), personobs.OwnerInput{PersonID: in.PersonID, Content: "[PERSON] swims", ValidFrom: &mixedFrom, ValidUntil: &mixedUntil})
	require.NoError(err)
}
