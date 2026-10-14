package carddav

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestRoomOnlyUnpublishedPersonUnpublishIsNoop(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	fixture := &mutationFixture{}
	service, st, personID, _ := seededMutationService(t, fixture)
	_, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "room@resource.calendar.google.com", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	requirements.NoError(service.UnpublishPerson(t.Context(), personID))
	assertions.Zero(fixture.deletes)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	requirements.NoError(err)
	requirements.Len(snapshot.Profile.ContactPoints, 1)
}

func TestRoomOnlyStoredPersonCanStillUnpublish(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	fixture := &mutationFixture{}
	service, st, personID, _ := seededMutationService(t, fixture)
	_, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "room@resource.calendar.google.com", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	requirements.NoError(err)
	_, err = st.GetPersonContext(t.Context(), personID)
	requirements.ErrorIs(err, store.ErrPersonNotFound)
	requirements.NoError(service.PublishPerson(t.Context(), personID))
	requirements.NoError(service.UnpublishPerson(t.Context(), personID))
	assertions.Equal(1, fixture.deletes)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	requirements.NoError(err)
	requirements.Len(snapshot.Profile.ContactPoints, 1)
	assertions.Equal("room@resource.calendar.google.com", snapshot.Profile.ContactPoints[0].NormalizedValue)
}
