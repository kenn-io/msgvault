package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInitSchemaSeedsTheSystemPersonCatalog(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	definitions, err := st.ListAttributeDefinitionsContext(ctx,
		store.AttributeDefinitionFilter{ObjectType: store.AttributeObjectPerson})
	require.NoError(err)

	slugs := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		slugs = append(slugs, definition.Slug)
		assert.Equal(store.AttributeOwnershipSystem, definition.Ownership,
			"seeded definition %s must be system-owned", definition.Slug)
		assert.False(definition.IsDeletable,
			"seeded definition %s must not be deletable", definition.Slug)
	}
	assert.Equal([]string{
		store.AttributeSlugPrimaryChannel,
		store.AttributeSlugContactFrequency,
		store.AttributeSlugAskMeAbout,
		store.AttributeSlugLastContacted,
		store.AttributeSlugNotes,
		"location", "birthplace", "membership", "religion", "politics",
		"personality", "family_pets", "interests_fun_now",
		"interests_fun_growing_up", "favorites_food", "favorites_place",
	}, slugs, "seeding must be minimal and display-ordered")

	organization, err := st.ListAttributeDefinitionsContext(ctx,
		store.AttributeDefinitionFilter{ObjectType: store.AttributeObjectOrganization})
	require.NoError(err)
	assert.Empty(organization)
}

func TestSeededAttributeDefinitionsIncludeExpandedPersonCatalog(t *testing.T) {
	tests := []struct {
		slug        string
		universalID string
		label       string
		cardinality store.AttributeCardinality
		sensitive   bool
		searchable  bool
	}{
		{"location", "2068efb0-9808-498b-ac3a-8e0a87d4513a", "Location", store.AttributeCardinalitySingle, false, true},
		{"birthplace", "cd5aaad0-4368-4686-85e1-4dbdb86cc54b", "Born in", store.AttributeCardinalitySingle, false, true},
		{"membership", "fbf748ac-585a-4f79-aac3-f7eda023b5e4", "Membership", store.AttributeCardinalityMulti, false, true},
		{"religion", "4425dff2-5da8-4398-8d91-9ec54defa1c0", "Religion", store.AttributeCardinalitySingle, true, false},
		{"politics", "f897f0b4-45fa-469a-97d5-c7d98517a50e", "Politics", store.AttributeCardinalitySingle, true, false},
		{"personality", "64142a35-ab0f-43cc-9828-87aaa362c693", "Personality", store.AttributeCardinalityMulti, true, false},
		{"family_pets", "e0775bae-cf07-4d06-9cc2-cb09a5358d82", "Pets", store.AttributeCardinalityMulti, false, true},
		{"interests_fun_now", "e8c815d5-3568-429b-a93b-00220b0a1b43", "Fun now", store.AttributeCardinalityMulti, false, true},
		{"interests_fun_growing_up", "7ffd6a23-74cc-4e05-ad6b-7d765d883a94", "Fun growing up", store.AttributeCardinalityMulti, false, true},
		{"favorites_food", "12612daa-90b0-461b-b928-79d4a2bc07ba", "Favorite food", store.AttributeCardinalityMulti, false, true},
		{"favorites_place", "be2bccc1-c395-4bb4-8f68-02e745ea7b22", "Favorite place", store.AttributeCardinalityMulti, false, true},
	}

	st := testutil.NewTestStore(t)
	seenIDs := make(map[string]struct{}, len(tests))
	for _, test := range tests {
		t.Run(test.slug, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			definition, err := st.GetAttributeDefinitionBySlugContext(
				t.Context(), store.AttributeObjectPerson, test.slug)
			require.NoError(err)
			assert.Equal(test.universalID, definition.UniversalID)
			assert.Equal(test.label, definition.Label)
			assert.Equal(store.AttributeValueText, definition.ValueType)
			assert.Equal(store.AttributeFieldText, definition.FieldType)
			assert.Equal(test.cardinality, definition.Cardinality)
			assert.Equal(test.sensitive, definition.IsSensitive)
			assert.Equal(test.searchable, definition.IsSearchable)
			assert.Equal(store.AttributeOwnershipSystem, definition.Ownership)
			assert.True(definition.UICreatable)
			assert.True(definition.UIEditable)
			assert.True(definition.APIMutable)
			assert.True(definition.IsAudited)
			assert.False(definition.IsDeletable)
			assert.True(definition.IsActive)
			require.NotNil(definition.Options)
			assert.Equal(120, definition.Options.MaxLength)
			_, duplicate := seenIDs[definition.UniversalID]
			assert.False(duplicate)
			seenIDs[definition.UniversalID] = struct{}{}
		})
	}
}

func TestSeededAttributeDefinitionsIncludeNotes(t *testing.T) {
	notes := definitionBySlug(t, store.SeededAttributeDefinitions(), store.AttributeSlugNotes)
	assert.Equal(t, "b72b3cf7-509f-4286-a0f0-bb039c85ff40", notes.UniversalID)
	assert.Equal(t, store.AttributeValueText, notes.ValueType)
	assert.Equal(t, store.AttributeFieldTextarea, notes.FieldType)
	assert.Equal(t, store.AttributeCardinalitySingle, notes.Cardinality)
	assert.Equal(t, store.AttributeOwnershipSystem, notes.Ownership)
	assert.True(t, notes.UICreatable)
	assert.True(t, notes.UIEditable)
	assert.True(t, notes.APIMutable)
	assert.True(t, notes.IsSensitive)
	assert.False(t, notes.IsSearchable)
	assert.True(t, notes.IsAudited)
	assert.False(t, notes.IsDeletable)
	require.NotNil(t, notes.VCardProperty)
	assert.Equal(t, "NOTE", *notes.VCardProperty)
}

func definitionBySlug(
	t *testing.T, definitions []store.AttributeDefinitionInput, slug string,
) store.AttributeDefinitionInput {
	t.Helper()
	for _, definition := range definitions {
		if definition.Slug == slug {
			return definition
		}
	}
	require.FailNow(t, "seeded attribute definition not found", "slug=%s", slug)
	return store.AttributeDefinitionInput{}
}

func TestSeededDefinitionsCarryTheirDocumentedShape(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	primary, err := st.GetAttributeDefinitionBySlugContext(ctx,
		store.AttributeObjectPerson, store.AttributeSlugPrimaryChannel)
	require.NoError(err)
	assert.Equal(store.AttributeUniversalIDPrimaryChannel, primary.UniversalID)
	assert.Equal(store.AttributeValueText, primary.ValueType)
	assert.Equal(store.AttributeFieldSelect, primary.FieldType)
	assert.Equal(store.AttributeCardinalitySingle, primary.Cardinality)
	require.NotNil(primary.Options)
	assert.Equal([]string{"email", "phone", "sms", "chat", "in_person"},
		primary.Options.ChoiceValues())
	assert.True(primary.APIMutable)

	frequency, err := st.GetAttributeDefinitionBySlugContext(ctx,
		store.AttributeObjectPerson, store.AttributeSlugContactFrequency)
	require.NoError(err)
	assert.Equal(store.AttributeUniversalIDContactFrequency, frequency.UniversalID)
	assert.Equal(store.AttributeValueInteger, frequency.ValueType)
	assert.Equal(store.AttributeFieldDuration, frequency.FieldType)
	require.NotNil(frequency.Options)
	assert.Equal("days", frequency.Options.Unit)

	askMeAbout, err := st.GetAttributeDefinitionBySlugContext(ctx,
		store.AttributeObjectPerson, store.AttributeSlugAskMeAbout)
	require.NoError(err)
	assert.Equal(store.AttributeUniversalIDAskMeAbout, askMeAbout.UniversalID)
	assert.Equal(store.AttributeValueText, askMeAbout.ValueType)
	assert.Equal(store.AttributeCardinalityMulti, askMeAbout.Cardinality)
	assert.True(askMeAbout.IsSearchable)
	require.NotNil(askMeAbout.Options)
	assert.Equal(120, askMeAbout.Options.MaxLength)
}

func TestSeededLastContactedIsReadOnlyAndDerived(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	derived, err := st.GetAttributeDefinitionBySlugContext(ctx,
		store.AttributeObjectPerson, store.AttributeSlugLastContacted)
	require.NoError(err)
	assert.Equal(store.AttributeUniversalIDLastContacted, derived.UniversalID)
	assert.Equal(store.AttributeValueTimestamp, derived.ValueType)
	require.NotNil(derived.DerivedSource)
	assert.Equal(store.AttributeDerivedSourceActivitySpine, *derived.DerivedSource)
	assert.False(derived.APIMutable)
	assert.False(derived.UICreatable)
	assert.False(derived.UIEditable)
	assert.True(derived.HistoryExempt)
}

func TestReSeedingPreservesUserLabelChangesAndRepairsStructure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	original, err := st.GetAttributeDefinitionBySlugContext(ctx,
		store.AttributeObjectPerson, store.AttributeSlugAskMeAbout)
	require.NoError(err)

	label := "Conversation starters"
	description := "Topics this person enjoys"
	descriptionPtr := &description
	displayOrder := int64(5)
	renamed, err := st.UpdateAttributeDefinitionContext(ctx, original.ID, original.Revision,
		store.AttributeDefinitionUpdate{
			Label: &label, Description: &descriptionPtr, DisplayOrder: &displayOrder})
	require.NoError(err)

	_, err = st.DB().Exec(st.Rebind(`
		UPDATE attribute_definitions
		SET field_type = 'textarea', is_sensitive = TRUE
		WHERE universal_id = ?
	`), store.AttributeUniversalIDAskMeAbout)
	require.NoError(err)

	require.NoError(st.EnsureSeededAttributeDefinitions())

	reseeded, err := st.GetAttributeDefinitionBySlugContext(ctx,
		store.AttributeObjectPerson, store.AttributeSlugAskMeAbout)
	require.NoError(err)
	assert.Equal(label, reseeded.Label)
	require.NotNil(reseeded.Description)
	assert.Equal(description, *reseeded.Description)
	assert.Equal(displayOrder, reseeded.DisplayOrder,
		"reseeding must preserve a user's display_order override")
	assert.Equal(original.UniversalID, reseeded.UniversalID)
	assert.Equal(original.Slug, reseeded.Slug)
	assert.Equal(store.AttributeFieldText, reseeded.FieldType)
	assert.False(reseeded.IsSensitive)
	assert.Greater(reseeded.Revision, renamed.Revision)

	require.NoError(st.EnsureSeededAttributeDefinitions())
	idempotent, err := st.GetAttributeDefinitionBySlugContext(ctx,
		store.AttributeObjectPerson, store.AttributeSlugAskMeAbout)
	require.NoError(err)
	assert.Equal(reseeded.Revision, idempotent.Revision)

	all, err := st.ListAttributeDefinitionsContext(ctx,
		store.AttributeDefinitionFilter{ObjectType: store.AttributeObjectPerson})
	require.NoError(err)
	assert.Len(all, len(store.SeededAttributeDefinitions()))
}

func TestReSeedingRefusesValueTypeRepairWhenValuesExist(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	participantID, err := st.EnsureParticipantByIdentifier(
		"email", "seed-type-guard@example.invalid", "Seed Type Guard")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(err)
	value := "distributed systems"
	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person.ID, DefinitionSlug: store.AttributeSlugAskMeAbout,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &value},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE attribute_definitions
		SET value_type = 'record_reference', field_type = 'person', record_target = 'person'
		WHERE universal_id = ?
	`), store.AttributeUniversalIDAskMeAbout)
	require.NoError(err)

	err = st.EnsureSeededAttributeDefinitionsContext(ctx)
	require.ErrorContains(err, "value_type")
	require.ErrorContains(err, "has existing values")
	drifted, getErr := st.GetAttributeDefinitionBySlugContext(
		ctx, store.AttributeObjectPerson, store.AttributeSlugAskMeAbout)
	require.NoError(getErr)
	assert.Equal(t, store.AttributeValueRecordReference, drifted.ValueType)
}

func TestInitSchemaPreservesLegacySeedSlugCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	_, err := st.DB().Exec(st.Rebind(`
		DELETE FROM attribute_definitions
		WHERE universal_id = ?
	`), store.AttributeUniversalIDLocation)
	require.NoError(err)

	legacy := definitionBySlug(
		t, store.SeededAttributeDefinitions(), store.AttributeSlugLocation,
	)
	legacy.UniversalID = "994e8d78-4711-42ec-9801-e3348e6fd133"
	legacy.Label = "Legacy location notes"
	legacy.FieldType = store.AttributeFieldTextarea
	legacy.Cardinality = store.AttributeCardinalityMulti
	legacy.IsSensitive = true
	legacy.IsSearchable = false
	legacy.Ownership = store.AttributeOwnershipUser
	legacy.IsDeletable = true
	legacy.Options = &store.AttributeOptions{MaxLength: 240}
	created, err := st.CreateAttributeDefinitionContext(ctx, legacy)
	require.NoError(err)
	participantID, err := st.EnsureParticipantByIdentifier(
		"email", "legacy-location@example.test", "Legacy Location")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(err)
	legacyValue := "Old town"
	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{
		PersonID: person.ID, DefinitionSlug: legacy.Slug,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &legacyValue},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)

	require.NoError(st.InitSchema())

	preserved, err := st.GetAttributeDefinitionBySlugContext(
		ctx, store.AttributeObjectPerson, store.AttributeSlugLocation)
	require.NoError(err)
	assert.Equal(created.ID, preserved.ID)
	assert.Equal(created.UniversalID, preserved.UniversalID)
	assert.Equal(legacy.Label, preserved.Label)
	assert.Equal(legacy.FieldType, preserved.FieldType)
	assert.Equal(legacy.Cardinality, preserved.Cardinality)
	assert.Equal(legacy.IsSensitive, preserved.IsSensitive)
	assert.Equal(store.AttributeOwnershipUser, preserved.Ownership)
	require.NotNil(preserved.Options)
	assert.Equal(240, preserved.Options.MaxLength)

	values, err := st.ListPersonAttributeValuesContext(ctx, person.ID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugLocation})
	require.NoError(err)
	require.Len(values, 1)
	require.NotNil(values[0].Value.Text)
	assert.Equal(legacyValue, *values[0].Value.Text)

	definitions, err := st.ListAttributeDefinitionsContext(ctx,
		store.AttributeDefinitionFilter{ObjectType: store.AttributeObjectPerson})
	require.NoError(err)
	assert.Len(definitions, len(store.SeededAttributeDefinitions())+1)
	var canonical *store.AttributeDefinition
	for i := range definitions {
		if definitions[i].UniversalID == store.AttributeUniversalIDLocation {
			canonical = &definitions[i]
			break
		}
	}
	require.NotNil(canonical)
	assert.NotEqual(store.AttributeSlugLocation, canonical.Slug)
	assert.Equal(store.AttributeOwnershipSystem, canonical.Ownership)

	preservedRevision := preserved.Revision
	canonicalRevision := canonical.Revision
	require.NoError(st.InitSchema())
	preservedAgain, err := st.GetAttributeDefinitionBySlugContext(
		ctx, store.AttributeObjectPerson, store.AttributeSlugLocation)
	require.NoError(err)
	canonicalAgain, err := st.GetAttributeDefinitionBySlugContext(
		ctx, store.AttributeObjectPerson, canonical.Slug)
	require.NoError(err)
	assert.Equal(preservedRevision, preservedAgain.Revision)
	assert.Equal(canonicalRevision, canonicalAgain.Revision)
}

func TestInitSchemaUsesNextFallbackWhenSeedFallbackSlugIsOccupied(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()

	_, err := st.DB().Exec(st.Rebind(`
		DELETE FROM attribute_definitions WHERE universal_id = ?
	`), store.AttributeUniversalIDLocation)
	require.NoError(err)
	legacy := definitionBySlug(
		t, store.SeededAttributeDefinitions(), store.AttributeSlugLocation,
	)
	legacy.UniversalID = "994e8d78-4711-42ec-9801-e3348e6fd133"
	legacy.Ownership = store.AttributeOwnershipUser
	legacy.IsDeletable = true
	_, err = st.CreateAttributeDefinitionContext(ctx, legacy)
	require.NoError(err)
	fallback := store.AttributeSlugLocation + "_system_" +
		strings.ReplaceAll(store.AttributeUniversalIDLocation, "-", "")
	occupiedFallback := legacy
	occupiedFallback.UniversalID = "b5464a90-758a-4df1-9fb8-fdf74b1fc288"
	occupiedFallback.Slug = fallback
	_, err = st.CreateAttributeDefinitionContext(ctx, occupiedFallback)
	require.NoError(err)

	require.NoError(st.InitSchema())
	canonical, err := st.GetAttributeDefinitionBySlugContext(
		ctx, store.AttributeObjectPerson, fallback+"_2")
	require.NoError(err)
	assert.Equal(store.AttributeUniversalIDLocation, canonical.UniversalID)
	preserved, err := st.GetAttributeDefinitionBySlugContext(
		ctx, store.AttributeObjectPerson, store.AttributeSlugLocation)
	require.NoError(err)
	assert.Equal(legacy.UniversalID, preserved.UniversalID)
	occupied, err := st.GetAttributeDefinitionBySlugContext(
		ctx, store.AttributeObjectPerson, fallback)
	require.NoError(err)
	assert.Equal(occupiedFallback.UniversalID, occupied.UniversalID)
}

func TestInitSchemaPreservesExistingSeedSlugByUniversalID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)

	_, err := st.DB().Exec(st.Rebind(`
		UPDATE attribute_definitions
		SET slug = 'location_custom'
		WHERE universal_id = ?
	`), store.AttributeUniversalIDLocation)
	require.NoError(err)

	require.NoError(st.InitSchema())
	restored, err := st.GetAttributeDefinitionBySlugContext(
		t.Context(), store.AttributeObjectPerson, "location_custom")
	require.NoError(err)
	assert.Equal(store.AttributeUniversalIDLocation, restored.UniversalID)
	_, err = st.GetAttributeDefinitionBySlugContext(
		t.Context(), store.AttributeObjectPerson, store.AttributeSlugLocation)
	require.ErrorIs(err, store.ErrAttributeDefinitionNotFound)
}

func TestInitSchemaResolvesCombinedSeedSlugAndUniversalIDCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()

	_, err := st.DB().Exec(st.Rebind(`
		UPDATE attribute_definitions
		SET slug = 'location_custom'
		WHERE universal_id = ?
	`), store.AttributeUniversalIDLocation)
	require.NoError(err)
	legacy := definitionBySlug(
		t, store.SeededAttributeDefinitions(), store.AttributeSlugLocation,
	)
	legacy.UniversalID = "994e8d78-4711-42ec-9801-e3348e6fd133"
	legacy.Ownership = store.AttributeOwnershipUser
	legacy.IsDeletable = true
	legacyDefinition, err := st.CreateAttributeDefinitionContext(ctx, legacy)
	require.NoError(err)

	require.NoError(st.InitSchema())
	canonical, err := st.GetAttributeDefinitionBySlugContext(
		ctx, store.AttributeObjectPerson, "location_custom")
	require.NoError(err)
	assert.Equal(store.AttributeUniversalIDLocation, canonical.UniversalID)
	preserved, err := st.GetAttributeDefinitionBySlugContext(
		ctx, store.AttributeObjectPerson, store.AttributeSlugLocation)
	require.NoError(err)
	assert.Equal(legacyDefinition.UniversalID, preserved.UniversalID)
}

func TestInitSchemaRejectsSeedUniversalIDOnWrongObjectType(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)

	_, err := st.DB().Exec(st.Rebind(`
		UPDATE attribute_definitions
		SET object_type = 'organization', slug = 'location_custom'
		WHERE universal_id = ?
	`), store.AttributeUniversalIDLocation)
	require.NoError(err)

	err = st.InitSchema()
	require.Error(err)
	require.ErrorContains(err, "belongs to object type organization")
}

func TestSeededDefinitionsPassTheSameValidationAsUserDefinitions(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	for _, seed := range store.SeededAttributeDefinitions() {
		t.Run(seed.Slug, func(t *testing.T) {
			_, err := st.CreateAttributeDefinitionContext(ctx, seed)
			require.ErrorIs(t, err, store.ErrAttributeDefinitionUniversalIDConflict)
		})
	}
}

func TestEnsureSeededAttributeDefinitionsRetriesConcurrentMissingSeed(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)

	_, err := st.DB().Exec(st.Rebind(`
		DELETE FROM attribute_definitions WHERE universal_id = ?
	`), store.AttributeUniversalIDLocation)
	require.NoError(err)

	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	hook := func(slug string) {
		if slug != store.AttributeSlugLocation {
			return
		}
		ready <- struct{}{}
		<-release
	}
	defer st.SetAttributeSeedReadHookForTest(hook)()

	errs := make(chan error, 2)
	go func() { errs <- st.EnsureSeededAttributeDefinitionsContext(t.Context()) }()
	go func() { errs <- st.EnsureSeededAttributeDefinitionsContext(t.Context()) }()
	for range 2 {
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			close(release)
			require.FailNow("both seeders did not reach the missing-seed window")
		}
	}
	close(release)

	require.NoError(<-errs)
	require.NoError(<-errs)
	definition, err := st.GetAttributeDefinitionBySlugContext(
		t.Context(), store.AttributeObjectPerson, store.AttributeSlugLocation)
	require.NoError(err)
	assert.Equal(t, store.AttributeUniversalIDLocation, definition.UniversalID)
}

func TestEnsureSeededAttributeDefinitionsRetriesConcurrentFallbackSeed(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)

	_, err := st.DB().Exec(st.Rebind(`
		DELETE FROM attribute_definitions WHERE universal_id = ?
	`), store.AttributeUniversalIDLocation)
	require.NoError(err)
	legacy := definitionBySlug(
		t, store.SeededAttributeDefinitions(), store.AttributeSlugLocation,
	)
	legacy.UniversalID = "994e8d78-4711-42ec-9801-e3348e6fd133"
	legacy.Ownership = store.AttributeOwnershipUser
	legacy.IsDeletable = true
	_, err = st.CreateAttributeDefinitionContext(t.Context(), legacy)
	require.NoError(err)

	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	hook := func(slug string) {
		if slug != store.AttributeSlugLocation {
			return
		}
		ready <- struct{}{}
		<-release
	}
	defer st.SetAttributeSeedReadHookForTest(hook)()

	errs := make(chan error, 2)
	go func() { errs <- st.EnsureSeededAttributeDefinitionsContext(t.Context()) }()
	go func() { errs <- st.EnsureSeededAttributeDefinitionsContext(t.Context()) }()
	for range 2 {
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			close(release)
			require.FailNow("both seeders did not reach the fallback-seed window")
		}
	}
	close(release)

	require.NoError(<-errs)
	require.NoError(<-errs)
	preserved, err := st.GetAttributeDefinitionBySlugContext(
		t.Context(), store.AttributeObjectPerson, store.AttributeSlugLocation)
	require.NoError(err)
	assert.Equal(t, legacy.UniversalID, preserved.UniversalID)
	definitions, err := st.ListAttributeDefinitionsContext(t.Context(),
		store.AttributeDefinitionFilter{ObjectType: store.AttributeObjectPerson})
	require.NoError(err)
	var canonicalCount int
	for _, definition := range definitions {
		if definition.UniversalID == store.AttributeUniversalIDLocation {
			canonicalCount++
			assert.NotEqual(t, store.AttributeSlugLocation, definition.Slug)
		}
	}
	assert.Equal(t, 1, canonicalCount)
}
