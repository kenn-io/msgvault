package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonAddressRoundTripsStructuredComponentsAndMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)

	address, err := st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: store.PersonAddressPostal, PostOfficeBox: new("PO Box 42"),
		ExtendedAddress: new("Suite 3"), StreetAddress: new("123 Example St."),
		Locality: new("Exampleville"), Region: new("CA"), PostalCode: new("90000"),
		CountryName:        new("United States"),
		ExtendedComponents: new("Room 5;Apt 2;Floor 3;123;Example St.;;;;;"),
		Label:              new("Home\nExampleville"), GeoURI: new("geo:37.386,-122.084"),
		Timezone: new("America/Los_Angeles"), CountryCode: new("US"),
		OriginalValue: "PO Box 42;Suite 3;123 Example St.;Exampleville;CA;90000;United States",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceVCardImport,
			SourceRef: new("resource-1"), Pref: new(1),
			TypeTokens: []string{"home"}, VCard: store.VCardIdentity{
				Property: "ADR", PropID: new("a1"), Group: new("item1"),
			}},
	})
	require.NoError(err)
	assert.Equal("123 Example St.", *address.StreetAddress)
	assert.Equal("geo:37.386,-122.084", *address.GeoURI)
	stored, err := st.ListPersonAddressesContext(ctx, personID, true)
	require.NoError(err)
	require.Len(stored, 1)
	assert.Equal("a1", *stored[0].Envelope.VCard.PropID)
}

func TestBirthAndDeathPlacesAreAddressRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	_, err := st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: store.PersonAddressBirthPlace, FreeText: new("Exampleville, CA"),
		OriginalValue: "Exampleville, CA",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceVCardImport,
			VCard: store.VCardIdentity{Property: "BIRTHPLACE"}},
	})
	require.NoError(err)
	_, err = st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: store.PersonAddressDeathPlace, PlaceURI: new("geo:37.386,-122.084"),
		OriginalValue: "geo:37.386,-122.084",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceVCardImport,
			VCard: store.VCardIdentity{Property: "DEATHPLACE"}},
	})
	require.NoError(err)
	addresses, err := st.ListPersonAddressesContext(ctx, personID, true)
	require.NoError(err)
	require.Len(addresses, 2)
	assert.Equal(store.PersonAddressBirthPlace, addresses[0].AddressKind)
	assert.Equal(store.PersonAddressDeathPlace, addresses[1].AddressKind)
}

func TestPersonAddressDerivesOriginalValueFromAlternateRepresentation(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		apply func(*store.PersonAddressInput, *string)
		check func(*testing.T, *store.PersonAddress)
	}{
		{
			name: "free text", value: "Exampleville, CA",
			apply: func(input *store.PersonAddressInput, value *string) {
				input.FreeText = value
			},
		},
		{
			name: "geo URI", value: "geo:37.386,-122.084",
			apply: func(input *store.PersonAddressInput, value *string) {
				input.GeoURI = value
			},
		},
		{
			name: "place URI", value: "https://example.invalid/places/42",
			apply: func(input *store.PersonAddressInput, value *string) {
				input.PlaceURI = value
			},
		},
		{
			name: "label", value: "Home address",
			apply: func(input *store.PersonAddressInput, value *string) {
				input.Label = value
			},
			check: func(t *testing.T, address *store.PersonAddress) {
				t.Helper()
				require.NotNil(t, address.Label)
				assert.Equal(t, "Home address", *address.Label)
				assert.Nil(t, address.CountryCode)
			},
		},
		{
			name: "country code", value: "US",
			apply: func(input *store.PersonAddressInput, value *string) {
				input.CountryCode = value
			},
			check: func(t *testing.T, address *store.PersonAddress) {
				t.Helper()
				require.NotNil(t, address.CountryCode)
				assert.Equal(t, "US", *address.CountryCode)
				assert.Nil(t, address.Label)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			st := storetest.New(t).Store
			input := store.PersonAddressInput{
				AddressKind: store.PersonAddressBirthPlace,
				Envelope:    store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}
			test.apply(&input, &test.value)

			personID := newTestPerson(t, st)
			address, err := st.AddPersonAddressContext(
				t.Context(), personID, input,
			)
			require.NoError(err)
			require.Equal(test.value, address.OriginalValue)
			if test.check != nil {
				test.check(t, address)
			}
			stored, err := st.ListPersonAddressesContext(t.Context(), personID, true)
			require.NoError(err)
			require.Len(stored, 1)
			require.Equal(test.value, stored[0].OriginalValue)
			if test.check != nil {
				test.check(t, &stored[0])
			}
		})
	}
}

func TestPersonAddressValidationAndSupersession(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	_, err := st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: "billing", StreetAddress: new("123 Example St."),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrInvalidPersonAddressKind)
	_, err = st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: store.PersonAddressPostal,
		Envelope:    store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrPersonAddressValueMissing)
	for _, input := range []store.PersonAddressInput{
		{AddressKind: store.PersonAddressPostal, Label: new(" \t\n "),
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}},
		{AddressKind: store.PersonAddressPostal, CountryCode: new(" \t\n "),
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}},
	} {
		_, err = st.AddPersonAddressContext(ctx, personID, input)
		require.ErrorIs(err, store.ErrPersonAddressValueMissing)
	}
	address, err := st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: store.PersonAddressPostal, StreetAddress: new("123 Example St."),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	require.NoError(st.SupersedePersonAddressContext(ctx, personID, address.Envelope.ID, nil))
	current, err := st.ListPersonAddressesContext(ctx, personID, true)
	require.NoError(err)
	assert.Empty(current)
}
