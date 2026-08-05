package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func personTextDefinition(slug string) store.AttributeDefinitionInput {
	return store.AttributeDefinitionInput{
		UniversalID: "test-" + slug,
		ObjectType:  store.AttributeObjectPerson,
		Slug:        slug,
		Label:       "Test " + slug,
		ValueType:   store.AttributeValueText,
		FieldType:   store.AttributeFieldText,
		Cardinality: store.AttributeCardinalitySingle,
		Ownership:   store.AttributeOwnershipUser,
		UICreatable: true,
		UIEditable:  true,
		APIMutable:  true,
		IsAudited:   true,
		IsDeletable: true,
	}
}

func onlyTestDefinitions(
	definitions []store.AttributeDefinition, slugs ...string,
) []store.AttributeDefinition {
	wanted := make(map[string]bool, len(slugs))
	for _, slug := range slugs {
		wanted[slug] = true
	}
	filtered := make([]store.AttributeDefinition, 0, len(slugs))
	for _, definition := range definitions {
		if wanted[definition.Slug] {
			filtered = append(filtered, definition)
		}
	}
	return filtered
}

func TestCreateAttributeDefinitionRejectsInvalidVocabularies(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name    string
		mutate  func(*store.AttributeDefinitionInput)
		wantMsg string
	}{
		{
			name:    "unknown object type",
			mutate:  func(in *store.AttributeDefinitionInput) { in.ObjectType = "household" },
			wantMsg: "object_type",
		},
		{
			name:    "unknown value type",
			mutate:  func(in *store.AttributeDefinitionInput) { in.ValueType = "blob" },
			wantMsg: "value_type",
		},
		{
			name:    "unknown field type",
			mutate:  func(in *store.AttributeDefinitionInput) { in.FieldType = "slider" },
			wantMsg: "field_type",
		},
		{
			name:    "unknown cardinality",
			mutate:  func(in *store.AttributeDefinitionInput) { in.Cardinality = "many" },
			wantMsg: "cardinality",
		},
		{
			name:    "unknown ownership",
			mutate:  func(in *store.AttributeDefinitionInput) { in.Ownership = "vendor" },
			wantMsg: "ownership",
		},
		{
			name:    "uppercase slug",
			mutate:  func(in *store.AttributeDefinitionInput) { in.Slug = "Ask_Me_About" },
			wantMsg: "slug",
		},
		{
			name:    "path-unsafe slug",
			mutate:  func(in *store.AttributeDefinitionInput) { in.Slug = "ask/me" },
			wantMsg: "slug",
		},
		{
			name:    "empty label",
			mutate:  func(in *store.AttributeDefinitionInput) { in.Label = "   " },
			wantMsg: "label",
		},
		{
			name: "record reference without target",
			mutate: func(in *store.AttributeDefinitionInput) {
				in.ValueType = store.AttributeValueRecordReference
				in.FieldType = store.AttributeFieldPerson
				in.RecordTarget = nil
			},
			wantMsg: "record_target",
		},
		{
			name: "organization widget with person target",
			mutate: func(in *store.AttributeDefinitionInput) {
				target := "person"
				in.ValueType = store.AttributeValueRecordReference
				in.FieldType = store.AttributeFieldOrganization
				in.RecordTarget = &target
			},
			wantMsg: "field_type organization requires record_target organization",
		},
		{
			name: "record target on a scalar value type",
			mutate: func(in *store.AttributeDefinitionInput) {
				target := "person"
				in.RecordTarget = &target
			},
			wantMsg: "record_target",
		},
		{
			name: "select widget without choices",
			mutate: func(in *store.AttributeDefinitionInput) {
				in.FieldType = store.AttributeFieldSelect
			},
			wantMsg: "options.choices",
		},
		{
			name: "multiselect widget on a single-cardinality definition",
			mutate: func(in *store.AttributeDefinitionInput) {
				in.FieldType = store.AttributeFieldMultiselect
				in.Options = &store.AttributeOptions{
					Choices: []store.AttributeChoice{{Value: "a", Label: "A"}},
				}
				in.Cardinality = store.AttributeCardinalitySingle
			},
			wantMsg: "multiselect requires cardinality multi",
		},
		{
			name: "checkbox widget on a text value type",
			mutate: func(in *store.AttributeDefinitionInput) {
				in.FieldType = store.AttributeFieldCheckbox
			},
			wantMsg: "field_type checkbox requires value_type boolean",
		},
		{
			name: "lowercase vCard property",
			mutate: func(in *store.AttributeDefinitionInput) {
				property := "x-custom"
				in.VCardProperty = &property
			},
			wantMsg: "vcard_property",
		},
		{
			name:    "negative display order",
			mutate:  func(in *store.AttributeDefinitionInput) { in.DisplayOrder = -1 },
			wantMsg: "display_order",
		},
		{
			name: "duplicate choice values",
			mutate: func(in *store.AttributeDefinitionInput) {
				in.FieldType = store.AttributeFieldSelect
				in.Options = &store.AttributeOptions{Choices: []store.AttributeChoice{
					{Value: "a", Label: "A"},
					{Value: "a", Label: "Also A"},
				}}
			},
			wantMsg: "duplicate",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			input := personTextDefinition("candidate")
			test.mutate(&input)
			_, err := st.CreateAttributeDefinitionContext(ctx, input)
			require.ErrorIs(err, store.ErrAttributeDefinitionInvalid)
			assert.ErrorContains(err, test.wantMsg)
		})
	}
}

func TestCreateAttributeDefinitionRejectsSelectTypesWithoutCanonicalChoices(
	t *testing.T,
) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name      string
		fieldType store.AttributeFieldType
		valueType store.AttributeValueType
	}{
		{
			name:      "select json",
			fieldType: store.AttributeFieldSelect,
			valueType: store.AttributeValueJSON,
		},
		{
			name:      "multiselect record reference",
			fieldType: store.AttributeFieldMultiselect,
			valueType: store.AttributeValueRecordReference,
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := personTextDefinition("unsupported_choice_type_" + string(rune('a'+i)))
			input.FieldType = test.fieldType
			input.ValueType = test.valueType
			input.Cardinality = store.AttributeCardinalityMulti
			input.Options = &store.AttributeOptions{Choices: []store.AttributeChoice{
				{Value: "synthetic", Label: "Synthetic"},
			}}
			if test.valueType == store.AttributeValueRecordReference {
				input.RecordTarget = new("person")
			}

			_, err := st.CreateAttributeDefinitionContext(ctx, input)
			require.ErrorIs(t, err, store.ErrAttributeDefinitionInvalid)
			assert.ErrorContains(t, err, "has no canonical string form")
		})
	}
}

func TestCreateAttributeDefinitionCanonicalizesChoicesForValueType(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name      string
		valueType store.AttributeValueType
		raw       string
		want      string
	}{
		{name: "text", valueType: store.AttributeValueText, raw: " synthetic ", want: "synthetic"},
		{name: "integer", valueType: store.AttributeValueInteger, raw: "+007", want: "7"},
		{name: "real", valueType: store.AttributeValueReal, raw: "1.500", want: "1.5"},
		{name: "boolean", valueType: store.AttributeValueBoolean, raw: "TRUE", want: "true"},
		{name: "date", valueType: store.AttributeValueDate, raw: " 2026-07-30 ", want: "2026-07-30"},
		{
			name:      "timestamp",
			valueType: store.AttributeValueTimestamp,
			raw:       "2026-07-30T10:00:00+02:00",
			want:      "2026-07-30T08:00:00Z",
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			input := personTextDefinition("canonical_" + test.name)
			input.UniversalID = "test-canonical-" + test.name
			input.ValueType = test.valueType
			input.FieldType = store.AttributeFieldSelect
			input.DisplayOrder = int64(i)
			input.Options = &store.AttributeOptions{Choices: []store.AttributeChoice{
				{Value: test.raw, Label: " Synthetic label "},
			}}

			created, err := st.CreateAttributeDefinitionContext(ctx, input)
			require.NoError(err)
			require.NotNil(created.Options)
			require.Len(created.Options.Choices, 1)
			assert.Equal(test.want, created.Options.Choices[0].Value)
			assert.Equal("Synthetic label", created.Options.Choices[0].Label)
		})
	}
}

func TestCreateAttributeDefinitionRejectsInvalidAndDuplicateCanonicalChoices(
	t *testing.T,
) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name      string
		valueType store.AttributeValueType
		choices   []store.AttributeChoice
		wantMsg   string
	}{
		{
			name:      "invalid integer",
			valueType: store.AttributeValueInteger,
			choices:   []store.AttributeChoice{{Value: "seven", Label: "Seven"}},
			wantMsg:   "must be an integer",
		},
		{
			name:      "invalid real",
			valueType: store.AttributeValueReal,
			choices:   []store.AttributeChoice{{Value: "many", Label: "Many"}},
			wantMsg:   "must be a number",
		},
		{
			name:      "invalid boolean",
			valueType: store.AttributeValueBoolean,
			choices:   []store.AttributeChoice{{Value: "sometimes", Label: "Sometimes"}},
			wantMsg:   "must be a boolean",
		},
		{
			name:      "invalid date",
			valueType: store.AttributeValueDate,
			choices:   []store.AttributeChoice{{Value: "2026-02-30", Label: "Invalid date"}},
			wantMsg:   "YYYY-MM-DD calendar date",
		},
		{
			name:      "invalid timestamp",
			valueType: store.AttributeValueTimestamp,
			choices:   []store.AttributeChoice{{Value: "tomorrow", Label: "Tomorrow"}},
			wantMsg:   "RFC3339",
		},
		{
			name:      "duplicate canonical integer",
			valueType: store.AttributeValueInteger,
			choices: []store.AttributeChoice{
				{Value: "01", Label: "One"},
				{Value: "1", Label: "Also one"},
			},
			wantMsg: "duplicate",
		},
	}

	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := personTextDefinition("invalid_choice_" + string(rune('a'+i)))
			input.UniversalID = "test-invalid-choice-" + test.name
			input.ValueType = test.valueType
			input.FieldType = store.AttributeFieldSelect
			input.Options = &store.AttributeOptions{Choices: test.choices}

			_, err := st.CreateAttributeDefinitionContext(ctx, input)
			require.ErrorIs(t, err, store.ErrAttributeDefinitionInvalid)
			assert.ErrorContains(t, err, test.wantMsg)
		})
	}
}

func TestCreateAttributeDefinitionStoresANovelUserDefinition(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	input := personTextDefinition("favorite_tea")
	input.FieldType = store.AttributeFieldSelect
	input.Cardinality = store.AttributeCardinalityMulti
	input.Options = &store.AttributeOptions{Choices: []store.AttributeChoice{
		{Value: "green", Label: "Green"},
		{Value: "oolong", Label: "Oolong"},
	}}
	input.IsSearchable = true
	input.DisplayOrder = 100

	created, err := st.CreateAttributeDefinitionContext(ctx, input)
	require.NoError(err)
	assert.Positive(created.ID)
	assert.Equal("favorite_tea", created.Slug)
	assert.Equal("test-favorite_tea", created.UniversalID)
	assert.Equal(store.AttributeObjectPerson, created.ObjectType)
	assert.Equal(store.AttributeCardinalityMulti, created.Cardinality)
	assert.Equal(store.AttributeOwnershipUser, created.Ownership)
	assert.Equal(int64(1), created.Revision)
	assert.True(created.IsActive)
	require.NotNil(created.Options)
	assert.Equal([]string{"green", "oolong"}, created.Options.ChoiceValues())

	fetched, err := st.GetAttributeDefinitionBySlugContext(
		ctx, store.AttributeObjectPerson, "favorite_tea")
	require.NoError(err)
	assert.Equal(*created, *fetched)
}

func TestCreateAttributeDefinitionAllowsOrganizationObjectTypeWithoutAValuePath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	input := personTextDefinition("industry")
	input.UniversalID = "test-org-industry"
	input.ObjectType = store.AttributeObjectOrganization

	created, err := st.CreateAttributeDefinitionContext(ctx, input)
	require.NoError(err)
	assert.Equal(store.AttributeObjectOrganization, created.ObjectType)

	personScoped := personTextDefinition("industry")
	personScoped.UniversalID = "test-person-industry"
	_, err = st.CreateAttributeDefinitionContext(ctx, personScoped)
	require.NoError(err)

	listed, err := st.ListAttributeDefinitionsContext(ctx, store.AttributeDefinitionFilter{
		ObjectType: store.AttributeObjectOrganization,
	})
	require.NoError(err)
	require.Len(listed, 1)
	assert.Equal("industry", listed[0].Slug)
}

func TestCreateAttributeDefinitionRejectsDuplicateSlugAndUniversalID(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	_, err := st.CreateAttributeDefinitionContext(ctx, personTextDefinition("nickname_note"))
	require.NoError(t, err)

	sameSlug := personTextDefinition("nickname_note")
	sameSlug.UniversalID = "test-other-universal-id"
	_, err = st.CreateAttributeDefinitionContext(ctx, sameSlug)
	require.ErrorIs(t, err, store.ErrAttributeDefinitionSlugConflict)

	sameUniversalID := personTextDefinition("other_slug")
	sameUniversalID.UniversalID = "test-nickname_note"
	_, err = st.CreateAttributeDefinitionContext(ctx, sameUniversalID)
	require.ErrorIs(t, err, store.ErrAttributeDefinitionUniversalIDConflict)
}

func TestUpdateAttributeDefinitionRenamesLabelAndKeepsIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	created, err := st.CreateAttributeDefinitionContext(
		ctx, personTextDefinition("cadence_note"))
	require.NoError(err)

	label := "Renamed cadence note"
	description := "How often to follow up"
	descriptionPtr := &description
	updated, err := st.UpdateAttributeDefinitionContext(ctx, created.ID, created.Revision,
		store.AttributeDefinitionUpdate{Label: &label, Description: &descriptionPtr})
	require.NoError(err)
	assert.Equal(label, updated.Label)
	require.NotNil(updated.Description)
	assert.Equal(description, *updated.Description)
	assert.Equal(created.UniversalID, updated.UniversalID)
	assert.Equal(created.Slug, updated.Slug)
	assert.Equal(created.Revision+1, updated.Revision)

	_, err = st.UpdateAttributeDefinitionContext(ctx, created.ID, created.Revision,
		store.AttributeDefinitionUpdate{Label: &label})
	require.ErrorIs(err, store.ErrAttributeDefinitionRevisionConflict)

	var nilDescription *string
	cleared, err := st.UpdateAttributeDefinitionContext(ctx, created.ID, updated.Revision,
		store.AttributeDefinitionUpdate{Description: &nilDescription})
	require.NoError(err)
	assert.Nil(cleared.Description)

	_, err = st.UpdateAttributeDefinitionContext(ctx, 999_999, 1,
		store.AttributeDefinitionUpdate{Label: &label})
	require.ErrorIs(err, store.ErrAttributeDefinitionNotFound)
}

func TestDeleteAttributeDefinitionRefusesUndeletableAndAllowsUnused(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	protected := personTextDefinition("protected_note")
	protected.IsDeletable = false
	created, err := st.CreateAttributeDefinitionContext(ctx, protected)
	require.NoError(err)
	require.ErrorIs(
		st.DeleteAttributeDefinitionContext(ctx, created.ID, created.Revision),
		store.ErrAttributeDefinitionNotDeletable,
	)

	deletable, err := st.CreateAttributeDefinitionContext(
		ctx, personTextDefinition("scratch_note"))
	require.NoError(err)
	require.NoError(st.DeleteAttributeDefinitionContext(
		ctx, deletable.ID, deletable.Revision))
	_, err = st.GetAttributeDefinitionContext(ctx, deletable.ID)
	require.ErrorIs(err, store.ErrAttributeDefinitionNotFound)
}

func TestListAttributeDefinitionsOrdersByDisplayOrderAndHidesInactiveByDefault(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	first := personTextDefinition("zzz_first")
	first.DisplayOrder = 1
	second := personTextDefinition("aaa_second")
	second.UniversalID = "test-aaa_second"
	second.DisplayOrder = 2
	createdFirst, err := st.CreateAttributeDefinitionContext(ctx, first)
	require.NoError(err)
	_, err = st.CreateAttributeDefinitionContext(ctx, second)
	require.NoError(err)

	listed, err := st.ListAttributeDefinitionsContext(ctx, store.AttributeDefinitionFilter{
		ObjectType: store.AttributeObjectPerson,
	})
	require.NoError(err)
	mine := onlyTestDefinitions(listed, "zzz_first", "aaa_second")
	require.Len(mine, 2)
	assert.Equal("zzz_first", mine[0].Slug, "display_order wins over slug ordering")

	inactive := false
	_, err = st.UpdateAttributeDefinitionContext(ctx, createdFirst.ID, createdFirst.Revision,
		store.AttributeDefinitionUpdate{IsActive: &inactive})
	require.NoError(err)

	active, err := st.ListAttributeDefinitionsContext(ctx, store.AttributeDefinitionFilter{
		ObjectType: store.AttributeObjectPerson,
	})
	require.NoError(err)
	activeMine := onlyTestDefinitions(active, "zzz_first", "aaa_second")
	require.Len(activeMine, 1)
	assert.Equal("aaa_second", activeMine[0].Slug)

	all, err := st.ListAttributeDefinitionsContext(ctx, store.AttributeDefinitionFilter{
		ObjectType: store.AttributeObjectPerson, IncludeHidden: true,
	})
	require.NoError(err)
	assert.Len(onlyTestDefinitions(all, "zzz_first", "aaa_second"), 2)
}
