package vcardmap

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

func TestProjectTemporalValuesUseVCardWireSyntax(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dateValue := "2026-07-30"
	timestamp := time.Date(2026, time.July, 30, 12, 34, 56, 123000000, time.UTC)
	snapshot := store.PersonVCardSnapshot{
		Profile: store.PersonProfile{
			Person: store.Person{ID: 1},
			Dates: []store.PersonDate{
				{
					Envelope: store.ValueEnvelope{ID: 1, Source: store.ProvenanceUser},
					DateKind: store.PersonDateBirthday,
					Date: store.PartialDate{
						Year: new(1985), Month: new(4), Day: new(12),
					},
				},
				{
					Envelope: store.ValueEnvelope{ID: 2, Source: store.ProvenanceUser},
					DateKind: store.PersonDateAnniversary,
					Date:     store.PartialDate{Month: new(4), Day: new(12)},
				},
			},
		},
		Attributes: []store.PersonVCardAttribute{
			{
				Definition: store.AttributeDefinition{
					ID: 1, Slug: "start-date", VCardProperty: new("X-START-DATE"),
				},
				Values: []store.PersonAttributeValue{{
					ID: 3, Value: store.AttributeValue{
						Type: store.AttributeValueDate, Date: &dateValue,
					},
				}},
			},
			{
				Definition: store.AttributeDefinition{
					ID: 2, Slug: "seen-at", VCardProperty: new("X-SEEN-AT"),
				},
				Values: []store.PersonAttributeValue{{
					ID: 4, Value: store.AttributeValue{
						Type: store.AttributeValueTimestamp, Timestamp: &timestamp,
					},
				}},
			},
		},
	}

	properties, _, err := projectPersonProperties(snapshot)
	require.NoError(err)
	assert.Equal("19850412",
		projectedByOwner(t, properties, "person_dates", 1, "date").Property.RawValue)
	assert.Equal("--0412",
		projectedByOwner(t, properties, "person_dates", 2, "date").Property.RawValue)
	assert.Equal("20260730",
		projectedByOwner(t, properties, "person_attribute_values", 3, "value").Property.RawValue)
	assert.Equal("20260730T123456Z",
		projectedByOwner(t, properties, "person_attribute_values", 4, "value").Property.RawValue)
}

func TestProjectPersonEnvelopePrefersPersistedMappingAndPreservesUnownedParameters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n" +
		"FN;X-KEEP=opaque:Mapped Old\r\nFN;PROP-ID=identity-name:Identity Old\r\n" +
		"X-VENDOR:untouched\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	first := projectOccurrence(t, envelope, "FN", 0)
	second := projectOccurrence(t, envelope, "FN", 1)
	require.NotNil(second.Identity.PropID)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: first.Identity, SourceRef: envelope.SourceRef,
		Table: "person_names", RowID: 7, Field: "formatted",
		Kind: vcard.HandlingNative,
	}}
	envelope.Residue = vcard.ResidueWithMappings(
		envelope.PropertyTree, envelope.NativeMappings,
	)
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Names: []store.PersonName{{
			Envelope: store.ValueEnvelope{
				ID: 7, Source: store.ProvenanceUser,
				VCard: store.VCardIdentity{
					Property: "FN", Group: secondIdentityGroup(second.Identity),
					PropID: second.Identity.PropID,
				},
			},
			NameKind: store.PersonNameFormatted, Formatted: new("Typed Name"),
		}},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	assert.Equal(raw, envelope.StoredBody, "projection must not mutate its input")
	updated := projectOccurrenceByIdentity(t, projected, first.Identity)
	assert.Equal("Typed Name", updated.Property.RawValue)
	require.Len(updated.Property.ParametersNamed("X-KEEP"), 1)
	assert.Equal("opaque", updated.Property.ParametersNamed("X-KEEP")[0].Values[0].Decoded)
	unchanged := projectOccurrenceByIdentity(t, projected, second.Identity)
	assert.Equal("Identity Old", unchanged.Property.RawValue)
	require.Len(projected.NativeMappings, 1)
	assert.True(first.Identity.Equal(projected.NativeMappings[0].Identity))
	assert.Contains(string(projected.StoredBody), "X-VENDOR:untouched")
}

func TestProjectPersonEnvelopeUsesExactReducedIdentityThenAppendsWithoutNameMatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n" +
		"item1.FN;PROP-ID=source-name:Identity Old\r\n" +
		"FN:Unclaimed Same Name\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	group, propID := "item1", "source-name"
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Names: []store.PersonName{
			{
				Envelope: store.ValueEnvelope{ID: 8, Source: store.ProvenanceUser,
					VCard: store.VCardIdentity{
						Property: "FN", Group: &group, PropID: &propID,
					}},
				NameKind: store.PersonNameFormatted, Formatted: new("Identity New"),
			},
			{
				Envelope: store.ValueEnvelope{ID: 9, Source: store.ProvenanceUser},
				NameKind: store.PersonNameFormatted, Formatted: new("Appended New"),
			},
		},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	identity := projectOccurrence(t, projected, "FN", 0)
	assert.Equal("Identity New", identity.Property.RawValue)
	unclaimed := projectOccurrence(t, projected, "FN", 1)
	assert.Equal("Unclaimed Same Name", unclaimed.Property.RawValue)
	appended := projectOccurrence(t, projected, "FN", 2)
	assert.Equal("Appended New", appended.Property.RawValue)
	assert.GreaterOrEqual(appended.Identity.Ordinal, envelope.NextOccurrenceOrdinal)
	require.Len(projected.NativeMappings, 2)
	assert.True(identity.Identity.Equal(projected.NativeMappings[0].Identity))
	assert.True(appended.Identity.Equal(projected.NativeMappings[1].Identity))
}

func TestProjectPersonEnvelopeDeletesOnlyStaleMappedOwner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n" +
		"FN:Mapped Stale\r\nFN:Residue Survives\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	stale := projectOccurrence(t, envelope, "FN", 0)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: stale.Identity, SourceRef: envelope.SourceRef,
		Table: "person_names", RowID: 10, Field: "formatted",
		Kind: vcard.HandlingNative,
	}}

	displayName := "Residue Survives"
	projected, err := ProjectPersonEnvelope(store.PersonVCardSnapshot{
		Profile: store.PersonProfile{Person: store.Person{
			ID: 1, DisplayName: &displayName,
		}},
	}, envelope)
	require.NoError(err)
	assert.NotContains(string(projected.StoredBody), "Mapped Stale")
	assert.Contains(string(projected.StoredBody), "Residue Survives")
	assert.Empty(projected.NativeMappings,
		"the surviving imported FN makes a derived one redundant")
	assert.NotContains(string(projected.StoredBody), "DERIVED")
	require.Len(projected.Residue, 1)
	assert.Equal("Residue Survives", projected.Residue[0].Property.RawValue)
}

func TestProjectPersonEnvelopeMovesOccurrenceToNewOwnerWithSameIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n" +
		"FN;PROP-ID=n1;X-KEEP=opaque:Old Row\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	occurrence := projectOccurrence(t, envelope, "FN", 0)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: occurrence.Identity, SourceRef: envelope.SourceRef,
		Table: "person_names", RowID: 10, Field: "formatted",
		Kind: vcard.HandlingNative,
	}}
	propID := "n1"
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Names: []store.PersonName{{
			Envelope: store.ValueEnvelope{
				ID: 11, Source: store.ProvenanceUser,
				VCard: store.VCardIdentity{Property: "FN", PropID: &propID},
			},
			NameKind: store.PersonNameFormatted, Formatted: new("New Row"),
		}},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err, "a replaced row that keeps its vCard identity must take over the occurrence")
	moved := projectOccurrenceByIdentity(t, projected, occurrence.Identity)
	assert.Equal("New Row", moved.Property.RawValue)
	require.Len(moved.Property.ParametersNamed("X-KEEP"), 1)
	assert.NotContains(string(projected.StoredBody), "Old Row")
	require.Len(projected.NativeMappings, 1)
	assert.Equal(int64(11), projected.NativeMappings[0].RowID)
	assert.True(occurrence.Identity.Equal(projected.NativeMappings[0].Identity))
	assert.Empty(projected.Residue)
}

func TestProjectPersonEnvelopeDropsClearedNameParametersAndKeepsVendorResidue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n" +
		"N;LANGUAGE=de;SCRIPT=Latn;SORT-AS=Beispiel,Alice;X-VENDOR=keep:" +
		"Beispiel;Alice;;;\r\nFN:Alice Beispiel\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	structured := projectOccurrence(t, envelope, "N", 0)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: structured.Identity, SourceRef: envelope.SourceRef,
		Table: "person_names", RowID: 51, Field: "structured",
		Kind: vcard.HandlingNative,
	}}
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1},
		Names: []store.PersonName{{
			Envelope:   store.ValueEnvelope{ID: 51, Source: store.ProvenanceUser},
			NameKind:   store.PersonNameStructured,
			FamilyName: new("Example"), GivenName: new("Alice"),
		}},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	name := mappedOccurrence(t, projected, "person_names", 51, "structured")
	assert.Equal("Example;Alice;;;;;", name.Property.RawValue)
	assert.Empty(projectedParameterValues(name.Property, "LANGUAGE"))
	assert.Empty(projectedParameterValues(name.Property, "SCRIPT"))
	assert.Empty(projectedParameterValues(name.Property, "SORT-AS"))
	assert.Equal([]string{"keep"}, projectedParameterValues(name.Property, "X-VENDOR"))
	assert.NotContains(string(projected.StoredBody), "SORT-AS=")
	assert.NotContains(string(projected.StoredBody), "LANGUAGE=")
	assert.NotContains(string(projected.StoredBody), "SCRIPT=")
	assert.Contains(string(projected.StoredBody), "X-VENDOR=keep")
}

func TestProjectPersonEnvelopeDropsClearedAddressAndDateParameters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\n" +
		"ADR;LABEL=Old Label;CC=DE;X-ADR-VENDOR=keep:;;1 Old St;Town;;;\r\n" +
		"BDAY;VALUE=date;CALSCALE=hebrew;X-BDAY-VENDOR=keep:1985-04-12\r\n" +
		"END:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	address := projectOccurrence(t, envelope, "ADR", 0)
	birthday := projectOccurrence(t, envelope, "BDAY", 0)
	envelope.NativeMappings = []vcard.NativeMapping{
		{
			Identity: address.Identity, SourceRef: envelope.SourceRef,
			Table: "person_addresses", RowID: 61, Field: "value",
			Kind: vcard.HandlingNative,
		},
		{
			Identity: birthday.Identity, SourceRef: envelope.SourceRef,
			Table: "person_dates", RowID: 71, Field: "date",
			Kind: vcard.HandlingNative,
		},
	}
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1},
		Names: []store.PersonName{{
			Envelope: store.ValueEnvelope{ID: 52, Source: store.ProvenanceUser,
				VCard: store.VCardIdentity{Property: "FN"}},
			NameKind: store.PersonNameFormatted, Formatted: new("Alice Example"),
		}},
		Addresses: []store.PersonAddress{{
			Envelope:      store.ValueEnvelope{ID: 61, Source: store.ProvenanceUser},
			AddressKind:   store.PersonAddressPostal,
			StreetAddress: new("1 Old St"), Locality: new("Town"),
		}},
		Dates: []store.PersonDate{{
			Envelope: store.ValueEnvelope{ID: 71, Source: store.ProvenanceUser},
			DateKind: store.PersonDateBirthday,
			Date:     store.PartialDate{Year: new(1985), Month: new(4), Day: new(12)},
		}},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	postal := mappedOccurrence(t, projected, "person_addresses", 61, "value")
	assert.Empty(projectedParameterValues(postal.Property, "LABEL"))
	assert.Empty(projectedParameterValues(postal.Property, "CC"))
	assert.Equal([]string{"keep"}, projectedParameterValues(postal.Property, "X-ADR-VENDOR"))
	date := mappedOccurrence(t, projected, "person_dates", 71, "date")
	assert.Empty(projectedParameterValues(date.Property, "CALSCALE"))
	assert.Equal([]string{"date"}, projectedParameterValues(date.Property, "VALUE"))
	assert.Equal([]string{"keep"}, projectedParameterValues(date.Property, "X-BDAY-VENDOR"))
	assert.NotContains(string(projected.StoredBody), "LABEL=")
	assert.NotContains(string(projected.StoredBody), "CC=")
	assert.NotContains(string(projected.StoredBody), "CALSCALE=")
	assert.Contains(string(projected.StoredBody), "X-ADR-VENDOR=keep")
	assert.Contains(string(projected.StoredBody), "X-BDAY-VENDOR=keep")
}

func TestProjectPersonEnvelopeMapsPortableScalarsAndLeavesUnsupportedValuesAsResidue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"X-STRUCTURED;X-KEEP=yes:opaque\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	structured := projectOccurrence(t, envelope, "X-STRUCTURED", 0)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: structured.Identity, SourceRef: envelope.SourceRef,
		Table: "person_attribute_values", RowID: 22, Field: "value",
		Kind: vcard.HandlingNative,
	}}
	text := "ambient"
	integer := int64(42)
	displayName := "Alice"
	snapshot := store.PersonVCardSnapshot{
		Profile: store.PersonProfile{Person: store.Person{ID: 1, DisplayName: &displayName}},
		Attributes: []store.PersonVCardAttribute{
			{
				Definition: store.AttributeDefinition{
					ID: 1, Slug: "genre", VCardProperty: new("X-GENRE"),
				},
				Values: []store.PersonAttributeValue{{
					ID: 21, DefinitionID: 1,
					Value: store.AttributeValue{Type: store.AttributeValueText, Text: &text},
				}},
			},
			{
				Definition: store.AttributeDefinition{
					ID: 2, Slug: "score", VCardProperty: new("X-SCORE"),
				},
				Values: []store.PersonAttributeValue{{
					ID: 23, DefinitionID: 2,
					Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &integer},
				}},
			},
			{
				Definition: store.AttributeDefinition{
					ID: 3, Slug: "structured", VCardProperty: new("X-STRUCTURED"),
				},
				Values: []store.PersonAttributeValue{{
					ID: 22, DefinitionID: 3,
					Value: store.AttributeValue{
						Type: store.AttributeValueJSON, JSON: []byte(`{"keep":true}`),
					},
				}},
			},
		},
	}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	assert.Contains(string(projected.StoredBody), "X-GENRE:ambient")
	assert.Contains(string(projected.StoredBody), "X-SCORE;VALUE=integer:42")
	assert.Contains(string(projected.StoredBody), "X-STRUCTURED;X-KEEP=yes:opaque")
	assert.Len(projected.NativeMappings, 3)
	assert.Contains(residueValues(projected.Residue), "opaque")
	assert.NotContains(residueValues(projected.Residue), "ambient")
}

func TestProjectPersonPropertiesMapsExtendedNamesAddressesAndPlaces(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	structuredID, phoneticID := int64(31), int64(32)
	structured := store.PersonName{
		Envelope:   store.ValueEnvelope{ID: structuredID, Source: store.ProvenanceUser},
		NameKind:   store.PersonNameStructured,
		FamilyName: new("Example"), GivenName: new("Alice"),
		AdditionalNames: new("Q"), HonorificPrefixes: new("Dr."),
		HonorificSuffixes: new("PhD"), SecondarySurname: new("Sample"),
		Generation: new("Jr."), Language: new("en"), Script: new("Latn"),
		SortAs: new("Example,Alice"),
	}
	altID := "phonetic-pair"
	phonetic := store.PersonName{
		Envelope: store.ValueEnvelope{ID: phoneticID, Source: store.ProvenanceUser,
			VCard: store.VCardIdentity{Property: "N", AltID: &altID}},
		NameKind: store.PersonNamePhonetic, FamilyName: new("ɪɡˈzæmpəl"),
		GivenName: new("ˈælɪs"), Language: new("en"),
		PhoneticSystem: new("ipa"), PhoneticScript: new("Latn"),
	}
	postal := store.PersonAddress{
		Envelope: store.ValueEnvelope{ID: 41, Source: store.ProvenanceUser,
			TypeTokens: []string{"home"}},
		AddressKind:        store.PersonAddressPostal,
		ExtendedComponents: new("PO Box 1;Suite 2;1 Example St;Exampletown;CA;90210;US;Room 5;Floor 3"),
		Label:              new("Home"), GeoURI: new("geo:34,-118"),
		Timezone: new("America/Los_Angeles"), CountryCode: new("US"),
	}
	birth := store.PersonAddress{
		Envelope:    store.ValueEnvelope{ID: 42, Source: store.ProvenanceUser},
		AddressKind: store.PersonAddressBirthPlace, PlaceURI: new("geo:40,-73"),
	}
	death := store.PersonAddress{
		Envelope:    store.ValueEnvelope{ID: 43, Source: store.ProvenanceUser},
		AddressKind: store.PersonAddressDeathPlace, FreeText: new("Example City"),
	}
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person:    store.Person{ID: 1, DisplayName: new("fallback")},
		Names:     []store.PersonName{structured, phonetic},
		Addresses: []store.PersonAddress{postal, birth, death},
	}}

	properties, _, err := projectPersonProperties(snapshot)
	require.NoError(err)
	structuredProperty := projectedByOwner(t, properties, "person_names", structuredID, "structured")
	assert.Equal("Example;Alice;Q;Dr.;PhD;Sample;Jr.", structuredProperty.Property.RawValue)
	assert.Equal([]string{"en"}, projectedParameterValues(structuredProperty.Property, "LANGUAGE"))
	assert.Equal([]string{"Latn"}, projectedParameterValues(structuredProperty.Property, "SCRIPT"))
	assert.Equal([]string{"Example", "Alice"}, projectedParameterValues(structuredProperty.Property, "SORT-AS"))
	phoneticProperty := projectedByOwner(t, properties, "person_names", phoneticID, "phonetic")
	assert.Equal([]string{"ipa"}, projectedParameterValues(phoneticProperty.Property, "PHONETIC"))
	assert.Equal([]string{altID}, projectedParameterValues(phoneticProperty.Property, "ALTID"))
	derived := projectedByOwner(t, properties, "person_names", structuredID, "derived_fn")
	assert.Equal("Dr. Alice Q Example Sample Jr. PhD", derived.Property.RawValue)
	assert.Equal([]string{"true"}, projectedParameterValues(derived.Property, "DERIVED"))
	postalProperty := projectedByOwner(t, properties, "person_addresses", 41, "value")
	components, err := vcard.SplitStructuredText(postalProperty.Property.RawValue)
	require.NoError(err)
	assert.Len(components, 9)
	assert.Equal("Room 5", components[7])
	assert.Equal([]string{"Home"}, projectedParameterValues(postalProperty.Property, "LABEL"))
	assert.Equal([]string{"US"}, projectedParameterValues(postalProperty.Property, "CC"))
	birthProperty := projectedByOwner(t, properties, "person_addresses", 42, "value")
	assert.Equal("geo:40,-73", birthProperty.Property.RawValue)
	assert.Equal([]string{"uri"}, projectedParameterValues(birthProperty.Property, "VALUE"))
	deathProperty := projectedByOwner(t, properties, "person_addresses", 43, "value")
	assert.Equal("Example City", deathProperty.Property.RawValue)
	assert.Equal([]string{"text"}, projectedParameterValues(deathProperty.Property, "VALUE"))
}

func TestProjectPersonPropertiesMapsEveryContactDateCategoryAndMediaKind(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	formatted := store.PersonName{
		Envelope: store.ValueEnvelope{ID: 1, Source: store.ProvenanceUser},
		NameKind: store.PersonNameFormatted, Formatted: new("Alice"),
	}
	contactCases := []struct {
		kind store.ContactAddressKind
		uri  string
		want string
	}{
		{store.ContactAddressEmail, "alice@example.com", "EMAIL"},
		{store.ContactAddressPhone, "+12025550123", "TEL"},
		{store.ContactAddressUsername, "im:alice", "IMPP"},
		{store.ContactAddressIMPP, "xmpp:alice@example.com", "IMPP"},
		{store.ContactAddressURL, "https://example.com", "URL"},
		{store.ContactAddressSocial, "https://social.example/alice", "SOCIALPROFILE"},
		{store.ContactAddressCalendar, "https://example.com/calendar", "CALURI"},
		{store.ContactAddressContactURI, "https://example.com/contact", "CONTACT-URI"},
		{store.ContactAddressOrgDirectory, "https://example.com/directory", "ORG-DIRECTORY"},
		{store.ContactAddressLanguage, "en-US", "LANG"},
	}
	points := make([]store.PersonContactPoint, 0, len(contactCases))
	for index, test := range contactCases {
		point := store.PersonContactPoint{
			Envelope: store.ValueEnvelope{ID: int64(100 + index), Source: store.ProvenanceUser,
				Pref: new(index + 1), TypeTokens: []string{"home"}},
			AddressKind: test.kind, OriginalValue: test.uri,
			NormalizedValue: test.uri,
		}
		if test.kind != store.ContactAddressEmail && test.kind != store.ContactAddressLanguage &&
			test.kind != store.ContactAddressPhone {
			point.URI = &test.uri
		}
		points = append(points, point)
	}
	media := []store.PersonMedia{
		{Envelope: store.ValueEnvelope{ID: 201, Source: store.ProvenanceUser}, MediaKind: store.PersonMediaPhoto, MediaType: new("image/png"), HasData: true},
		{Envelope: store.ValueEnvelope{ID: 202, Source: store.ProvenanceUser}, MediaKind: store.PersonMediaLogo, URI: new("https://example.com/logo.png")},
		{Envelope: store.ValueEnvelope{ID: 203, Source: store.ProvenanceUser}, MediaKind: store.PersonMediaSound, URI: new("https://example.com/sound.ogg")},
		{Envelope: store.ValueEnvelope{ID: 204, Source: store.ProvenanceUser}, MediaKind: store.PersonMediaKey, URI: new("https://example.com/key.asc")},
	}
	snapshot := store.PersonVCardSnapshot{
		Profile: store.PersonProfile{
			Person: store.Person{ID: 1}, Names: []store.PersonName{formatted},
			ContactPoints: points,
			Dates: []store.PersonDate{
				{Envelope: store.ValueEnvelope{ID: 301, Source: store.ProvenanceUser}, DateKind: store.PersonDateBirthday, Date: store.PartialDate{Year: new(1985), Month: new(4), Day: new(12)}},
				{Envelope: store.ValueEnvelope{ID: 302, Source: store.ProvenanceUser}, DateKind: store.PersonDateAnniversary, DateText: new("spring")},
				{Envelope: store.ValueEnvelope{ID: 303, Source: store.ProvenanceUser}, DateKind: store.PersonDateDeath, Date: store.PartialDate{Year: new(2050)}},
			},
			Categories: []store.PersonCategory{{Envelope: store.ValueEnvelope{ID: 401, Source: store.ProvenanceUser}, OriginalValue: "Friends"}},
			Media:      media,
		},
		MediaData: []store.PersonVCardMediaData{{MediaID: 201, MediaType: "image/png", Data: []byte("png")}},
	}

	properties, _, err := projectPersonProperties(snapshot)
	require.NoError(err)
	for index, test := range contactCases {
		projected := projectedByOwner(t, properties, "person_contact_points", int64(100+index), "original_value")
		assert.Equal(test.want, projected.Property.Name)
		assert.Equal([]string{"home"}, projectedParameterValues(projected.Property, "TYPE"))
		assert.Equal([]string{strconv.Itoa(index + 1)}, projectedParameterValues(projected.Property, "PREF"))
	}
	assert.Equal("tel:+12025550123", projectedByOwner(t, properties, "person_contact_points", 101, "original_value").Property.RawValue)
	assert.Equal("19850412", projectedByOwner(t, properties, "person_dates", 301, "date").Property.RawValue)
	assert.Equal([]string{"text"}, projectedParameterValues(projectedByOwner(t, properties, "person_dates", 302, "date").Property, "VALUE"))
	assert.Equal("CATEGORIES", projectedByOwner(t, properties, "person_categories", 401, "value").Property.Name)
	assert.Contains(projectedByOwner(t, properties, "person_media", 201, "value").Property.RawValue, "data:image/png;base64,")
	for index, want := range []string{"PHOTO", "LOGO", "SOUND", "KEY"} {
		assert.Equal(want, projectedByOwner(t, properties, "person_media", int64(201+index), "value").Property.Name)
	}
}

func TestProjectPersonEnvelopeRequiresAbsoluteURIsForURIContactProperties(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"URL:example.com\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	imported := projectOccurrence(t, envelope, "URL", 0)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: imported.Identity, SourceRef: envelope.SourceRef,
		Table: "person_contact_points", RowID: 100, Field: "original_value",
		Kind: vcard.HandlingNative,
	}}
	service := "mastodon"
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1, DisplayName: new("Alice")},
		ContactPoints: []store.PersonContactPoint{
			{
				Envelope:    store.ValueEnvelope{ID: 100, Source: store.ProvenanceUser},
				AddressKind: store.ContactAddressURL, OriginalValue: "example.com",
			},
			{
				Envelope:    store.ValueEnvelope{ID: 101, Source: store.ProvenanceUser},
				AddressKind: store.ContactAddressCalendar, OriginalValue: "not a uri",
			},
			{
				Envelope:    store.ValueEnvelope{ID: 102, Source: store.ProvenanceUser},
				AddressKind: store.ContactAddressSocial, OriginalValue: "@alice",
				ServiceSlug: &service,
			},
			{
				Envelope:    store.ValueEnvelope{ID: 103, Source: store.ProvenanceUser},
				AddressKind: store.ContactAddressSocial, OriginalValue: "https://social.example/@alice",
			},
		},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	body := string(projected.StoredBody)
	assert.Contains(body, "URL:example.com\r\n",
		"an unrenderable value keeps the imported occurrence as residue")
	assert.NotContains(body, "CALURI")
	assert.Contains(body, "SOCIALPROFILE;SERVICE-TYPE=mastodon;VALUE=text:@alice\r\n")
	assert.Contains(body, "SOCIALPROFILE:https://social.example/@alice\r\n")
	kept := mappingByOwner(t, projected.NativeMappings, "person_contact_points", 100, "original_value")
	assert.Equal(vcard.HandlingPreserve, kept.Kind)
	residue := make([]string, 0, len(projected.Residue))
	for _, occurrence := range projected.Residue {
		residue = append(residue, occurrence.Property.Name+":"+occurrence.Property.RawValue)
	}
	assert.Contains(residue, "URL:example.com")

	// Once the handle becomes a URI the projection drops the text reset it
	// owns, but leaves the unowned SERVICE-TYPE in place.
	snapshot.Profile.ContactPoints[2].OriginalValue = "https://mastodon.example/@alice"
	reprojected, err := ProjectPersonEnvelope(snapshot, projected)
	require.NoError(err)
	assert.Contains(string(reprojected.StoredBody),
		"SOCIALPROFILE;SERVICE-TYPE=mastodon:https://mastodon.example/@alice\r\n")
	assert.NotContains(string(reprojected.StoredBody), "VALUE=text")
}

func TestProjectPersonEnvelopeContactPointsReplaceStaleValueParameters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"TEL;VALUE=text:+1 202 555 0100\r\n" +
		"EMAIL;VALUE=uri:mailto:old@example.com\r\n" +
		"URL;VALUE=text:https://old.example/alice\r\n" +
		"item1.EMAIL;VALUE=text:old-alias@example.com\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	owners := []struct {
		name  string
		index int
		rowID int64
	}{
		{name: "TEL", rowID: 100},
		{name: "EMAIL", rowID: 101},
		{name: "URL", rowID: 102},
		{name: "EMAIL", index: 1, rowID: 103},
	}
	for _, owner := range owners {
		occurrence := projectOccurrence(t, envelope, owner.name, owner.index)
		envelope.NativeMappings = append(envelope.NativeMappings, vcard.NativeMapping{
			Identity: occurrence.Identity, SourceRef: envelope.SourceRef,
			Table: "person_contact_points", RowID: owner.rowID,
			Field: "original_value", Kind: vcard.HandlingNative,
		})
	}
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1, DisplayName: new("Alice")},
		ContactPoints: []store.PersonContactPoint{
			{
				Envelope:    store.ValueEnvelope{ID: 100, Source: store.ProvenanceUser},
				AddressKind: store.ContactAddressPhone, OriginalValue: "+1 202 555 0100",
				NormalizedValue: "+12025550100",
			},
			{
				Envelope:    store.ValueEnvelope{ID: 101, Source: store.ProvenanceUser},
				AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.com",
			},
			{
				Envelope:    store.ValueEnvelope{ID: 102, Source: store.ProvenanceUser},
				AddressKind: store.ContactAddressURL, OriginalValue: "https://new.example/alice",
			},
			{
				Envelope:    store.ValueEnvelope{ID: 103, Source: store.ProvenanceUser},
				AddressKind: store.ContactAddressEmail, OriginalValue: "alias@example.com",
				URI: new("mailto:alias@example.com"),
			},
		},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	body := string(projected.StoredBody)
	assert.Contains(body, "TEL:tel:+12025550100\r\n")
	assert.Contains(body, "EMAIL:alice@example.com\r\n")
	assert.Contains(body, "URL:https://new.example/alice\r\n")
	assert.Contains(body, "item1.EMAIL:mailto:alias@example.com\r\n",
		"EMAIL is text-only (RFC 6350 section 6.4.2) and never carries VALUE=uri")
	assert.NotContains(body, "TEL;VALUE=text")
	assert.NotContains(body, "VALUE=uri")
	assert.NotContains(body, "URL;VALUE=text")
}

func TestProjectPersonPropertiesFallBackWhenURIFieldsAreNotURIs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1, DisplayName: new("Alice")},
		ContactPoints: []store.PersonContactPoint{{
			Envelope:    store.ValueEnvelope{ID: 100, Source: store.ProvenanceUser},
			AddressKind: store.ContactAddressPhone, OriginalValue: "(202) 555-0123",
			NormalizedValue: "+12025550123", URI: new("12:30"),
		}},
		Addresses: []store.PersonAddress{
			{
				Envelope:    store.ValueEnvelope{ID: 200, Source: store.ProvenanceUser},
				AddressKind: store.PersonAddressBirthPlace, OriginalValue: "Springfield",
				PlaceURI: new("Springfield, USA"), FreeText: new("Springfield"),
			},
			{
				Envelope:    store.ValueEnvelope{ID: 201, Source: store.ProvenanceUser},
				AddressKind: store.PersonAddressDeathPlace, OriginalValue: "unused",
				PlaceURI: new("Somewhere; unknown"),
			},
			{
				Envelope:    store.ValueEnvelope{ID: 202, Source: store.ProvenanceUser},
				AddressKind: store.PersonAddressBirthPlace, OriginalValue: "unused",
				PlaceURI: new("geo:41.8781,-87.6298"),
			},
		},
		Media: []store.PersonMedia{
			{
				Envelope:  store.ValueEnvelope{ID: 300, Source: store.ProvenanceUser},
				MediaKind: store.PersonMediaPhoto, URI: new("photos/alice.jpg"),
			},
			{
				Envelope:  store.ValueEnvelope{ID: 301, Source: store.ProvenanceUser},
				MediaKind: store.PersonMediaLogo, URI: new("https://example.com/logo.png"),
			},
		},
	}}

	properties, retained, err := projectPersonProperties(snapshot)
	require.NoError(err)
	assert.Equal("tel:+12025550123",
		projectedByOwner(t, properties, "person_contact_points", 100, "original_value").Property.RawValue,
		"a TEL override that is not a URI falls back to the normalized number")
	birthplace := projectedByOwner(t, properties, "person_addresses", 200, fieldValue)
	assert.Equal("Springfield", birthplace.Property.RawValue)
	assert.Equal([]string{"text"}, projectedParameterValues(birthplace.Property, "VALUE"))
	deathplace := projectedByOwner(t, properties, "person_addresses", 201, fieldValue)
	assert.Equal("Somewhere\\; unknown", deathplace.Property.RawValue,
		"a non-URI place with no text falls back to the string itself, escaped")
	assert.Equal([]string{"text"}, projectedParameterValues(deathplace.Property, "VALUE"))
	geo := projectedByOwner(t, properties, "person_addresses", 202, fieldValue)
	assert.Equal("geo:41.8781,-87.6298", geo.Property.RawValue)
	assert.Equal([]string{"uri"}, projectedParameterValues(geo.Property, "VALUE"))
	for _, property := range properties {
		assert.NotEqual("PHOTO", property.Property.Name, "a relative media reference must not render")
	}
	assert.Contains(retained,
		retainedOwner{Owner: projectedOwner{Table: "person_media", RowID: 300, Field: fieldValue}})
	assert.Equal("https://example.com/logo.png",
		projectedByOwner(t, properties, "person_media", 301, fieldValue).Property.RawValue)
}

func TestProjectPersonEnvelopeOwnsRelatedValueType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"RELATED;VALUE=text;TYPE=friend;X-KEEP=yes:Old Friend\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	occurrence := projectOccurrence(t, envelope, "RELATED", 0)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: occurrence.Identity, SourceRef: envelope.SourceRef,
		Table: "person_relationships", RowID: 701, Field: "related",
		Kind: vcard.HandlingNative,
	}}
	friend := int64(1)
	snapshot := store.PersonVCardSnapshot{
		Profile: store.PersonProfile{Person: store.Person{ID: 1, DisplayName: new("Alice")}},
		Relationships: []store.PersonRelationshipView{
			{
				Relationship:        store.PersonRelationship{ID: 701, RelationshipTypeID: friend},
				Direction:           store.RelationshipDirectionOutgoing,
				CounterpartVCardUID: "bba20e70-e528-4dcf-ae0c-a4fd3e71fe20",
			},
			{
				Relationship:        store.PersonRelationship{ID: 702, RelationshipTypeID: friend},
				Direction:           store.RelationshipDirectionOutgoing,
				CounterpartVCardUID: "legacy-uid-42",
			},
		},
		RelationshipTypes: []store.RelationshipType{
			{ID: friend, Slug: "friend", IsSymmetric: true, VCardRelatedType: new("friend")},
		},
	}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	moved := projectOccurrenceByIdentity(t, projected, occurrence.Identity)
	assert.Equal("urn:uuid:bba20e70-e528-4dcf-ae0c-a4fd3e71fe20", moved.Property.RawValue)
	assert.Empty(projectedParameterValues(moved.Property, "VALUE"),
		"the imported VALUE=text must not survive onto a urn:uuid: value")
	assert.Equal([]string{"friend"}, projectedParameterValues(moved.Property, "TYPE"))
	assert.Equal([]string{"yes"}, projectedParameterValues(moved.Property, "X-KEEP"))
	assert.Contains(string(projected.StoredBody), "RELATED;TYPE=friend;VALUE=text:legacy-uid-42\r\n")
}

func TestProjectPersonPropertiesSkipsReservedAttributeTargets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	text := "3.0"
	attributes := make([]store.PersonVCardAttribute, 0, 4)
	for index, name := range []string{"VERSION", "BEGIN", "END", "UID"} {
		attributes = append(attributes, store.PersonVCardAttribute{
			Definition: store.AttributeDefinition{
				ID: int64(index + 1), Slug: "reserved", VCardProperty: new(name),
			},
			Values: []store.PersonAttributeValue{{
				ID: int64(900 + index), DefinitionID: int64(index + 1),
				Value: store.AttributeValue{Type: store.AttributeValueText, Text: &text},
			}},
		})
	}
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1, DisplayName: new("Alice")},
	}, Attributes: attributes}
	envelope := parseProjectEnvelope(t, []byte(
		"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n"+
			"UID:urn:uuid:bba20e70-e528-4dcf-ae0c-a4fd3e71fe20\r\nEND:VCARD\r\n"))

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err, "a reserved attribute target must not break the whole projection")
	body := string(projected.StoredBody)
	assert.Equal(1, strings.Count(body, "VERSION:"))
	assert.Equal(1, strings.Count(body, "BEGIN:"))
	assert.Equal(1, strings.Count(body, "END:"))
	assert.Equal(1, strings.Count(body, "UID:"))
	assert.NotContains(body, "3.0")
}

func TestProjectPersonEnvelopeKeepsImportedFNWhenProfileHasNoName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n"+
		"FN:Imported Name\r\nEND:VCARD\r\n"))
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1},
		ContactPoints: []store.PersonContactPoint{{
			Envelope:    store.ValueEnvelope{ID: 5, Source: store.ProvenanceUser},
			AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.com",
		}},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err, "a profile with nothing to derive FN from must not block other semantic writes")
	assert.Contains(string(projected.StoredBody), "FN:Imported Name\r\n")
	assert.Contains(string(projected.StoredBody), "EMAIL:alice@example.com\r\n")
	assert.NotContains(string(projected.StoredBody), "DERIVED")

	// A card that has no FN anywhere is still refused, by rendering.
	bare := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n"+
		"NOTE:no name here\r\nEND:VCARD\r\n"))
	_, err = ProjectPersonEnvelope(snapshot, bare)
	require.Error(err)
}

func TestProjectPersonPropertiesCarriesUnstructuredPostalAddresses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1, DisplayName: new("Alice")},
		Addresses: []store.PersonAddress{
			{
				Envelope:    store.ValueEnvelope{ID: 700, Source: store.ProvenanceUser},
				AddressKind: store.PersonAddressPostal, OriginalValue: "1 Main St, Town",
			},
			{
				Envelope:    store.ValueEnvelope{ID: 701, Source: store.ProvenanceUser},
				AddressKind: store.PersonAddressPostal, OriginalValue: ";;1 Main\\, St;Town;;;",
			},
			{
				Envelope:    store.ValueEnvelope{ID: 702, Source: store.ProvenanceUser},
				AddressKind: store.PersonAddressPostal, OriginalValue: "ignored",
				FreeText: new("Second Floor, 2 Side St"),
			},
			{
				Envelope:    store.ValueEnvelope{ID: 703, Source: store.ProvenanceUser},
				AddressKind: store.PersonAddressPostal, StreetAddress: new("3 Real St"),
				OriginalValue: "not used when components exist",
			},
			{
				Envelope:    store.ValueEnvelope{ID: 704, Source: store.ProvenanceUser},
				AddressKind: store.PersonAddressPostal,
			},
		},
	}}

	properties, retained, err := projectPersonProperties(snapshot)
	require.NoError(err)
	freeText := projectedByOwner(t, properties, "person_addresses", 700, fieldValue)
	assert.Equal(";;;;;;", freeText.Property.RawValue)
	assert.Equal([]string{"1 Main St, Town"}, projectedParameterValues(freeText.Property, "LABEL"))
	structured := projectedByOwner(t, properties, "person_addresses", 701, fieldValue)
	assert.Equal(";;1 Main\\, St;Town;;;", structured.Property.RawValue,
		"a structured original value is re-emitted with its escaping intact, not escaped again")
	assert.Empty(projectedParameterValues(structured.Property, "LABEL"))
	fromFreeText := projectedByOwner(t, properties, "person_addresses", 702, fieldValue)
	assert.Equal([]string{"Second Floor, 2 Side St"}, projectedParameterValues(fromFreeText.Property, "LABEL"))
	components := projectedByOwner(t, properties, "person_addresses", 703, fieldValue)
	assert.Equal(";;3 Real St;;;;", components.Property.RawValue)
	for _, property := range properties {
		assert.False(property.Owner.Table == "person_addresses" && property.Owner.RowID == 704,
			"a postal row with nothing to render is not projected")
	}
	assert.Contains(retained,
		retainedOwner{Owner: projectedOwner{Table: "person_addresses", RowID: 704, Field: fieldValue}})
}

func TestProjectPersonEnvelopeHandsAcceptedReviewOccurrenceToEdgePerResource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"item1.RELATED;TYPE=friend;X-KEEP=yes:Bob from the gym\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	occurrence := projectOccurrence(t, envelope, "RELATED", 0)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: occurrence.Identity, SourceRef: envelope.SourceRef,
		Table: "person_relationship_reviews", RowID: 55, Field: "related",
		Kind: vcard.HandlingPreserve,
	}}
	// The edge carries an identity that fits some other card, not this one:
	// the envelope's own mapping, not that identity, must decide ownership.
	otherGroup := "item9"
	friend := int64(1)
	snapshot := store.PersonVCardSnapshot{
		Profile: store.PersonProfile{Person: store.Person{ID: 1, DisplayName: new("Alice")}},
		Relationships: []store.PersonRelationshipView{{
			Relationship: store.PersonRelationship{
				ID: 701, RelationshipTypeID: friend,
				VCardIdentity: store.VCardIdentity{Property: "RELATED", Group: &otherGroup},
			},
			Direction:           store.RelationshipDirectionOutgoing,
			CounterpartVCardUID: "bba20e70-e528-4dcf-ae0c-a4fd3e71fe20",
		}},
		RelationshipTypes: []store.RelationshipType{
			{ID: friend, Slug: "friend", IsSymmetric: true, VCardRelatedType: new("friend")},
		},
		AcceptedRelationshipReviews: []store.PersonVCardAcceptedReview{{ReviewID: 55, RelationshipID: 701}},
	}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	assert.Equal("person_relationship_reviews", envelope.NativeMappings[0].Table,
		"projection must not mutate its input")
	moved := projectOccurrenceByIdentity(t, projected, occurrence.Identity)
	assert.Equal("urn:uuid:bba20e70-e528-4dcf-ae0c-a4fd3e71fe20", moved.Property.RawValue)
	assert.Equal("item1", moved.Property.Group)
	assert.Equal([]string{"yes"}, projectedParameterValues(moved.Property, "X-KEEP"))
	assert.Equal(1, strings.Count(string(projected.StoredBody), "RELATED"))
	mapping := mappingByOwner(t, projected.NativeMappings, "person_relationships", 701, "related")
	assert.True(occurrence.Identity.Equal(mapping.Identity))
	assert.Equal(vcard.HandlingRelationship, mapping.Kind)
	require.Len(projected.NativeMappings, 1, "the edge; the imported FN needs no derived one")
	for _, occurrence := range projected.Residue {
		assert.NotEqual("RELATED", occurrence.Property.Name, "the review's occurrence must not fall to residue")
	}
}

func TestProjectPersonEnvelopeScopesIdentityFallbackToTheOriginatingResource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"item1.EMAIL;X-KEEP=yes:other@example.com\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	require.Equal("book", envelope.SourceRef)
	group := "item1"
	otherBook := "other-book"
	sameBook := "book"
	sameResourceUID := "resource"
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1, DisplayName: new("Alice")},
		ContactPoints: []store.PersonContactPoint{
			{
				// Read from another resource: its item1.EMAIL is that card's,
				// not this one's, however alike the identities look.
				Envelope: store.ValueEnvelope{ID: 100, Source: store.ProvenanceVCardImport,
					SourceRef: &otherBook,
					VCard:     store.VCardIdentity{Property: "EMAIL", Group: &group}},
				AddressKind: store.ContactAddressEmail, OriginalValue: "foreign@example.com",
			},
		},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	body := string(projected.StoredBody)
	assert.Contains(body, "item1.EMAIL;X-KEEP=yes:other@example.com\r\n",
		"an occurrence must not be claimed by a row from another resource")
	assert.Contains(body, "EMAIL:foreign@example.com\r\n")

	// The same identity read from this resource, or from no recorded
	// resource, may claim the occurrence.
	for name, provenance := range map[string]struct {
		sourceRef, sourceResourceUID *string
	}{
		"same resource": {&sameBook, &sameResourceUID},
		"unscoped":      {nil, nil},
	} {
		snapshot.Profile.ContactPoints[0].Envelope.SourceRef = provenance.sourceRef
		snapshot.Profile.ContactPoints[0].Envelope.SourceResourceUID = provenance.sourceResourceUID
		claimed, err := ProjectPersonEnvelope(snapshot, envelope)
		require.NoError(err, name)
		assert.Contains(string(claimed.StoredBody), "item1.EMAIL;X-KEEP=yes:foreign@example.com\r\n", name)
	}
}

func TestProjectPersonEnvelopeScopesIdentityFallbackToSourceResourceUID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n"+
		"item1.EMAIL;X-KEEP=yes:other@example.com\r\nEND:VCARD\r\n"))
	require.Equal("book", envelope.SourceRef)
	require.Equal("resource", envelope.SourceResourceUID)

	book := "book"
	otherResourceUID := "another-card"
	group := "item1"
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1, DisplayName: new("Alice")},
		ContactPoints: []store.PersonContactPoint{{
			Envelope: store.ValueEnvelope{
				ID: 100, Source: store.ProvenanceVCardImport,
				SourceRef: &book, SourceResourceUID: &otherResourceUID,
				VCard: store.VCardIdentity{Property: "EMAIL", Group: &group},
			},
			AddressKind:   store.ContactAddressEmail,
			OriginalValue: "foreign@example.com",
		}},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	body := string(projected.StoredBody)
	assert.Contains(body, "item1.EMAIL;X-KEEP=yes:other@example.com\r\n",
		"an occurrence must not be claimed by another card in the same source")
	assert.Contains(body, "EMAIL:foreign@example.com\r\n")
}

func TestProjectPersonEnvelopeRetiresReviewOccurrenceWhenEdgeAlreadyOwnsOne(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"RELATED;TYPE=friend;X-EDGE=yes:urn:uuid:bba20e70-e528-4dcf-ae0c-a4fd3e71fe20\r\n" +
		"item1.RELATED;TYPE=friend;X-REVIEW=yes:Bob from the gym\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	edgeOccurrence := projectOccurrence(t, envelope, "RELATED", 0)
	reviewOccurrence := projectOccurrence(t, envelope, "RELATED", 1)
	envelope.NativeMappings = []vcard.NativeMapping{
		{
			Identity: edgeOccurrence.Identity, SourceRef: envelope.SourceRef,
			Table: "person_relationships", RowID: 701, Field: "related",
			Kind: vcard.HandlingNative,
		},
		{
			Identity: reviewOccurrence.Identity, SourceRef: envelope.SourceRef,
			Table: "person_relationship_reviews", RowID: 55, Field: "related",
			Kind: vcard.HandlingPreserve,
		},
	}
	friend := int64(1)
	snapshot := store.PersonVCardSnapshot{
		Profile: store.PersonProfile{Person: store.Person{ID: 1, DisplayName: new("Alice")}},
		Relationships: []store.PersonRelationshipView{{
			Relationship:        store.PersonRelationship{ID: 701, RelationshipTypeID: friend},
			Direction:           store.RelationshipDirectionOutgoing,
			CounterpartVCardUID: "bba20e70-e528-4dcf-ae0c-a4fd3e71fe20",
		}},
		RelationshipTypes: []store.RelationshipType{
			{ID: friend, Slug: "friend", IsSymmetric: true, VCardRelatedType: new("friend")},
		},
		AcceptedRelationshipReviews: []store.PersonVCardAcceptedReview{{ReviewID: 55, RelationshipID: 701}},
	}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	body := string(projected.StoredBody)
	assert.Equal(1, strings.Count(body, "RELATED"), "one edge, one RELATED")
	kept := projectOccurrenceByIdentity(t, projected, edgeOccurrence.Identity)
	assert.Equal([]string{"yes"}, projectedParameterValues(kept.Property, "X-EDGE"),
		"the edge keeps the occurrence it already owned")
	assert.NotContains(body, "X-REVIEW")
	owners := 0
	for _, mapping := range projected.NativeMappings {
		if mapping.Table == "person_relationships" && mapping.RowID == 701 {
			owners++
		}
	}
	assert.Equal(1, owners, "an edge owns at most one occurrence per resource")
	for _, occurrence := range projected.Residue {
		assert.NotEqual("RELATED", occurrence.Property.Name)
	}
}

func TestProjectPersonEnvelopeUsesReviewIdentityForUnmappedEdgeInItsResourceOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"item1.RELATED;TYPE=friend;X-KEEP=yes:Bob from the gym\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	require.Equal("book", envelope.SourceRef)
	occurrence := projectOccurrence(t, envelope, "RELATED", 0)
	group := "item1"
	friend := int64(1)
	snapshot := func(sourceRef, sourceResourceUID *string) store.PersonVCardSnapshot {
		return store.PersonVCardSnapshot{
			Profile: store.PersonProfile{Person: store.Person{ID: 1, DisplayName: new("Alice")}},
			Relationships: []store.PersonRelationshipView{{
				Relationship:        store.PersonRelationship{ID: 701, RelationshipTypeID: friend},
				Direction:           store.RelationshipDirectionOutgoing,
				CounterpartVCardUID: "bba20e70-e528-4dcf-ae0c-a4fd3e71fe20",
			}},
			RelationshipTypes: []store.RelationshipType{
				{ID: friend, Slug: "friend", IsSymmetric: true, VCardRelatedType: new("friend")},
			},
			AcceptedRelationshipReviews: []store.PersonVCardAcceptedReview{{
				ReviewID: 55, RelationshipID: 701, SourceRef: sourceRef,
				SourceResourceUID: sourceResourceUID,
				VCardIdentity:     store.VCardIdentity{Property: "RELATED", Group: &group},
			}},
		}
	}

	// No mapping exists yet (the card was never projected while the review
	// was pending): the review's identity, read from this resource, lets the
	// edge take the occurrence over in place.
	sameBook := "book"
	sameResourceUID := "resource"
	projected, err := ProjectPersonEnvelope(snapshot(&sameBook, &sameResourceUID), envelope)
	require.NoError(err)
	moved := projectOccurrenceByIdentity(t, projected, occurrence.Identity)
	assert.Equal("urn:uuid:bba20e70-e528-4dcf-ae0c-a4fd3e71fe20", moved.Property.RawValue)
	assert.Equal([]string{"yes"}, projectedParameterValues(moved.Property, "X-KEEP"))
	assert.Equal(1, strings.Count(string(projected.StoredBody), "RELATED"))

	// A review read from another card in the same source names that card's
	// occurrence, not this one's: the edge is appended and the imported line
	// stays as it was.
	otherResourceUID := "another-card"
	foreign, err := ProjectPersonEnvelope(snapshot(&sameBook, &otherResourceUID), envelope)
	require.NoError(err)
	untouched := projectOccurrenceByIdentity(t, foreign, occurrence.Identity)
	assert.Equal("Bob from the gym", untouched.Property.RawValue)
	assert.Equal([]string{"yes"}, projectedParameterValues(untouched.Property, "X-KEEP"))
	assert.Equal(2, strings.Count(string(foreign.StoredBody), "RELATED"))
}

func TestProjectPersonEnvelopeMapsPrimaryEmploymentAndDirectionalRelationships(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"ORG:Side Org\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	sideOccurrence := projectOccurrence(t, envelope, "ORG", 0)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: sideOccurrence.Identity, SourceRef: envelope.SourceRef,
		Table: "employments", RowID: 502, Field: "organization_id",
		Kind: vcard.HandlingNative,
	}}
	parentType, childType := int64(601), int64(602)
	counterpartUID := "bba20e70-e528-4dcf-ae0c-a4fd3e71fe20"
	snapshot := store.PersonVCardSnapshot{
		Profile: store.PersonProfile{Person: store.Person{ID: 1}, Names: []store.PersonName{{
			Envelope: store.ValueEnvelope{ID: 1, Source: store.ProvenanceUser},
			NameKind: store.PersonNameFormatted, Formatted: new("Alice"),
		}}},
		Employments: []store.PersonVCardEmployment{
			{Employment: store.Employment{ID: 501, IsCurrent: true, IsPrimary: true, Title: new("Engineer"), Role: new("Architect"), Department: new("Platform")}, Organization: store.OrganizationProfile{Organization: store.Organization{Name: "Primary Org"}}},
			{Employment: store.Employment{ID: 502, IsCurrent: true, Source: store.ProvenanceVCardImport}, Organization: store.OrganizationProfile{Organization: store.Organization{Name: "Side Org"}}},
		},
		Relationships: []store.PersonRelationshipView{{
			Relationship:        store.PersonRelationship{ID: 701, RelationshipTypeID: parentType},
			Direction:           store.RelationshipDirectionOutgoing,
			CounterpartVCardUID: counterpartUID,
		}},
		RelationshipTypes: []store.RelationshipType{
			{ID: parentType, Slug: "parent", IsCanonical: true, InverseTypeID: &childType, VCardRelatedType: new("parent")},
			{ID: childType, Slug: "child", InverseTypeID: &parentType, VCardRelatedType: new("child")},
		},
	}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	assert.Contains(string(projected.StoredBody), "ORG:Primary Org;Platform")
	assert.Contains(string(projected.StoredBody), "TITLE:Engineer")
	assert.Contains(string(projected.StoredBody), "ROLE:Architect")
	related := mappedOccurrence(t, projected, "person_relationships", 701, "related")
	assert.Equal("urn:uuid:"+counterpartUID, related.Property.RawValue)
	assert.Equal([]string{"child"}, projectedParameterValues(related.Property, "TYPE"))
	sideMapping := mappingByOwner(t, projected.NativeMappings, "employments", 502, "organization_id")
	assert.Equal(vcard.HandlingPreserve, sideMapping.Kind)
	assert.Contains(residueValues(projected.Residue), "Side Org")
}

func parseProjectEnvelope(t *testing.T, raw []byte) vcard.ResourceEnvelope {
	t.Helper()
	envelope, err := vcard.ParseResourceEnvelope(raw)
	require.NoError(t, err)
	envelope.SourceRef = "book"
	envelope.SourceResourceUID = "resource"
	return envelope
}

func projectOccurrence(
	t *testing.T, envelope vcard.ResourceEnvelope, name string, index int,
) vcard.PropertyOccurrence {
	t.Helper()
	seen := 0
	for _, occurrence := range envelope.PropertyTree {
		if occurrence.Property.Name != name {
			continue
		}
		if seen == index {
			return occurrence
		}
		seen++
	}
	require.FailNow(t, "property occurrence not found", "%s[%d]", name, index)
	return vcard.PropertyOccurrence{}
}

func projectOccurrenceByIdentity(
	t *testing.T, envelope vcard.ResourceEnvelope, identity vcard.PropertyIdentity,
) vcard.PropertyOccurrence {
	t.Helper()
	for _, occurrence := range envelope.PropertyTree {
		if occurrence.Identity.Equal(identity) {
			return occurrence
		}
	}
	require.FailNow(t, "property identity not found")
	return vcard.PropertyOccurrence{}
}

func secondIdentityGroup(identity vcard.PropertyIdentity) *string {
	if identity.Group == "" {
		return nil
	}
	return &identity.Group
}

func residueValues(residue []vcard.PropertyOccurrence) []string {
	values := make([]string, 0, len(residue))
	for _, occurrence := range residue {
		values = append(values, occurrence.Property.RawValue)
	}
	return values
}

func projectedByOwner(
	t *testing.T, properties []projectedProperty,
	table string, rowID int64, field string,
) projectedProperty {
	t.Helper()
	for _, property := range properties {
		if property.Owner == (projectedOwner{Table: table, RowID: rowID, Field: field}) {
			return property
		}
	}
	require.FailNow(t, "projected owner not found", "%s/%d/%s", table, rowID, field)
	return projectedProperty{}
}

func projectedParameterValues(property vcard.Property, name string) []string {
	values := make([]string, 0)
	for _, parameter := range property.ParametersNamed(name) {
		for _, value := range parameter.Values {
			values = append(values, value.Decoded)
		}
	}
	return values
}

func mappingByOwner(
	t *testing.T, mappings []vcard.NativeMapping,
	table string, rowID int64, field string,
) vcard.NativeMapping {
	t.Helper()
	for _, mapping := range mappings {
		if mapping.Table == table && mapping.RowID == rowID && mapping.Field == field {
			return mapping
		}
	}
	require.FailNow(t, "native mapping owner not found", "%s/%d/%s", table, rowID, field)
	return vcard.NativeMapping{}
}

func mappedOccurrence(
	t *testing.T, envelope vcard.ResourceEnvelope,
	table string, rowID int64, field string,
) vcard.PropertyOccurrence {
	t.Helper()
	mapping := mappingByOwner(t, envelope.NativeMappings, table, rowID, field)
	return projectOccurrenceByIdentity(t, envelope, mapping.Identity)
}
