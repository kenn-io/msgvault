package vcardmap

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

func TestProjectPersonEnvelopePersistedMappingWinsOverIdentityFallbackInEitherOrder(t *testing.T) {
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"EMAIL;PROP-ID=e1;X-KEEP=yes:mapped@example.com\r\nEND:VCARD\r\n")
	propID := "e1"
	mapped := store.PersonContactPoint{
		Envelope:    store.ValueEnvelope{ID: 10, Source: store.ProvenanceUser},
		AddressKind: store.ContactAddressEmail, OriginalValue: "ten@example.com",
	}
	byIdentity := store.PersonContactPoint{
		Envelope: store.ValueEnvelope{ID: 11, Source: store.ProvenanceUser,
			VCard: store.VCardIdentity{Property: "EMAIL", PropID: &propID}},
		AddressKind: store.ContactAddressEmail, OriginalValue: "eleven@example.com",
	}
	for name, points := range map[string][]store.PersonContactPoint{
		"mapped owner first":   {mapped, byIdentity},
		"identity owner first": {byIdentity, mapped},
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			envelope := parseProjectEnvelope(t, raw)
			occurrence := projectOccurrence(t, envelope, "EMAIL", 0)
			envelope.NativeMappings = []vcard.NativeMapping{{
				Identity: occurrence.Identity, SourceRef: envelope.SourceRef,
				Table: "person_contact_points", RowID: 10, Field: "original_value",
				Kind: vcard.HandlingNative,
			}}
			snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
				Person: store.Person{ID: 1, DisplayName: new("Alice")}, ContactPoints: points,
			}}

			projected, err := ProjectPersonEnvelope(snapshot, envelope)
			require.NoError(err)
			kept := projectOccurrenceByIdentity(t, projected, occurrence.Identity)
			assert.Equal("ten@example.com", kept.Property.RawValue,
				"the persisted mapping keeps its occurrence")
			assert.Equal([]string{"yes"}, projectedParameterValues(kept.Property, "X-KEEP"))
			assert.Equal([]string{"e1"}, projectedParameterValues(kept.Property, "PROP-ID"))
			body := string(projected.StoredBody)
			assert.Equal(1, strings.Count(body, "PROP-ID=e1"),
				"an appended occurrence must not repeat a PROP-ID the card already carries")
			assert.Contains(body, "EMAIL:eleven@example.com\r\n")
			tenth := mappingByOwner(t, projected.NativeMappings, "person_contact_points", 10, "original_value")
			assert.True(occurrence.Identity.Equal(tenth.Identity))
			eleventh := mappingByOwner(t, projected.NativeMappings, "person_contact_points", 11, "original_value")
			assert.False(occurrence.Identity.Equal(eleventh.Identity))
		})
	}
}

func TestProjectPersonEnvelopeDerivesFullNameOnlyWhenCardHasNone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	imported := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n"+
		"FN:Imported Name\r\nEND:VCARD\r\n"))
	structured := store.PersonName{
		Envelope: store.ValueEnvelope{ID: 3, Source: store.ProvenanceUser},
		NameKind: store.PersonNameStructured, FamilyName: new("Example"),
		GivenName: new("Alice"), HonorificPrefixes: new("Dr."),
		SecondarySurname: new("Sample"), Generation: new("Jr."),
	}
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1, DisplayName: new("Alice")},
		Names:  []store.PersonName{structured},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, imported)
	require.NoError(err)
	body := string(projected.StoredBody)
	assert.Equal(1, strings.Count(body, "\r\nFN"), "an imported FN is not doubled by a derived one")
	assert.Contains(body, "FN:Imported Name\r\n")
	assert.NotContains(body, "DERIVED")
	for _, mapping := range projected.NativeMappings {
		assert.NotEqual("derived_fn", mapping.Field)
	}

	bare := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n"+
		"NOTE:no name yet\r\nEND:VCARD\r\n"))
	derived, err := ProjectPersonEnvelope(snapshot, bare)
	require.NoError(err)
	assert.Contains(string(derived.StoredBody), "FN;DERIVED=true:Dr. Alice Example Sample Jr.\r\n")
	again, err := ProjectPersonEnvelope(snapshot, derived)
	require.NoError(err)
	assert.Equal(derived.StoredBody, again.StoredBody, "a derived FN the projection owns is updated, not doubled")
	assert.Equal(1, strings.Count(string(again.StoredBody), "\r\nFN"))
}

func TestProjectPersonPropertiesPhoneticNameEmitsScriptOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	both := store.PersonName{
		Envelope: store.ValueEnvelope{ID: 1, Source: store.ProvenanceUser},
		NameKind: store.PersonNamePhonetic, FamilyName: new("Jyu"), GivenName: new("Lei"),
		Script: new("Jpan"), PhoneticSystem: new("jyut"), PhoneticScript: new("Latn"),
	}
	scriptOnly := store.PersonName{
		Envelope: store.ValueEnvelope{ID: 2, Source: store.ProvenanceUser},
		NameKind: store.PersonNamePhonetic, FamilyName: new("Jyu"),
		Script: new("Jpan"), PhoneticSystem: new("jyut"),
	}
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1}, Names: []store.PersonName{both, scriptOnly},
	}}

	properties, _, err := projectPersonProperties(snapshot)
	require.NoError(err)
	phonetic := projectedByOwner(t, properties, "person_names", 1, "phonetic")
	assert.Len(phonetic.Property.ParametersNamed("SCRIPT"), 1)
	assert.Equal([]string{"Latn"}, projectedParameterValues(phonetic.Property, "SCRIPT"),
		"the phonetic script wins over the name script")
	assert.Equal([]string{"jyut"}, projectedParameterValues(phonetic.Property, "PHONETIC"))
	fallback := projectedByOwner(t, properties, "person_names", 2, "phonetic")
	assert.Equal([]string{"Jpan"}, projectedParameterValues(fallback.Property, "SCRIPT"))
}

func TestProjectPersonEnvelopeKeepsRelatedPrefAndVendorParameters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"RELATED;TYPE=friend,colleague;PREF=1;X-KEEP=yes:" +
		"urn:uuid:bba20e70-e528-4dcf-ae0c-a4fd3e71fe20\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	occurrence := projectOccurrence(t, envelope, "RELATED", 0)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: occurrence.Identity, SourceRef: envelope.SourceRef,
		Table: "person_relationships", RowID: 701, Field: "related",
		Kind: vcard.HandlingRelationship,
	}}
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
	}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	related := projectOccurrenceByIdentity(t, projected, occurrence.Identity)
	assert.Equal([]string{"1"}, projectedParameterValues(related.Property, "PREF"),
		"the projection has no typed field for PREF, so the imported one survives")
	assert.Equal([]string{"yes"}, projectedParameterValues(related.Property, "X-KEEP"))
	// One relationship row carries one relationship type: the store has no
	// place for further TYPE tokens, so only the wire type is rendered.
	assert.Equal([]string{"friend"}, projectedParameterValues(related.Property, "TYPE"))
	body := string(projected.StoredBody)
	assert.Contains(body, "PREF=1")
	assert.Contains(body, "X-KEEP=yes")

	again, err := ProjectPersonEnvelope(snapshot, projected)
	require.NoError(err)
	assert.Equal(projected.StoredBody, again.StoredBody)
}

func TestProjectPersonPropertiesPostalTypedFieldsWinOverExtendedComponents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1},
		Addresses: []store.PersonAddress{{
			Envelope:      store.ValueEnvelope{ID: 41, Source: store.ProvenanceUser},
			AddressKind:   store.PersonAddressPostal,
			StreetAddress: new("2 New St"), Locality: new("Newtown"),
			ExtendedComponents: new(
				"PO Box 1;Suite 2;1 Old St;Oldtown;CA;90210;US;Room 5;Floor 3",
			),
		}},
	}}

	properties, _, err := projectPersonProperties(snapshot)
	require.NoError(err)
	postal := projectedByOwner(t, properties, "person_addresses", 41, fieldValue)
	assert.Equal(";;2 New St;Newtown;;;;Room 5;Floor 3", postal.Property.RawValue,
		"typed fields render components 0-6; only components 7+ come from the extension")
}

func TestProjectPersonPropertiesKeepsLiteralEscapesInStructuredComponents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// On the wire, "\\n" is a literal backslash followed by n and "\," a
	// literal comma. Decoding the components more than once would turn the
	// first into a line break and corrupt the value on every re-render.
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1},
		Addresses: []store.PersonAddress{{
			Envelope:      store.ValueEnvelope{ID: 41, Source: store.ProvenanceUser},
			AddressKind:   store.PersonAddressPostal,
			OriginalValue: `;;C:\\new\\dir;Town\, State;;;`,
		}, {
			Envelope:      store.ValueEnvelope{ID: 42, Source: store.ProvenanceUser},
			AddressKind:   store.PersonAddressPostal,
			StreetAddress: new("1 Main St"),
			ExtendedComponents: new(
				`;;1 Main St;;;;;Wing\\north;Desk\, 4`,
			),
		}},
	}}

	properties, _, err := projectPersonProperties(snapshot)
	require.NoError(err)
	unstructured := projectedByOwner(t, properties, "person_addresses", 41, fieldValue)
	assert.Equal(`;;C:\\new\\dir;Town\, State;;;`, unstructured.Property.RawValue,
		"a wire-form original re-renders byte for byte")
	extended := projectedByOwner(t, properties, "person_addresses", 42, fieldValue)
	assert.Equal(`;;1 Main St;;;;;Wing\\north;Desk\, 4`, extended.Property.RawValue,
		"extended components keep their escapes through split and rejoin")
}

func TestProjectPersonPropertiesRendersPartialDateVariants(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dates := []store.PersonDate{
		{Envelope: store.ValueEnvelope{ID: 1, Source: store.ProvenanceUser},
			DateKind: store.PersonDateBirthday, Date: store.PartialDate{Year: new(1985)}},
		{Envelope: store.ValueEnvelope{ID: 2, Source: store.ProvenanceUser},
			DateKind: store.PersonDateBirthday, Date: store.PartialDate{Year: new(1985), Month: new(4)}},
		{Envelope: store.ValueEnvelope{ID: 3, Source: store.ProvenanceUser},
			DateKind: store.PersonDateBirthday, Date: store.PartialDate{Day: new(12)}},
		{Envelope: store.ValueEnvelope{ID: 4, Source: store.ProvenanceUser},
			DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: new(4)}},
		{Envelope: store.ValueEnvelope{ID: 5, Source: store.ProvenanceUser},
			DateKind: store.PersonDateBirthday},
	}
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1}, Dates: dates,
	}}

	properties, retained, err := projectPersonProperties(snapshot)
	require.NoError(err)
	for rowID, want := range map[int64]string{1: "1985", 2: "1985-04", 3: "---12", 4: "--04"} {
		date := projectedByOwner(t, properties, "person_dates", rowID, "date")
		assert.Equal(want, date.Property.RawValue)
		assert.Equal([]string{"date"}, projectedParameterValues(date.Property, "VALUE"))
	}
	assert.Contains(retained,
		retainedOwner{Owner: projectedOwner{Table: "person_dates", RowID: 5, Field: "date"}},
		"an empty date has nothing to render and keeps any imported occurrence")
}

func TestProjectPersonEnvelopeMatchesGroupsCaseInsensitively(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n"+
		"ITEM1.EMAIL;X-KEEP=yes:old@example.com\r\nEND:VCARD\r\n"))
	group := "item1"
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1, DisplayName: new("Alice")},
		ContactPoints: []store.PersonContactPoint{{
			Envelope: store.ValueEnvelope{ID: 100, Source: store.ProvenanceUser,
				VCard: store.VCardIdentity{Property: "EMAIL", Group: &group}},
			AddressKind: store.ContactAddressEmail, OriginalValue: "new@example.com",
		}},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	body := string(projected.StoredBody)
	assert.Contains(body, ".EMAIL;X-KEEP=yes:new@example.com\r\n")
	assert.Equal(1, strings.Count(body, "EMAIL"))
}

func TestProjectPersonEnvelopeRecordsRegistryHandlingOnMappings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"))
	friend := int64(1)
	text := "ambient"
	snapshot := store.PersonVCardSnapshot{
		Profile: store.PersonProfile{
			Person: store.Person{ID: 1, DisplayName: new("Alice")},
			ContactPoints: []store.PersonContactPoint{{
				Envelope:    store.ValueEnvelope{ID: 100, Source: store.ProvenanceUser},
				AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.com",
			}},
		},
		Employments: []store.PersonVCardEmployment{{
			Employment:   store.Employment{ID: 501, IsCurrent: true, IsPrimary: true, Title: new("Engineer")},
			Organization: store.OrganizationProfile{Organization: store.Organization{Name: "Org"}},
		}},
		Relationships: []store.PersonRelationshipView{{
			Relationship:        store.PersonRelationship{ID: 701, RelationshipTypeID: friend},
			Direction:           store.RelationshipDirectionOutgoing,
			CounterpartVCardUID: "bba20e70-e528-4dcf-ae0c-a4fd3e71fe20",
		}},
		RelationshipTypes: []store.RelationshipType{
			{ID: friend, Slug: "friend", IsSymmetric: true, VCardRelatedType: new("friend")},
		},
		Attributes: []store.PersonVCardAttribute{{
			Definition: store.AttributeDefinition{ID: 1, Slug: "genre", VCardProperty: new("X-GENRE")},
			Values: []store.PersonAttributeValue{{
				ID: 21, DefinitionID: 1,
				Value: store.AttributeValue{Type: store.AttributeValueText, Text: &text},
			}},
		}},
	}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	assert.Equal(vcard.HandlingNative,
		mappingByOwner(t, projected.NativeMappings, "person_contact_points", 100, "original_value").Kind)
	assert.Equal(vcard.HandlingDerived,
		mappingByOwner(t, projected.NativeMappings, "employments", 501, "organization_id").Kind)
	assert.Equal(vcard.HandlingDerived,
		mappingByOwner(t, projected.NativeMappings, "employments", 501, "title").Kind)
	assert.Equal(vcard.HandlingRelationship,
		mappingByOwner(t, projected.NativeMappings, "person_relationships", 701, "related").Kind)
	assert.Equal(vcard.HandlingNative,
		mappingByOwner(t, projected.NativeMappings, "person_attribute_values", 21, "value").Kind)
}

func TestProjectPersonEnvelopeRendersOrganizationUnitsAndSkipsBlankOrganizations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n"+
		"ORG;X-KEEP=yes:Imported Org\r\nEND:VCARD\r\n"))
	imported := projectOccurrence(t, envelope, "ORG", 0)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: imported.Identity, SourceRef: envelope.SourceRef,
		Table: "employments", RowID: 502, Field: "organization_id",
		Kind: vcard.HandlingDerived,
	}}
	snapshot := store.PersonVCardSnapshot{
		Profile: store.PersonProfile{Person: store.Person{ID: 1, DisplayName: new("Alice")}},
		Employments: []store.PersonVCardEmployment{
			{
				Employment:   store.Employment{ID: 501, IsCurrent: true, IsPrimary: true, Department: new(" Platform / Core ")},
				Organization: store.OrganizationProfile{Organization: store.Organization{Name: "Primary Org"}},
			},
		},
	}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	assert.Contains(string(projected.StoredBody), "ORG:Primary Org;Platform;Core\r\n")

	snapshot.Employments = []store.PersonVCardEmployment{{
		Employment: store.Employment{ID: 502, IsCurrent: true, IsPrimary: true,
			Department: new("Platform"), Source: store.ProvenanceVCardImport},
		Organization: store.OrganizationProfile{Organization: store.Organization{Name: "  "}},
	}}
	blank, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	assert.Contains(string(blank.StoredBody), "ORG;X-KEEP=yes:Imported Org\r\n",
		"a unit without an organization is not an ORG value; the imported line stays as residue")
	assert.Equal(1, strings.Count(string(blank.StoredBody), "ORG"))
	assert.Equal(vcard.HandlingPreserve,
		mappingByOwner(t, blank.NativeMappings, "employments", 502, "organization_id").Kind)
}

func TestProjectPersonEnvelopeReprojectionOfRichCardIsByteIdentical(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	uid := "bba20e70-e528-4dcf-ae0c-a4fd3e71fe20"
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n" +
		"FN;PROP-ID=f1;X-FN=keep:Alice Example\r\n" +
		"N;PROP-ID=n1;X-N=keep:Example;Alice;;;\r\n" +
		"NICKNAME;PROP-ID=k1;X-NICK=keep:Al\r\n" +
		"EMAIL;PROP-ID=e1;TYPE=home,work;PREF=1;X-EMAIL=keep:alice@example.com\r\n" +
		"TEL;PROP-ID=t1;TYPE=cell;X-TEL=keep:tel:+12025550123\r\n" +
		"ADR;PROP-ID=a1;TYPE=home;LABEL=Home;X-ADR=keep:;;1 Example St;Town;;;\r\n" +
		"CATEGORIES;PROP-ID=c1;X-CAT=keep:Friends\r\n" +
		"RELATED;PROP-ID=r1;TYPE=friend;PREF=2;X-REL=keep:urn:uuid:" + uid + "\r\n" +
		"X-VENDOR;X-FUTURE=yes:untouched\r\nEND:VCARD\r\n")
	envelope := parseProjectEnvelope(t, raw)
	identity := func(property, propID string) store.VCardIdentity {
		return store.VCardIdentity{Property: property, PropID: &propID}
	}
	friend := int64(1)
	snapshot := store.PersonVCardSnapshot{
		Profile: store.PersonProfile{
			Person: store.Person{ID: 1, DisplayName: new("Alice Example")},
			Names: []store.PersonName{
				{Envelope: store.ValueEnvelope{ID: 1, Source: store.ProvenanceUser, VCard: identity("FN", "f1")},
					NameKind: store.PersonNameFormatted, Formatted: new("Alice Example")},
				{Envelope: store.ValueEnvelope{ID: 2, Source: store.ProvenanceUser, VCard: identity("N", "n1")},
					NameKind: store.PersonNameStructured, FamilyName: new("Example"), GivenName: new("Alice")},
				{Envelope: store.ValueEnvelope{ID: 3, Source: store.ProvenanceUser, VCard: identity("NICKNAME", "k1")},
					NameKind: store.PersonNameNickname, Formatted: new("Al")},
			},
			ContactPoints: []store.PersonContactPoint{
				{Envelope: store.ValueEnvelope{ID: 4, Source: store.ProvenanceUser, VCard: identity("EMAIL", "e1"),
					TypeTokens: []string{"home", "work"}, Pref: new(1)},
					AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.com"},
				{Envelope: store.ValueEnvelope{ID: 5, Source: store.ProvenanceUser, VCard: identity("TEL", "t1"),
					TypeTokens: []string{"cell"}},
					AddressKind: store.ContactAddressPhone, OriginalValue: "+1 202 555 0123",
					NormalizedValue: "+12025550123"},
			},
			Addresses: []store.PersonAddress{{
				Envelope: store.ValueEnvelope{ID: 6, Source: store.ProvenanceUser, VCard: identity("ADR", "a1"),
					TypeTokens: []string{"home"}},
				AddressKind: store.PersonAddressPostal, StreetAddress: new("1 Example St"),
				Locality: new("Town"), Label: new("Home"),
			}},
			Categories: []store.PersonCategory{{
				Envelope:      store.ValueEnvelope{ID: 7, Source: store.ProvenanceUser, VCard: identity("CATEGORIES", "c1")},
				OriginalValue: "Friends",
			}},
		},
		Relationships: []store.PersonRelationshipView{{
			Relationship: store.PersonRelationship{ID: 701, RelationshipTypeID: friend,
				VCardIdentity: identity("RELATED", "r1")},
			Direction: store.RelationshipDirectionOutgoing, CounterpartVCardUID: uid,
		}},
		RelationshipTypes: []store.RelationshipType{
			{ID: friend, Slug: "friend", IsSymmetric: true, VCardRelatedType: new("friend")},
		},
	}

	first, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	body := string(first.StoredBody)
	for _, name := range []string{"FN", "N", "NICKNAME", "EMAIL", "TEL", "ADR", "CATEGORIES", "RELATED"} {
		assert.Equal(1, strings.Count(body, "\r\n"+name+";"), name)
	}
	for _, vendor := range []string{"X-FN=keep", "X-N=keep", "X-NICK=keep", "X-EMAIL=keep",
		"X-TEL=keep", "X-ADR=keep", "X-CAT=keep", "X-REL=keep", "X-VENDOR;X-FUTURE=yes:untouched"} {
		assert.Contains(body, vendor)
	}
	assert.Contains(body, "PREF=2")
	assert.Len(first.NativeMappings, 8)

	second, err := ProjectPersonEnvelope(snapshot, first)
	require.NoError(err)
	assert.Equal(first.StoredBody, second.StoredBody)
	assert.Equal(first.NativeMappings, second.NativeMappings)
	assert.Equal(first.RenderMetadata.Revision, second.RenderMetadata.Revision)
}

func TestProjectPersonEnvelopeRejectsDuplicateOwnersAndUnknownRelationshipTypes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"))
	duplicate := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1, DisplayName: new("Alice")},
		ContactPoints: []store.PersonContactPoint{
			{Envelope: store.ValueEnvelope{ID: 5, Source: store.ProvenanceUser},
				AddressKind: store.ContactAddressEmail, OriginalValue: "one@example.com"},
			{Envelope: store.ValueEnvelope{ID: 5, Source: store.ProvenanceUser},
				AddressKind: store.ContactAddressEmail, OriginalValue: "two@example.com"},
		},
	}}
	_, err := ProjectPersonEnvelope(duplicate, envelope)
	require.Error(err)
	assert.Contains(err.Error(), "duplicate projected vCard owner")

	untyped := store.PersonVCardSnapshot{
		Profile: store.PersonProfile{Person: store.Person{ID: 1, DisplayName: new("Alice")}},
		Relationships: []store.PersonRelationshipView{{
			Relationship:        store.PersonRelationship{ID: 701, RelationshipTypeID: 99},
			Direction:           store.RelationshipDirectionOutgoing,
			CounterpartVCardUID: "bba20e70-e528-4dcf-ae0c-a4fd3e71fe20",
		}},
	}
	_, err = ProjectPersonEnvelope(untyped, envelope)
	require.Error(err)
	assert.Contains(err.Error(), "relationship 701 has no snapshot type")
}

func TestProjectPersonEnvelopeLeavesForeignSourceMappingsAlone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n"+
		"FN;X-KEEP=yes:Foreign Owned\r\nEND:VCARD\r\n"))
	foreign := projectOccurrence(t, envelope, "FN", 0)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: foreign.Identity, SourceRef: "other-book",
		Table: "person_names", RowID: 7, Field: "formatted", Kind: vcard.HandlingNative,
	}}
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1},
		Names: []store.PersonName{{
			Envelope: store.ValueEnvelope{ID: 7, Source: store.ProvenanceUser},
			NameKind: store.PersonNameFormatted, Formatted: new("Typed Name"),
		}},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	untouched := projectOccurrenceByIdentity(t, projected, foreign.Identity)
	assert.Equal("Foreign Owned", untouched.Property.RawValue,
		"a mapping recorded for another source never binds this source's rows")
	assert.Contains(string(projected.StoredBody), "FN:Typed Name\r\n")
	require.Len(projected.NativeMappings, 2)
	kept := projected.NativeMappings[0]
	assert.Equal("other-book", kept.SourceRef)
	assert.True(foreign.Identity.Equal(kept.Identity))
	assert.Equal(envelope.SourceRef, projected.NativeMappings[1].SourceRef)
}

func TestProjectPersonEnvelopeNeverBindsOverForeignSourceMappings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n"+
		"FN;X-KEEP=yes:Foreign Owned\r\nEND:VCARD\r\n"))
	foreign := projectOccurrence(t, envelope, "FN", 0)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: foreign.Identity, SourceRef: "other-book",
		Table: "person_names", RowID: 7, Field: "formatted", Kind: vcard.HandlingNative,
	}}
	// The row's identity names an FN with no group or PROP-ID: exactly the
	// occurrence the other source owns, and the only FN on the card.
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1},
		Names: []store.PersonName{{
			Envelope: store.ValueEnvelope{
				ID: 9, Source: store.ProvenanceUser,
				VCard: store.VCardIdentity{Property: "FN"},
			},
			NameKind: store.PersonNameFormatted, Formatted: new("Typed Name"),
		}},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	untouched := projectOccurrenceByIdentity(t, projected, foreign.Identity)
	assert.Equal("Foreign Owned", untouched.Property.RawValue,
		"an identity fallback must not claim an occurrence another source owns")
	assert.Contains(string(projected.StoredBody), "FN:Typed Name\r\n")
	owners := make(map[string]int)
	for _, mapping := range projected.NativeMappings {
		owners[mapping.Identity.Key()]++
	}
	assert.Equal(1, owners[foreign.Identity.Key()],
		"one occurrence must never carry two native mappings")
	_, err = vcard.MarshalResourceMetadata(projected)
	require.NoError(err, "the projected metadata must stay valid")
}

func TestProjectPersonEnvelopeRetainsNamesThatRenderNothing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n"+
		"FN:Alice\r\nN;X-KEEP=yes:Doe;Jane;;;\r\nNICKNAME;X-KEEP=yes:Al\r\nEND:VCARD\r\n"))
	structured := projectOccurrence(t, envelope, "N", 0)
	nickname := projectOccurrence(t, envelope, "NICKNAME", 0)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: structured.Identity, SourceRef: envelope.SourceRef,
		Table: "person_names", RowID: 7, Field: "structured", Kind: vcard.HandlingNative,
	}, {
		Identity: nickname.Identity, SourceRef: envelope.SourceRef,
		Table: "person_names", RowID: 8, Field: "nickname", Kind: vcard.HandlingNative,
	}}
	// Both rows are current but carry nothing the projection can render: the
	// structured name has only its wire-form original, the nickname is blank.
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1, DisplayName: new("Alice")},
		Names: []store.PersonName{{
			Envelope: store.ValueEnvelope{ID: 7, Source: store.ProvenanceVCardImport},
			NameKind: store.PersonNameStructured, OriginalValue: "Doe;Jane;;;",
		}, {
			Envelope: store.ValueEnvelope{ID: 8, Source: store.ProvenanceVCardImport},
			NameKind: store.PersonNameNickname, Formatted: new("  "),
		}},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	body := string(projected.StoredBody)
	assert.Contains(body, "N;X-KEEP=yes:Doe;Jane;;;\r\n",
		"a current row that renders nothing keeps its imported occurrence")
	assert.Contains(body, "NICKNAME;X-KEEP=yes:Al\r\n")
	assert.Equal(vcard.HandlingPreserve,
		mappingByOwner(t, projected.NativeMappings, "person_names", 7, "structured").Kind)
	assert.Equal(vcard.HandlingPreserve,
		mappingByOwner(t, projected.NativeMappings, "person_names", 8, "nickname").Kind)
}

func TestProjectPersonEnvelopeOverLegacyCardKeepsAppendedOwnersAligned(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope, err := vcard.ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:2.1\r\n" +
		"N:Doe;Jane;;;\r\nEND:VCARD\r\n"))
	require.NoError(err)
	envelope.SourceRef = "book"
	envelope.SourceResourceUID = "resource"
	// Canonicalizing an N-only card synthesizes a derived FN, advancing the
	// ordinal counter past what the pre-merge envelope reported. Appended
	// owners must land on their own occurrences, not the synthesized FN's.
	snapshot := store.PersonVCardSnapshot{Profile: store.PersonProfile{
		Person: store.Person{ID: 1, DisplayName: new("Alice")},
		Names: []store.PersonName{{
			Envelope: store.ValueEnvelope{ID: 8, Source: store.ProvenanceUser},
			NameKind: store.PersonNameNickname, Formatted: new("Al"),
		}},
	}}

	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	body := string(projected.StoredBody)
	assert.Equal(1, strings.Count(body, "\r\nFN"), "exactly one FN line: %q", body)
	assert.Contains(body, "NICKNAME:Al\r\n")
	nickname := mappingByOwner(t, projected.NativeMappings, "person_names", 8, "nickname")
	assert.Equal("NICKNAME", nickname.Identity.OriginalName,
		"the appended nickname owner maps to its own occurrence")
	_, err = vcard.MarshalResourceMetadata(projected)
	require.NoError(err, "the projected metadata must stay valid")

	again, err := ProjectPersonEnvelope(snapshot, projected)
	require.NoError(err)
	assert.Equal(string(projected.StoredBody), string(again.StoredBody),
		"re-projection is byte-identical")
}

func TestProjectPersonEnvelopeRemovesFormerPrimaryEmploymentValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n"+
		"FN:Alice\r\nEND:VCARD\r\n"))
	first := store.PersonVCardEmployment{
		Employment: store.Employment{ID: 601, IsCurrent: true, IsPrimary: true,
			Title: new("Engineer"), Source: store.ProvenanceUser},
		Organization: store.OrganizationProfile{Organization: store.Organization{Name: "First Org"}},
	}
	second := store.PersonVCardEmployment{
		Employment: store.Employment{ID: 602, IsCurrent: true, IsPrimary: true,
			Title: new("Director"), Source: store.ProvenanceUser},
		Organization: store.OrganizationProfile{Organization: store.Organization{Name: "Second Org"}},
	}
	snapshot := store.PersonVCardSnapshot{
		Profile:     store.PersonProfile{Person: store.Person{ID: 1, DisplayName: new("Alice")}},
		Employments: []store.PersonVCardEmployment{first},
	}
	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	assert.Contains(string(projected.StoredBody), "ORG:First Org\r\n")

	// The primary switches: the projector generated First Org's lines, so
	// they leave the card with the primacy; only Second Org's remain.
	first.Employment.IsPrimary = false
	snapshot.Employments = []store.PersonVCardEmployment{first, second}
	switched, err := ProjectPersonEnvelope(snapshot, projected)
	require.NoError(err)
	body := string(switched.StoredBody)
	assert.NotContains(body, "First Org", "former primary values must not accumulate")
	assert.NotContains(body, "TITLE:Engineer\r\n")
	assert.Contains(body, "ORG:Second Org\r\n")
	assert.Contains(body, "TITLE:Director\r\n")
	assert.Equal(1, strings.Count(body, "\r\nORG"), "%q", body)
}

func TestProjectPersonEnvelopeKeepsImportedEmploymentValuesAsResidue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n"+
		"FN:Alice\r\nORG;X-KEEP=yes:Imported Org\r\nEND:VCARD\r\n"))
	imported := projectOccurrence(t, envelope, "ORG", 0)
	envelope.NativeMappings = []vcard.NativeMapping{{
		Identity: imported.Identity, SourceRef: envelope.SourceRef,
		Table: "employments", RowID: 701, Field: "organization_id",
		Kind: vcard.HandlingDerived,
	}}
	// The employment came from this card's import; demoting it from primary
	// must keep the imported line as residue, not delete it.
	snapshot := store.PersonVCardSnapshot{
		Profile: store.PersonProfile{Person: store.Person{ID: 1, DisplayName: new("Alice")}},
		Employments: []store.PersonVCardEmployment{{
			Employment: store.Employment{ID: 701, IsCurrent: true, IsPrimary: false,
				Source: store.ProvenanceVCardImport},
			Organization: store.OrganizationProfile{Organization: store.Organization{Name: "Imported Org"}},
		}},
	}
	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	assert.Contains(string(projected.StoredBody), "ORG;X-KEEP=yes:Imported Org\r\n")
	assert.Equal(vcard.HandlingPreserve,
		mappingByOwner(t, projected.NativeMappings, "employments", 701, "organization_id").Kind)
}

func TestProjectPersonEnvelopeDropsGeneratedRelatedWhenWireTypeClears(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n"+
		"FN:Alice\r\nEND:VCARD\r\n"))
	wire := "kin"
	relationshipType := store.RelationshipType{ID: 601, IsSymmetric: true, VCardRelatedType: &wire}
	snapshot := store.PersonVCardSnapshot{
		Profile:           store.PersonProfile{Person: store.Person{ID: 1, DisplayName: new("Alice")}},
		RelationshipTypes: []store.RelationshipType{relationshipType},
		Relationships: []store.PersonRelationshipView{{
			Relationship: store.PersonRelationship{
				ID: 801, RelationshipTypeID: 601, Source: store.ProvenanceUser,
			},
			Direction:           store.RelationshipDirectionIncoming,
			CounterpartVCardUID: "bba20e70-e528-4dcf-ae0c-a4fd3e71fe20",
		}},
	}
	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	require.Contains(string(projected.StoredBody), "RELATED;TYPE=kin")

	// The type's wire mapping is cleared. The projector generated this line —
	// the edge is user-created, not imported card data — so it leaves the
	// card instead of surviving as residue.
	snapshot.RelationshipTypes[0].VCardRelatedType = nil
	cleared, err := ProjectPersonEnvelope(snapshot, projected)
	require.NoError(err)
	assert.NotContains(string(cleared.StoredBody), "RELATED",
		"a projector-generated RELATED must not outlive its wire type")
}

func TestProjectPersonEnvelopeRetainsEmploymentResidueOnlyInItsOwnResource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// The employment was imported from book "other-book"; this envelope is
	// "book". Any occurrence mapped to it here was generated by the
	// projector, so demoting the employment removes it from this card.
	otherBook := "other-book"
	employment := store.PersonVCardEmployment{
		Employment: store.Employment{ID: 601, IsCurrent: true, IsPrimary: true,
			Title: new("Engineer"), Source: store.ProvenanceVCardImport, SourceRef: &otherBook},
		Organization: store.OrganizationProfile{Organization: store.Organization{Name: "First Org"}},
	}
	snapshot := store.PersonVCardSnapshot{
		Profile:     store.PersonProfile{Person: store.Person{ID: 1, DisplayName: new("Alice")}},
		Employments: []store.PersonVCardEmployment{employment},
	}
	envelope := parseProjectEnvelope(t, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n"+
		"FN:Alice\r\nEND:VCARD\r\n"))
	projected, err := ProjectPersonEnvelope(snapshot, envelope)
	require.NoError(err)
	require.Contains(string(projected.StoredBody), "ORG:First Org\r\n")

	snapshot.Employments[0].Employment.IsPrimary = false
	demoted, err := ProjectPersonEnvelope(snapshot, projected)
	require.NoError(err)
	assert.NotContains(string(demoted.StoredBody), "First Org",
		"an employment imported from another resource retains nothing here")
	assert.NotContains(string(demoted.StoredBody), "TITLE:Engineer")
}
