package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSupersedeDefaultsFutureDatedProfileCloseToActiveFrom(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	personID := newTestPerson(t, st)
	activeFrom := time.Date(2099, 6, 2, 12, 0, 0, 0, time.UTC)
	envelope := store.ValueEnvelopeInput{Source: store.ProvenanceUser, ActiveFrom: &activeFrom}

	name, err := st.AddPersonNameContext(ctx, personID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Future Name"), Envelope: envelope,
	})
	require.NoError(err)
	point, err := st.AddPersonContactPointContext(ctx, personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "future@example.org", Envelope: envelope,
	})
	require.NoError(err)
	address, err := st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: store.PersonAddressPostal, StreetAddress: new("Future Street"), Envelope: envelope,
	})
	require.NoError(err)
	date, err := st.AddPersonDateContext(ctx, personID, store.PersonDateInput{
		DateKind: store.PersonDateBirthday, Date: partialDate(2099, 6, 2), Envelope: envelope,
	})
	require.NoError(err)
	category, err := st.AddPersonCategoryContext(ctx, personID, store.PersonCategoryInput{
		OriginalValue: "Future Category", Envelope: envelope,
	})
	require.NoError(err)
	media, err := st.AddPersonMediaContext(ctx, personID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto, Data: []byte("future-photo"), Envelope: envelope,
	})
	require.NoError(err)

	values := []struct {
		table string
		id    int64
		close func() error
	}{
		{"person_names", name.Envelope.ID, func() error {
			return st.SupersedePersonNameContext(ctx, personID, name.Envelope.ID, nil)
		}},
		{"person_contact_points", point.Envelope.ID, func() error {
			return st.SupersedePersonContactPointContext(ctx, personID, point.Envelope.ID, nil)
		}},
		{"person_addresses", address.Envelope.ID, func() error {
			return st.SupersedePersonAddressContext(ctx, personID, address.Envelope.ID, nil)
		}},
		{"person_dates", date.Envelope.ID, func() error {
			return st.SupersedePersonDateContext(ctx, personID, date.Envelope.ID, nil)
		}},
		{"person_categories", category.Envelope.ID, func() error {
			return st.SupersedePersonCategoryContext(ctx, personID, category.Envelope.ID, nil)
		}},
		{"person_media", media.Envelope.ID, func() error {
			return st.SupersedePersonMediaContext(ctx, personID, media.Envelope.ID, nil)
		}},
	}
	for _, value := range values {
		require.NoError(value.close(), value.table)
		var activeUntil sql.NullTime
		err := st.DB().QueryRow(
			st.Rebind("SELECT active_until FROM "+value.table+" WHERE id = ?"), value.id,
		).Scan(&activeUntil)
		require.NoError(err, value.table)
		require.True(activeUntil.Valid, value.table)
		assert.True(activeFrom.Equal(activeUntil.Time), value.table)
	}
}

func TestSupersedeRejectsCloseBeforeStoredActiveFrom(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	activeFrom := time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC)
	tooEarly := activeFrom.Add(-time.Second)
	envelope := store.ValueEnvelopeInput{Source: store.ProvenanceUser, ActiveFrom: &activeFrom}

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
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation, ActiveFrom: &activeFrom},
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

func TestSupersedeDefaultsFutureDatedObservationCloseToActiveFrom(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	participantID, err := st.EnsureParticipantByIdentifier("example", "future-observation", "Future User")
	require.NoError(err)
	activeFrom := time.Date(2099, 6, 2, 12, 0, 0, 0, time.UTC)
	result, err := st.RecordContactObservationContext(ctx, participantID, store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "future-observation@example.org",
		Envelope: store.ValueEnvelopeInput{
			Source: store.ProvenanceArchiveObservation, ActiveFrom: &activeFrom,
		},
	})
	require.NoError(err)

	require.NoError(st.SupersedeParticipantObservationContext(
		ctx, participantID, result.Observation.Envelope.ID, nil,
	))
	observations, err := st.ListParticipantObservationsContext(ctx, participantID, false)
	require.NoError(err)
	require.Len(observations, 1)
	require.NotNil(observations[0].Envelope.ActiveUntil)
	assert.True(activeFrom.Equal(*observations[0].Envelope.ActiveUntil))
}

func TestSupersedePreservesExistingWorldTimeClose(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	personID := newTestPerson(t, st)
	activeFrom := time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC)
	activeUntil := activeFrom.Add(24 * time.Hour)
	name, err := st.AddPersonNameContext(ctx, personID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Historical Name"),
		Envelope: store.ValueEnvelopeInput{
			Source: store.ProvenanceVCardImport, ActiveFrom: &activeFrom, ActiveUntil: &activeUntil,
		},
	})
	require.NoError(err)

	require.NoError(st.SupersedePersonNameContext(ctx, personID, name.Envelope.ID, nil))
	history, err := st.GetPersonProfileHistoryContext(ctx, personID)
	require.NoError(err)
	require.Len(history.Names, 1)
	require.NotNil(history.Names[0].Envelope.ActiveUntil)
	assert.True(activeUntil.Equal(*history.Names[0].Envelope.ActiveUntil))
	assert.NotNil(history.Names[0].Envelope.SupersededAt)
}

func TestSupersedeObservationPreservesExistingWorldTimeClose(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	participantID, err := st.EnsureParticipantByIdentifier("example", "participant-history", "Test User")
	require.NoError(err)
	activeFrom := time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC)
	activeUntil := activeFrom.Add(24 * time.Hour)
	result, err := st.RecordContactObservationContext(ctx, participantID, store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "history@example.com",
		Envelope: store.ValueEnvelopeInput{
			Source: store.ProvenanceArchiveObservation, ActiveFrom: &activeFrom, ActiveUntil: &activeUntil,
		},
	})
	require.NoError(err)

	require.NoError(st.SupersedeParticipantObservationContext(
		ctx, participantID, result.Observation.Envelope.ID, nil,
	))
	observations, err := st.ListParticipantObservationsContext(ctx, participantID, false)
	require.NoError(err)
	require.Len(observations, 1)
	require.NotNil(observations[0].Envelope.ActiveUntil)
	assert.True(activeUntil.Equal(*observations[0].Envelope.ActiveUntil))
	assert.NotNil(observations[0].Envelope.SupersededAt)
}
