package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDirectoryPeopleForParticipantsResolvesCadenceAndOmitsUnbound(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := createDirectoryPerson(t, st, "Cadence Person", "lookup-cadence@example.test", "friend", "inactive", "Example Co")
	days := int64(21)
	_, err := st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency,
		Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser,
	})
	require.NoError(t, err)

	participantID := person.ParticipantIDs[0]
	people, err := st.DirectoryPeopleForParticipantsContext(t.Context(), []int64{participantID, 999999, participantID, 0, -1})
	require.NoError(t, err)
	require.Len(t, people, 1)
	got, ok := people[participantID]
	require.True(t, ok)
	assert.Equal(t, person.ID, got.ID)
	assert.Equal(t, &days, got.Cadence.FrequencyDays)
	assert.Equal(t, "A", got.Cadence.List)
	assert.Equal(t, store.CadenceUnknown, got.Cadence.Status)
	assert.NotContains(t, people, int64(999999))
}

func TestDirectoryPeopleForParticipantsReturnsEmptyForEmptyInput(t *testing.T) {
	st := testutil.NewTestStore(t)
	people, err := st.DirectoryPeopleForParticipantsContext(t.Context(), nil)
	require.NoError(t, err)
	assert.Empty(t, people)
}
