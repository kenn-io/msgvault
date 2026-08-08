package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSupersedeRejectsCloseBeforeStoredActiveFrom(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	activeFrom := time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC)
	tooEarly := activeFrom.Add(-time.Second)
	envelope := store.ValueEnvelope{Source: store.ProvenanceUser, ActiveFrom: &activeFrom}

	name, err := st.AddPersonNameContext(ctx, personID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alice Example"), Envelope: envelope,
	})
	require.NoError(err)
	point, err := st.AddPersonContactPointContext(ctx, personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.com", Envelope: envelope,
	})
	require.NoError(err)
	address, err := st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: store.PersonAddressPostal, StreetAddress: new("123 Example St."), Envelope: envelope,
	})
	require.NoError(err)
	date, err := st.AddPersonDateContext(ctx, personID, store.PersonDateInput{
		DateKind: store.PersonDateBirthday, Date: partialDate(1985, 4, 12), Envelope: envelope,
	})
	require.NoError(err)
	category, err := st.AddPersonCategoryContext(ctx, personID, store.PersonCategoryInput{
		OriginalValue: "Friends", Envelope: envelope,
	})
	require.NoError(err)
	media, err := st.AddPersonMediaContext(ctx, personID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto, Data: []byte("synthetic-photo"), Envelope: envelope,
	})
	require.NoError(err)

	closers := []struct {
		name  string
		close func() error
	}{
		{"name", func() error { return st.SupersedePersonNameContext(ctx, personID, name.Envelope.ID, &tooEarly) }},
		{"contact point", func() error {
			return st.SupersedePersonContactPointContext(ctx, personID, point.Envelope.ID, &tooEarly)
		}},
		{"address", func() error { return st.SupersedePersonAddressContext(ctx, personID, address.Envelope.ID, &tooEarly) }},
		{"date", func() error { return st.SupersedePersonDateContext(ctx, personID, date.Envelope.ID, &tooEarly) }},
		{"category", func() error { return st.SupersedePersonCategoryContext(ctx, personID, category.Envelope.ID, &tooEarly) }},
		{"media", func() error { return st.SupersedePersonMediaContext(ctx, personID, media.Envelope.ID, &tooEarly) }},
	}
	for _, closer := range closers {
		require.ErrorIs(closer.close(), store.ErrProfileValueCloseBeforeActive, closer.name)
	}

	profile, err := st.GetPersonProfileContext(ctx, personID)
	require.NoError(err)
	assert.Len(profile.Names, 1)
	assert.Len(profile.ContactPoints, 1)
	assert.Len(profile.Addresses, 1)
	assert.Len(profile.Dates, 1)
	assert.Len(profile.Categories, 1)
	assert.Len(profile.Media, 1)
}

func TestSupersedeObservationRejectsCloseBeforeStoredActiveFrom(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	participantID, err := st.EnsureParticipantByIdentifier("example", "participant-1", "Test User")
	require.NoError(err)
	activeFrom := time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC)
	tooEarly := activeFrom.Add(-time.Second)
	result, err := st.RecordContactObservationContext(ctx, participantID, store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "user@example.com",
		Envelope: store.ValueEnvelope{Source: store.ProvenanceArchiveObservation, ActiveFrom: &activeFrom},
	})
	require.NoError(err)

	err = st.SupersedeParticipantObservationContext(
		ctx, participantID, result.Observation.Envelope.ID, &tooEarly,
	)
	require.ErrorIs(err, store.ErrProfileValueCloseBeforeActive)
	observations, err := st.ListParticipantObservationsContext(ctx, participantID, true)
	require.NoError(err)
	assert.Len(observations, 1)
}
