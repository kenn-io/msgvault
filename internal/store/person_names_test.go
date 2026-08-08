package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonNamesRetainStructuredComponentsAndRFC9554Fields(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)

	structured, err := st.AddPersonNameContext(ctx, personID, store.PersonNameInput{
		NameKind: store.PersonNameStructured, FamilyName: new("Example"),
		GivenName: new("Alice"), AdditionalNames: new("Q"),
		HonorificPrefixes: new("Dr."), HonorificSuffixes: new("PhD"),
		SecondarySurname: new("Sample"), Generation: new("Jr."),
		Script: new("Latn"), SortAs: new("Example,Alice"),
		OriginalValue: "Example;Alice;Q;Dr.;PhD;Sample;Jr.",
		Envelope: store.ValueEnvelope{Source: store.ProvenanceVCardImport,
			VCard: store.VCardIdentity{Property: "N", PropID: new("n1")}},
	})
	require.NoError(err)
	assert.Equal(store.PersonNameStructured, structured.NameKind)
	assert.Equal("Sample", *structured.SecondarySurname)
	assert.Equal("Jr.", *structured.Generation)
	assert.True(structured.Envelope.IsCurrent())

	_, err = st.AddPersonNameContext(ctx, personID, store.PersonNameInput{
		NameKind: store.PersonNamePhonetic, GivenName: new("synthetic"),
		Language: new("en"), PhoneticSystem: new("ipa"),
		OriginalValue: ";synthetic;;;",
		Envelope: store.ValueEnvelope{Source: store.ProvenanceVCardImport,
			VCard: store.VCardIdentity{Property: "N", AltID: new("1")}},
	})
	require.NoError(err)
	names, err := st.ListPersonNamesContext(ctx, personID, true)
	require.NoError(err)
	assert.Len(names, 2)
}

func TestPersonNamesKeepMultipleFormattedFormsPerLanguage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)

	for _, form := range []struct {
		formatted, language string
		pref                int
	}{{"Alice Example", "en", 1}, {"Synthetic Alternate", "ja", 2}} {
		_, err := st.AddPersonNameContext(ctx, personID, store.PersonNameInput{
			NameKind: store.PersonNameFormatted, Formatted: new(form.formatted),
			Language: new(form.language), OriginalValue: form.formatted,
			Envelope: store.ValueEnvelope{Source: store.ProvenanceCardDAVImport,
				Pref: new(form.pref), VCard: store.VCardIdentity{Property: "FN", AltID: new("1")}},
		})
		require.NoError(err)
	}
	names, err := st.ListPersonNamesContext(ctx, personID, true)
	require.NoError(err)
	require.Len(names, 2)
	assert.Equal(1, *names[0].Envelope.Pref)
	assert.Equal("Alice Example", *names[0].Formatted)
}

func TestSupersedePersonNameClosesBothTimeAxes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	name, err := st.AddPersonNameContext(ctx, personID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alice Example"),
		OriginalValue: "Alice Example", Envelope: store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	require.NoError(st.SupersedePersonNameContext(ctx, personID, name.Envelope.ID, nil))
	current, err := st.ListPersonNamesContext(ctx, personID, true)
	require.NoError(err)
	assert.Empty(current)
	all, err := st.ListPersonNamesContext(ctx, personID, false)
	require.NoError(err)
	require.Len(all, 1)
	assert.NotNil(all[0].Envelope.SupersededAt)
	assert.NotNil(all[0].Envelope.ActiveUntil)
	assert.False(all[0].Envelope.IsCurrent())
}

func TestAddPersonNameRejectsInvalidInput(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	_, err := st.AddPersonNameContext(ctx, personID, store.PersonNameInput{
		NameKind: "middle", Formatted: new("Alice Example"),
		Envelope: store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrInvalidPersonNameKind)
	_, err = st.AddPersonNameContext(ctx, personID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Envelope: store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrPersonNameValueMissing)
	_, err = st.AddPersonNameContext(ctx, 999999, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alice Example"),
		Envelope: store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrPersonNotFound)
}

func partialDate(year, month, day int) store.PartialDate {
	date := store.PartialDate{}
	if year != 0 {
		date.Year = new(year)
	}
	if month != 0 {
		date.Month = new(month)
	}
	if day != 0 {
		date.Day = new(day)
	}
	return date
}

func newTestPerson(t *testing.T, st *store.Store) int64 {
	t.Helper()
	require := require.New(t)
	participantID, err := st.EnsureParticipantByIdentifier("email", "alice@example.com", "Alice Example")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(context.Background(), participantID)
	require.NoError(err)
	return person.ID
}
