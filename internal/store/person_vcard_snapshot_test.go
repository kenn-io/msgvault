package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vcard"
)

func TestPersonVCardSnapshotLoadsAllProjectionInputsDeterministically(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	person := createEnvelopePerson(t, st, "alice@example.com")
	counterpart := createEnvelopePerson(t, st, "bob@example.com")
	_, err := st.AddPersonNameContext(ctx, person.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alice Example"),
		OriginalValue: "Alice Example",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	media, err := st.AddPersonMediaContext(ctx, person.ID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto, MediaType: new("image/png"),
		Data:     []byte("synthetic-photo"),
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	definitionInput := personTextDefinition("favorite_genre")
	definitionInput.UniversalID = "test-vcard-favorite-genre"
	definitionInput.VCardProperty = new("X-FAVORITE-GENRE")
	definition, err := st.CreateAttributeDefinitionContext(ctx, definitionInput)
	require.NoError(err)
	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person.ID, DefinitionSlug: definition.Slug,
		Value: store.AttributeValue{
			Type: store.AttributeValueText, Text: new("ambient"),
		},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	organization, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	_, err = st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	_, err = st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{
		SourcePersonID: person.ID, TargetPersonID: counterpart.ID,
		TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "test",
	})
	require.NoError(err)
	resolution, err := st.ResolveRelatedValueContext(ctx, store.RelatedImport{
		PersonID: person.ID, RawValue: "Unresolved Person",
		RawType: "unknown-relation", ValueKind: store.RelatedValueKindText,
		Source: store.ProvenanceVCardImport, Actor: "test",
		VCardIdentity: store.VCardIdentity{Property: "RELATED"},
	})
	require.NoError(err)
	require.NotNil(resolution.Review)

	first, err := st.LoadPersonVCardSnapshotContext(ctx, person.ID)
	require.NoError(err)
	second, err := st.LoadPersonVCardSnapshotContext(ctx, person.ID)
	require.NoError(err)
	assert.Equal(first.Fingerprint, second.Fingerprint)
	assert.Len(first.Profile.Names, 1)
	require.Len(first.MediaData, 1)
	assert.Equal(media.Envelope.ID, first.MediaData[0].MediaID)
	assert.Equal([]byte("synthetic-photo"), first.MediaData[0].Data)
	attributes := make(map[string]store.PersonVCardAttribute, len(first.Attributes))
	for _, attribute := range first.Attributes {
		attributes[attribute.Definition.Slug] = attribute
	}
	customAttribute, ok := attributes[definition.Slug]
	require.True(ok, "custom vCard attribute is present")
	require.NotNil(customAttribute.Definition.VCardProperty)
	assert.Equal("X-FAVORITE-GENRE", *customAttribute.Definition.VCardProperty)
	require.Len(customAttribute.Values, 1)
	assert.Equal("ambient", *customAttribute.Values[0].Value.Text)
	notesAttribute, ok := attributes[store.AttributeSlugNotes]
	require.True(ok, "standard Notes vCard attribute is present")
	require.NotNil(notesAttribute.Definition.VCardProperty)
	assert.Equal("NOTE", *notesAttribute.Definition.VCardProperty)
	assert.Empty(notesAttribute.Values)
	require.Len(first.Employments, 1)
	assert.Equal(organization.ID, first.Employments[0].Organization.Organization.ID)
	require.Len(first.Relationships, 1)
	assert.Equal(counterpart.VCardUID, first.Relationships[0].CounterpartVCardUID)
	require.Len(first.RelationshipTypes, 1)
	assert.Equal("friend", first.RelationshipTypes[0].Slug)
	require.Len(first.PendingRelationshipReviews, 1)
	assert.Equal(resolution.Review.ID, first.PendingRelationshipReviews[0].ID)

	_, err = st.AddPersonCategoryContext(ctx, person.ID, store.PersonCategoryInput{
		OriginalValue: "Friends",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	changed, err := st.LoadPersonVCardSnapshotContext(ctx, person.ID)
	require.NoError(err)
	assert.NotEqual(first.Fingerprint, changed.Fingerprint)
}

func TestPersonNotesSnapshotCarriesVCardDefinitionAndValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	person := createEnvelopePerson(t, st, "notes@example.test")
	note := "line one\nline two"
	written, err := st.SetPersonAttributeValueContext(t.Context(),
		store.PersonAttributeValueInput{
			PersonID: person.ID, DefinitionSlug: store.AttributeSlugNotes,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &note},
			Source: store.ProvenanceUser,
		})
	require.NoError(err)

	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), person.ID)
	require.NoError(err)
	require.Len(snapshot.Attributes, 1)
	attribute := snapshot.Attributes[0]
	require.NotNil(attribute.Definition.VCardProperty)
	assert.Equal("NOTE", *attribute.Definition.VCardProperty)
	require.Len(attribute.Values, 1)
	assert.Equal(written.Value.ID, attribute.Values[0].ID)
	require.NotNil(attribute.Values[0].Value.Text)
	assert.Equal(note, *attribute.Values[0].Value.Text)
}

// A snapshot's employment set doubles as the projection's retention set, so a
// page-sized read would delete the mapped properties of every employment past
// the first page.
func TestPersonVCardSnapshotRetainsEmploymentsBeyondOnePage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	person := createEnvelopePerson(t, st, "alice@example.com")
	organization, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Org", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	total := store.DefaultEmploymentPageSize + 1
	created := make([]store.Employment, 0, total)
	for i := range total {
		employment, addErr := st.AddEmploymentContext(ctx, store.EmploymentInput{
			PersonID: person.ID, OrganizationID: organization.ID,
			Title:  new(fmt.Sprintf("Engineer %03d", i)),
			Source: store.ProvenanceUser,
		})
		require.NoError(addErr)
		created = append(created, *employment)
	}

	snapshot, err := st.LoadPersonVCardSnapshotContext(ctx, person.ID)
	require.NoError(err)
	assert.Len(snapshot.Employments, total)
	loaded := make(map[int64]store.PersonVCardEmployment, len(snapshot.Employments))
	for _, entry := range snapshot.Employments {
		loaded[entry.Employment.ID] = entry
	}
	for _, employment := range created {
		entry, present := loaded[employment.ID]
		if !assert.True(present, "employment %d missing from snapshot", employment.ID) {
			continue
		}
		assert.Equal(employment.Title, entry.Employment.Title)
		assert.Equal(organization.ID, entry.Organization.Organization.ID)
	}

	// The paged listing keeps its bounds; only the snapshot reads past them.
	firstPage, err := st.ListEmploymentsContext(
		ctx, store.EmploymentFilter{PersonID: person.ID},
	)
	require.NoError(err)
	assert.Len(firstPage, store.DefaultEmploymentPageSize)
	secondPage, err := st.ListEmploymentsContext(ctx, store.EmploymentFilter{
		PersonID: person.ID, Limit: 10, Offset: store.DefaultEmploymentPageSize,
	})
	require.NoError(err)
	require.Len(secondPage, total-store.DefaultEmploymentPageSize)

	// The last row sorts past the first page: every other employment shares its
	// ordering keys, so the listing falls back to ascending id.
	beyondPage := created[total-1]
	assert.Equal(beyondPage.ID, secondPage[0].ID)
	_, err = st.UpdateEmploymentContext(
		ctx, beyondPage.ID, beyondPage.Revision, store.EmploymentInput{
			PersonID: person.ID, OrganizationID: organization.ID,
			Title: new("Staff Engineer"), Source: store.ProvenanceUser,
		},
	)
	require.NoError(err)
	changed, err := st.LoadPersonVCardSnapshotContext(ctx, person.ID)
	require.NoError(err)
	assert.NotEqual(snapshot.Fingerprint, changed.Fingerprint)
}

func TestVCardSemanticCommitRejectsChangedProjectionAtomically(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	person := createEnvelopePerson(t, st, "alice@example.com")
	_, err := st.AddPersonNameContext(ctx, person.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alice"),
		OriginalValue: "Alice",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n")
	created, snapshot, prepared := renderVCardEnvelope(t, st, person.ID,
		raw, "projection-conflict", "Rendered From Old Snapshot")
	_, err = st.AddPersonCategoryContext(ctx, person.ID, store.PersonCategoryInput{
		OriginalValue: "Changed",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	_, err = st.CommitVCardResourceEnvelopeContext(
		ctx, "book", "projection-conflict", created.Revision,
		snapshot.Fingerprint, prepared,
	)
	require.ErrorIs(err, store.ErrVCardProjectionConflict)
	var conflict *store.VCardProjectionConflictError
	require.ErrorAs(err, &conflict)
	assert.Equal(person.ID, conflict.PersonID)
	wantFingerprint := snapshot.Fingerprint
	assert.Equal(wantFingerprint, conflict.Expected)
	assert.NotEqual(conflict.Expected, conflict.Actual)

	loaded, err := st.GetVCardResourceEnvelopeContext(
		ctx, "book", "projection-conflict",
	)
	require.NoError(err)
	assert.Equal(created.Revision, loaded.Revision)
	assert.Equal(raw, loaded.StoredBody)
	assert.Equal(vcard.ETagForBody(raw), loaded.ETag)
}
