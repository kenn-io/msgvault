package personfacts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCatalogFiltersIneligibleAttributeDefinitions(t *testing.T) {
	definitions := []Definition{
		{UniversalID: "eligible", Slug: "eligible", Description: "Eligible field", ValueType: ValueText, Cardinality: CardinalitySingle, Active: true, APIMutable: true},
		{UniversalID: "inactive", Slug: "inactive", Description: "Inactive field", ValueType: ValueText, Cardinality: CardinalitySingle, APIMutable: true},
		{UniversalID: "immutable", Slug: "immutable", Description: "Immutable field", ValueType: ValueText, Cardinality: CardinalitySingle, Active: true},
		{UniversalID: "derived", Slug: "derived", Description: "Derived field", ValueType: ValueText, Cardinality: CardinalitySingle, Active: true, APIMutable: true, Derived: true},
		{UniversalID: "blank", Slug: "blank", Description: " \t", ValueType: ValueText, Cardinality: CardinalitySingle, Active: true, APIMutable: true},
		{UniversalID: "long", Slug: "long", Description: strings.Repeat("🙂", 281), ValueType: ValueText, Cardinality: CardinalitySingle, Active: true, APIMutable: true},
		{UniversalID: "json", Slug: "json", Description: "JSON field", ValueType: "json", Cardinality: CardinalitySingle, Active: true, APIMutable: true},
		{UniversalID: "reference", Slug: "reference", Description: "Reference field", ValueType: "record_reference", Cardinality: CardinalitySingle, RecordTarget: "person", Active: true, APIMutable: true},
		{UniversalID: "organization", Slug: "organization", Description: "Structured only", ValueType: ValueOrganization, Cardinality: CardinalitySingle, Active: true, APIMutable: true},
		{UniversalID: "partial_date", Slug: "partial_date", Description: "Structured only", ValueType: ValuePartialDate, Cardinality: CardinalitySingle, Active: true, APIMutable: true},
	}

	catalog, err := BuildCatalog(definitions, CatalogOptions{})
	require.NoError(t, err)
	require.Len(t, catalog.Targets, 2)
	assert.Equal(t, []string{"eligible", "system:employment"}, []string{
		catalog.Targets[0].Key,
		catalog.Targets[1].Key,
	})
}

func TestBuildCatalogIncludesSupportedGenericTypes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	definitions := []Definition{
		{UniversalID: "text", Slug: "text", Description: "Text", ValueType: ValueText, Cardinality: CardinalitySingle, Active: true, APIMutable: true},
		{UniversalID: "integer", Slug: "integer", Description: "Integer", ValueType: ValueInteger, Cardinality: CardinalityMulti, Active: true, APIMutable: true},
		{UniversalID: "real", Slug: "real", Description: "Real", ValueType: ValueReal, Cardinality: CardinalitySingle, Active: true, APIMutable: true},
		{UniversalID: "boolean", Slug: "boolean", Description: "Boolean", ValueType: ValueBoolean, Cardinality: CardinalitySingle, Active: true, APIMutable: true},
		{UniversalID: "date", Slug: "date", Description: "Date", ValueType: ValueDate, Cardinality: CardinalitySingle, Active: true, APIMutable: true},
		{UniversalID: "timestamp", Slug: "timestamp", Description: "Timestamp", ValueType: ValueTimestamp, Cardinality: CardinalitySingle, Active: true, APIMutable: true},
	}

	catalog, err := BuildCatalog(definitions, CatalogOptions{})
	require.NoError(err)
	require.Len(catalog.Targets, 7)
	assert.Equal([]ValueType{ValueBoolean, ValueDate, ValueInteger, ValueReal, ValueText, ValueTimestamp}, []ValueType{
		catalog.Targets[0].ValueType,
		catalog.Targets[1].ValueType,
		catalog.Targets[2].ValueType,
		catalog.Targets[3].ValueType,
		catalog.Targets[4].ValueType,
		catalog.Targets[5].ValueType,
	})
	assert.Equal(TargetEmployment, catalog.Targets[6].Kind)
}

func TestBuildCatalogCarriesChoiceValuesAndLabels(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	definitions := []Definition{{
		UniversalID: "status", Slug: "status", Description: "Relationship status",
		ValueType: ValueText, Cardinality: CardinalitySingle, Active: true, APIMutable: true,
		Choices: []ChoiceDescriptor{{Value: "z", Label: "Zulu"}, {Value: "a", Label: "Alpha"}},
	}}

	catalog, err := BuildCatalog(definitions, CatalogOptions{})
	require.NoError(err)
	require.Len(catalog.Targets, 2)
	assert.Equal([]ChoiceDescriptor{{Value: "a", Label: "Alpha"}, {Value: "z", Label: "Zulu"}}, catalog.Targets[0].Choices)
	assert.Equal([]ChoiceDescriptor{{Value: "z", Label: "Zulu"}, {Value: "a", Label: "Alpha"}}, definitions[0].Choices)
}

func TestDescriptorRevisionUsesInferenceContentOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	target := TargetDescriptor{
		Kind: TargetAttribute, Key: "preferred_language", UniversalID: "preferred_language",
		Slug: "preferred_language", Description: "Preferred language", ValueType: ValueText,
		Cardinality: CardinalitySingle, Choices: []ChoiceDescriptor{{Value: "fr", Label: "French"}},
	}

	first, err := DescriptorRevision(target)
	require.NoError(err)
	target.Revision = "previous-api-revision"
	second, err := DescriptorRevision(target)
	require.NoError(err)
	assert.Equal(first, second)

	target.Description = "Preferred language for communication"
	third, err := DescriptorRevision(target)
	require.NoError(err)
	assert.NotEqual(first, third)
}

func TestBuildCatalogMaxLengthChangesDescriptorAndCatalogIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	definition := Definition{
		UniversalID: "biography", Slug: "biography", Description: "Short biography",
		ValueType: ValueText, Cardinality: CardinalitySingle, MaxLength: 12,
		Active: true, APIMutable: true,
	}

	first, err := BuildCatalog([]Definition{definition}, CatalogOptions{})
	require.NoError(err)
	require.Len(first.Targets, 2)
	assert.Equal(12, first.Targets[0].MaxLength)

	definition.MaxLength = 24
	second, err := BuildCatalog([]Definition{definition}, CatalogOptions{})
	require.NoError(err)
	require.Len(second.Targets, 2)
	assert.NotEqual(first.Targets[0].Revision, second.Targets[0].Revision)
	assert.NotEqual(first.Fingerprint, second.Fingerprint)
}

func TestBuildCatalogEmploymentDescriptor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	catalog, err := BuildCatalog(nil, CatalogOptions{})
	require.NoError(err)
	require.Len(catalog.Targets, 1)

	employment := catalog.Targets[0]
	employment.Revision = ""
	assert.Equal(TargetDescriptor{
		Kind:        TargetEmployment,
		Key:         "system:employment",
		UniversalID: "system:employment",
		Slug:        "employment",
		Description: "Current and historical employment, including organization, title, role, department, location, and partial start and end dates",
		ValueType:   ValueEmployment,
		Cardinality: CardinalityMulti,
		Fields: []FieldDescriptor{
			{Name: "organization", ValueType: ValueOrganization, Cardinality: CardinalitySingle, Required: true},
			{Name: "title", ValueType: ValueText, Cardinality: CardinalitySingle},
			{Name: "role", ValueType: ValueText, Cardinality: CardinalitySingle},
			{Name: "department", ValueType: ValueText, Cardinality: CardinalitySingle},
			{Name: "location", ValueType: ValueText, Cardinality: CardinalitySingle},
			{Name: "start_date", ValueType: ValuePartialDate, Cardinality: CardinalitySingle},
			{Name: "end_date", ValueType: ValuePartialDate, Cardinality: CardinalitySingle},
		},
	}, employment)
	assert.NotEmpty(catalog.Targets[0].Revision)
}

func TestBuildCatalogSensitiveOption(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	definitions := []Definition{{
		UniversalID: "sensitive", Slug: "sensitive", Description: "Sensitive detail",
		ValueType: ValueText, Cardinality: CardinalitySingle, Sensitive: true, Active: true, APIMutable: true,
	}}

	withoutSensitive, err := BuildCatalog(definitions, CatalogOptions{})
	require.NoError(err)
	assert.Len(withoutSensitive.Targets, 1)

	withSensitive, err := BuildCatalog(definitions, CatalogOptions{IncludeSensitive: true})
	require.NoError(err)
	require.Len(withSensitive.Targets, 2)
	assert.True(withSensitive.Targets[0].Sensitive)
}
