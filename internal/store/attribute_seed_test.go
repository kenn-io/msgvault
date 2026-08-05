package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInitSchemaSeedsExactlyTheFourSystemDefinitions(t *testing.T) {
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
	}, slugs, "seeding must be minimal and display-ordered")

	organization, err := st.ListAttributeDefinitionsContext(ctx,
		store.AttributeDefinitionFilter{ObjectType: store.AttributeObjectOrganization})
	require.NoError(err)
	assert.Empty(organization)
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
		UPDATE attribute_definitions SET field_type = 'textarea' WHERE universal_id = ?
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
	assert.Greater(reseeded.Revision, renamed.Revision)

	require.NoError(st.EnsureSeededAttributeDefinitions())
	idempotent, err := st.GetAttributeDefinitionBySlugContext(ctx,
		store.AttributeObjectPerson, store.AttributeSlugAskMeAbout)
	require.NoError(err)
	assert.Equal(reseeded.Revision, idempotent.Revision)

	all, err := st.ListAttributeDefinitionsContext(ctx,
		store.AttributeDefinitionFilter{ObjectType: store.AttributeObjectPerson})
	require.NoError(err)
	assert.Len(all, 4)
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
