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
		Envelope: store.ValueEnvelope{Source: store.ProvenanceVCardImport,
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
		Envelope: store.ValueEnvelope{Source: store.ProvenanceVCardImport,
			VCard: store.VCardIdentity{Property: "BIRTHPLACE"}},
	})
	require.NoError(err)
	_, err = st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: store.PersonAddressDeathPlace, PlaceURI: new("geo:37.386,-122.084"),
		OriginalValue: "geo:37.386,-122.084",
		Envelope: store.ValueEnvelope{Source: store.ProvenanceVCardImport,
			VCard: store.VCardIdentity{Property: "DEATHPLACE"}},
	})
	require.NoError(err)
	addresses, err := st.ListPersonAddressesContext(ctx, personID, true)
	require.NoError(err)
	require.Len(addresses, 2)
	assert.Equal(store.PersonAddressBirthPlace, addresses[0].AddressKind)
	assert.Equal(store.PersonAddressDeathPlace, addresses[1].AddressKind)
}

func TestPersonAddressValidationAndSupersession(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	_, err := st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: "billing", StreetAddress: new("123 Example St."),
		Envelope: store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrInvalidPersonAddressKind)
	_, err = st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: store.PersonAddressPostal,
		Envelope:    store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrPersonAddressValueMissing)
	address, err := st.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind: store.PersonAddressPostal, StreetAddress: new("123 Example St."),
		Envelope: store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	require.NoError(st.SupersedePersonAddressContext(ctx, personID, address.Envelope.ID, nil))
	current, err := st.ListPersonAddressesContext(ctx, personID, true)
	require.NoError(err)
	assert.Empty(current)
}
