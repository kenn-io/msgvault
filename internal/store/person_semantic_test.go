package store_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonSemanticDocumentRendersCuratedCurrentTextExactly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	personID := mustPerson(t, f, "semantic-person@example.invalid", "Synthetic Person")
	counterpartID := mustPerson(t, f, "semantic-counterpart@example.invalid", "Synthetic Counterpart")

	currentName, err := f.Store.AddPersonNameContext(ctx, personID, store.PersonNameInput{
		NameKind: store.PersonNameNickname, Formatted: new("  Alias\nPerson  "),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	historicalName, err := f.Store.AddPersonNameContext(ctx, personID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Historical Name"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	require.NoError(f.Store.SupersedePersonNameContext(ctx, personID, historicalName.Envelope.ID, nil))
	_, err = f.Store.AddPersonCategoryContext(ctx, personID, store.PersonCategoryInput{
		OriginalValue: " Collaborator ",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	_, err = f.Store.AddPersonAddressContext(ctx, personID, store.PersonAddressInput{
		AddressKind:   store.PersonAddressPostal,
		StreetAddress: new("42 Private Street"), PostalCode: new("A1B 2C3"),
		Locality: new("Harbor City"), Region: new("North Region"),
		CountryName: new("Exampleland"),
		Envelope:    store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	_, err = f.Store.AddPersonContactPointContext(ctx, personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "private@example.invalid",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	_, err = f.Store.AddPersonContactPointContext(ctx, personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressPhone, OriginalValue: "+15550000001",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	_, err = f.Store.AddPersonDateContext(ctx, personID, store.PersonDateInput{
		DateKind: store.PersonDateBirthday, DateText: new("private-date"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	_, err = f.Store.AddPersonMediaContext(ctx, personID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto, URI: new("https://media.example.invalid/private"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	_, err = f.Store.RecordContactObservationContext(ctx,
		f.EnsureParticipant("observed@example.invalid", "observed-secret", "example.invalid"),
		store.ParticipantContactObservationInput{
			AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"),
			OriginalValue: "observed-secret",
			Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
		})
	require.NoError(err)

	createSemanticAttribute(t, f, "favorite_topics", "Favorite topics",
		store.AttributeValueJSON, store.AttributeFieldJSON, true, false)
	_, err = f.Store.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: "favorite_topics",
		Value: store.AttributeValue{Type: store.AttributeValueJSON,
			JSON: json.RawMessage(`{ "z": "distributed\nsystems", "a": [2, 1] }`)},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	createSemanticAttribute(t, f, "private_code", "Private code",
		store.AttributeValueText, store.AttributeFieldText, true, true)
	setSemanticTextAttribute(t, f, personID, "private_code", "sensitive-secret")
	createSemanticAttribute(t, f, "internal_note", "Internal note",
		store.AttributeValueText, store.AttributeFieldText, false, false)
	setSemanticTextAttribute(t, f, personID, "internal_note", "non-searchable-secret")
	createSemanticAttribute(t, f, "email_alias", "Email alias",
		store.AttributeValueText, store.AttributeFieldEmail, true, false)
	setSemanticTextAttribute(t, f, personID, "email_alias", "typed-private@example.invalid")
	createSemanticAttribute(t, f, "important_date", "Important date",
		store.AttributeValueDate, store.AttributeFieldDate, true, false)
	_, err = f.Store.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: "important_date",
		Value:  store.AttributeValue{Type: store.AttributeValueDate, Date: new("2026-08-20")},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)

	organization, err := f.Store.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Labs", Kind: store.OrganizationKindCompany,
		PrimaryDomain: new("example.invalid"), Description: new("Synthetic organization"),
	})
	require.NoError(err)
	organizationProfile, err := f.Store.ReplaceOrganizationProfileContext(
		ctx, organization.ID, organization.Revision, store.OrganizationProfileInput{
			Names: []store.OrganizationNameInput{{
				Name: "Synthetic Research Lab", NameKind: store.OrganizationNameKindAlias,
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
			Addresses: []store.OrganizationAddressInput{{
				AddressKind:   store.PersonAddressPostal,
				StreetAddress: new("99 Hidden Avenue"), PostalCode: new("99999"),
				Locality: new("Worktown"), Region: new("Work Region"), CountryName: new("Workland"),
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
			ContactPoints: []store.OrganizationContactPointInput{{
				AddressKind: store.ContactAddressEmail, OriginalValue: "org-private@example.invalid",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
			Media: []store.OrganizationMediaInput{{
				MediaKind: store.PersonMediaLogo, URI: new("https://media.example.invalid/org-private"),
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
			Categories: []store.OrganizationCategoryInput{{
				Category: "Research", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			}},
		})
	require.NoError(err)
	_, err = f.Store.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: personID, OrganizationID: organizationProfile.Organization.ID,
		Title: new("Principal Engineer"), Role: new("Platform Lead"),
		Department: new("Research"), Location: new("Remote"),
		Description: new("Builds synthetic systems"), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	past, err := f.Store.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: personID, OrganizationID: organizationProfile.Organization.ID,
		Title: new("Historical Job"), IsCurrent: new(false), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	require.False(past.IsCurrent)

	_, err = f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: personID, TargetPersonID: counterpartID, TypeSlug: "parent",
		Notes: new("relationship-private-note"), Source: store.ProvenanceUser, Actor: "synthetic-user",
	})
	require.NoError(err)
	ended, err := f.Store.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: personID, TargetPersonID: counterpartID, TypeSlug: "co-worker",
		Source: store.ProvenanceUser, Actor: "synthetic-user",
	})
	require.NoError(err)
	endDate, err := store.ParseRelationshipDate("2024")
	require.NoError(err)
	_, err = f.Store.EndPersonRelationshipContext(ctx, ended.ID, ended.Revision, endDate, "synthetic-user")
	require.NoError(err)

	document, err := f.Store.LoadPersonSemanticDocumentContext(ctx, personID)
	require.NoError(err)
	wantText := strings.Join([]string{
		"Display name: Synthetic Person",
		"Alternate name: Alias Person",
		"Category: Collaborator",
		"Location: Harbor City, North Region, Exampleland",
		`Attribute Favorite topics [favorite_topics]: {"a":[2,1],"z":"distributed\nsystems"}`,
		"Current employment: Principal Engineer; Platform Lead; Research; Remote; Builds synthetic systems at Example Labs",
		"Organization: Example Labs",
		"Organization alternate name: Synthetic Research Lab",
		"Organization category: Research",
		"Organization description: Synthetic organization",
		"Organization domain: example.invalid",
		"Organization kind: company",
		"Organization location: Worktown, Work Region, Workland",
		"Relationship: child — Synthetic Counterpart",
	}, "\n")
	assert.Equal(wantText, document.Text)
	assert.Equal(personID, document.PersonID)
	assert.Equal(store.PersonSemanticRendererPolicy, document.RendererPolicy)
	assert.Equal(semanticDigest(wantText), document.Revision)
	assert.NotEmpty(currentName.Envelope.Source, "fixture includes excluded provenance")
	for _, excluded := range []string{
		"private@example.invalid", "+15550000001", "42 Private Street", "A1B 2C3",
		"private-date", "media.example.invalid", "observed-secret", "Historical Name",
		"sensitive-secret", "non-searchable-secret", "typed-private@example.invalid",
		"2026-08-20", "99 Hidden Avenue", "99999", "org-private@example.invalid",
		"Historical Job", "relationship-private-note", "co-worker", "synthetic-user",
	} {
		assert.NotContains(document.Text, excluded)
	}
}

func TestPersonSemanticDocumentIsStableAcrossInsertionOrderAndCanonicalJSON(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	firstID := mustPerson(t, f, "semantic-order-a@example.invalid", "Same Person")
	secondID := mustPerson(t, f, "semantic-order-b@example.invalid", "Same Person")

	createSemanticAttribute(t, f, "semantic_json", "Structured data",
		store.AttributeValueJSON, store.AttributeFieldJSON, true, false)
	for _, fixture := range []struct {
		personID   int64
		names      []string
		categories []string
		jsonValue  string
	}{
		{firstID, []string{"Zulu", "Alpha"}, []string{"Team Z", "Team A"}, `{"z":2,"a":{"y":1,"x":0}}`},
		{secondID, []string{"Alpha", "Zulu"}, []string{"Team A", "Team Z"}, `{ "a": { "x": 0, "y": 1 }, "z": 2 }`},
	} {
		for _, name := range fixture.names {
			_, err := f.Store.AddPersonNameContext(t.Context(), fixture.personID, store.PersonNameInput{
				NameKind: store.PersonNameNickname, Formatted: &name,
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			})
			require.NoError(err)
		}
		for _, category := range fixture.categories {
			_, err := f.Store.AddPersonCategoryContext(t.Context(), fixture.personID,
				store.PersonCategoryInput{OriginalValue: category,
					Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
			require.NoError(err)
		}
		_, err := f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
			PersonID: fixture.personID, DefinitionSlug: "semantic_json",
			Value: store.AttributeValue{Type: store.AttributeValueJSON,
				JSON: json.RawMessage(fixture.jsonValue)},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
	}

	first, err := f.Store.LoadPersonSemanticDocumentContext(t.Context(), firstID)
	require.NoError(err)
	second, err := f.Store.LoadPersonSemanticDocumentContext(t.Context(), secondID)
	require.NoError(err)
	assert.Equal(first.Text, second.Text)
	assert.Equal(first.Revision, second.Revision)
	assert.Equal(strings.Join([]string{
		"Display name: Same Person",
		"Alternate name: Alpha",
		"Alternate name: Zulu",
		"Category: Team A",
		"Category: Team Z",
		`Attribute Structured data [semantic_json]: {"a":{"x":0,"y":1},"z":2}`,
	}, "\n"), first.Text)
}

func TestPersonSemanticDocumentIncludesEveryAllowedTypedAttributeKind(t *testing.T) {
	f := storetest.New(t)
	personID := mustPerson(t, f, "semantic-types@example.invalid", "Typed Person")
	targetID := mustPerson(t, f, "semantic-types-target@example.invalid", "Target Person")

	tests := []struct {
		slug, label string
		valueType   store.AttributeValueType
		fieldType   store.AttributeFieldType
		value       store.AttributeValue
		want        string
	}{
		{"bool_value", "Boolean value", store.AttributeValueBoolean, store.AttributeFieldCheckbox,
			store.AttributeValue{Type: store.AttributeValueBoolean, Boolean: new(true)}, "true"},
		{"integer_value", "Integer value", store.AttributeValueInteger, store.AttributeFieldDuration,
			store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(42))}, "42"},
		{"real_value", "Real value", store.AttributeValueReal, store.AttributeFieldText,
			store.AttributeValue{Type: store.AttributeValueReal, Real: new(2.5)}, "2.5"},
		{"text_value", "Text value", store.AttributeValueText, store.AttributeFieldTextarea,
			store.AttributeValue{Type: store.AttributeValueText, Text: new("multi\n line")}, "multi line"},
		{"record_value", "Record value", store.AttributeValueRecordReference, store.AttributeFieldPerson,
			store.AttributeValue{Type: store.AttributeValueRecordReference,
				RecordType: new("person"), RecordID: &targetID}, fmt.Sprintf("person:%d", targetID)},
	}
	for _, tc := range tests {
		createSemanticAttribute(t, f, tc.slug, tc.label, tc.valueType, tc.fieldType, true, false)
		_, err := f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: tc.slug, Value: tc.value,
			Source: store.ProvenanceUser,
		})
		require.NoError(t, err)
	}

	document, err := f.Store.LoadPersonSemanticDocumentContext(t.Context(), personID)
	require.NoError(t, err)
	for _, tc := range tests {
		assert.Contains(t, document.Text,
			fmt.Sprintf("Attribute %s [%s]: %s", tc.label, tc.slug, tc.want))
	}
}

func TestPersonSemanticDocumentMutationListAndDeletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	emptyParticipant := f.EnsureParticipant(
		"semantic-empty@example.invalid", "observed-only", "example.invalid")
	emptyPerson, _, err := f.Store.CreatePersonFromParticipantContext(ctx, emptyParticipant)
	require.NoError(err)
	populatedID := mustPerson(t, f, "semantic-list@example.invalid", "Before Mutation")

	empty, err := f.Store.LoadPersonSemanticDocumentContext(ctx, emptyPerson.ID)
	require.NoError(err)
	assert.Empty(empty.Text, "an extant person with no curated discoverability text is valid")
	assert.Equal(semanticDigest(""), empty.Revision)

	before, err := f.Store.LoadPersonSemanticDocumentContext(ctx, populatedID)
	require.NoError(err)
	person, err := f.Store.GetPersonContext(ctx, populatedID)
	require.NoError(err)
	_, err = f.Store.UpdatePersonDisplayNameContext(ctx, populatedID, person.Revision, new("After Mutation"))
	require.NoError(err)
	after, err := f.Store.LoadPersonSemanticDocumentContext(ctx, populatedID)
	require.NoError(err)
	assert.NotEqual(before.Revision, after.Revision)
	assert.Equal("Display name: After Mutation", after.Text)

	documents, err := f.Store.ListPersonSemanticDocumentsContext(ctx)
	require.NoError(err)
	require.Len(documents, 2)
	assert.Less(documents[0].PersonID, documents[1].PersonID)

	person, err = f.Store.GetPersonContext(ctx, populatedID)
	require.NoError(err)
	require.NoError(f.Store.DeletePersonContext(ctx, populatedID, person.Revision))
	_, err = f.Store.LoadPersonSemanticDocumentContext(ctx, populatedID)
	require.ErrorIs(err, store.ErrPersonNotFound)
	documents, err = f.Store.ListPersonSemanticDocumentsContext(ctx)
	require.NoError(err)
	require.Len(documents, 1)
	assert.Equal(emptyPerson.ID, documents[0].PersonID)
}

func TestResolvePersonSemanticCandidatesReturnsOnlyCurrentRootsInCandidateOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	firstID := mustPerson(t, f, "semantic-candidate-first@example.invalid", "Synthetic First")
	secondID := mustPerson(t, f, "semantic-candidate-second@example.invalid", "Synthetic Second")
	deletedID := mustPerson(t, f, "semantic-candidate-deleted@example.invalid", "Synthetic Deleted")
	lastID := mustPerson(t, f, "semantic-candidate-last@example.invalid", "Synthetic Last")
	emptyParticipant := f.EnsureParticipant(
		"semantic-candidate-empty@example.invalid", "observed-only", "example.invalid")
	emptyPerson, _, err := f.Store.CreatePersonFromParticipantContext(ctx, emptyParticipant)
	require.NoError(err)
	require.Greater(lastID, secondID, "precondition: candidates will reverse durable ID order")
	firstDocument, err := f.Store.LoadPersonSemanticDocumentContext(ctx, firstID)
	require.NoError(err)
	secondDocument, err := f.Store.LoadPersonSemanticDocumentContext(ctx, secondID)
	require.NoError(err)
	deletedDocument, err := f.Store.LoadPersonSemanticDocumentContext(ctx, deletedID)
	require.NoError(err)
	lastDocument, err := f.Store.LoadPersonSemanticDocumentContext(ctx, lastID)
	require.NoError(err)
	emptyDocument, err := f.Store.LoadPersonSemanticDocumentContext(ctx, emptyPerson.ID)
	require.NoError(err)
	require.Empty(emptyDocument.Text)

	first, err := f.Store.GetPersonContext(ctx, firstID)
	require.NoError(err)
	_, err = f.Store.UpdatePersonDisplayNameContext(
		ctx, firstID, first.Revision, new("Synthetic First Updated"),
	)
	require.NoError(err)
	deleted, err := f.Store.GetPersonContext(ctx, deletedID)
	require.NoError(err)
	require.NoError(f.Store.DeletePersonContext(ctx, deletedID, deleted.Revision))

	people, err := f.Store.ResolvePersonSemanticCandidatesContext(ctx, []store.PersonSemanticCandidate{
		{PersonID: emptyPerson.ID, Revision: emptyDocument.Revision},
		{PersonID: lastID, Revision: lastDocument.Revision},
		{PersonID: secondID, Revision: secondDocument.Revision},
		{PersonID: firstID, Revision: firstDocument.Revision},
		{PersonID: deletedID, Revision: deletedDocument.Revision},
		{PersonID: 999999, Revision: "missing"},
	})
	require.NoError(err)
	require.Len(people, 2)
	assert.Equal([]int64{lastID, secondID}, []int64{people[0].ID, people[1].ID})
	require.NotNil(people[0].DisplayName)
	require.NotNil(people[1].DisplayName)
	assert.Equal([]string{"Synthetic Last", "Synthetic Second"}, []string{
		*people[0].DisplayName, *people[1].DisplayName,
	})
}

func TestPersonSemanticScanFailsButResolverSkipsOneUnrenderablePerson(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	badID := mustPerson(t, f, "semantic-bad@example.invalid", "Broken Projection")
	goodID := mustPerson(t, f, "semantic-good@example.invalid", "Healthy Projection")
	setSemanticTextAttribute(t, f, badID, store.AttributeSlugAskMeAbout, "distributed systems")

	goodDocument, err := f.Store.LoadPersonSemanticDocumentContext(ctx, goodID)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE attribute_definitions
		SET value_type = 'record_reference', field_type = 'person', record_target = 'person'
		WHERE universal_id = ?
	`), store.AttributeUniversalIDAskMeAbout)
	require.NoError(err)

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	documents, err := f.Store.ListPersonSemanticDocumentsContext(ctx)
	require.ErrorContains(err, "incomplete record reference")
	assert.Empty(documents)

	people, err := f.Store.ResolvePersonSemanticCandidatesContext(ctx, []store.PersonSemanticCandidate{
		{PersonID: badID, Revision: "unrenderable"},
		{PersonID: goodID, Revision: goodDocument.Revision},
	})
	require.NoError(err)
	require.Len(people, 1)
	assert.Equal(goodID, people[0].ID)
	assert.Contains(logs.String(), "person_id="+strconv.FormatInt(badID, 10))
}

func TestPersonSemanticDocumentCapsExactHashedUTF8Bytes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	personID := mustPerson(t, f, "semantic-cap@example.invalid", "before cap")
	person, err := f.Store.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	longName := strings.Repeat("雪", store.MaxPersonSemanticDocumentBytes)
	_, err = f.Store.UpdatePersonDisplayNameContext(
		t.Context(), personID, person.Revision, &longName)
	require.NoError(err)

	document, err := f.Store.LoadPersonSemanticDocumentContext(t.Context(), personID)
	require.NoError(err)
	assert.Len([]byte(document.Text), store.MaxPersonSemanticDocumentBytes)
	assert.True(utf8.ValidString(document.Text))
	assert.Equal(semanticDigest(document.Text), document.Revision)
}

func createSemanticAttribute(
	t *testing.T, f *storetest.Fixture, slug, label string,
	valueType store.AttributeValueType, fieldType store.AttributeFieldType,
	searchable, sensitive bool,
) {
	t.Helper()
	input := personTextDefinition(slug)
	input.UniversalID = "person-semantic-test-" + slug
	input.Label = label
	input.ValueType = valueType
	input.FieldType = fieldType
	input.IsSearchable = searchable
	input.IsSensitive = sensitive
	if valueType == store.AttributeValueRecordReference {
		input.RecordTarget = new("person")
	}
	_, err := f.Store.CreateAttributeDefinitionContext(t.Context(), input)
	require.NoError(t, err)
}

func setSemanticTextAttribute(
	t *testing.T, f *storetest.Fixture, personID int64, slug, value string,
) {
	t.Helper()
	_, err := f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: slug,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &value},
		Source: store.ProvenanceUser,
	})
	require.NoError(t, err)
}

func semanticDigest(text string) string {
	digest := sha256.Sum256([]byte(store.PersonSemanticRendererPolicy + "\x00" + text))
	return hex.EncodeToString(digest[:])
}
