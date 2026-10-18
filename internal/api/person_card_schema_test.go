package api

import (
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonCardSchemaPreservesArchiveTypes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	doc := OpenAPIDocument()
	schemas := doc.Components.Schemas.Map()
	requirements.NotNil(schemas["Person"])
	assertions.Contains(schemas["Person"].Properties, "participant_ids")
	assertions.NotContains(schemas["Person"].Properties, "untrusted_text")
	requirements.NotNil(schemas["PersonCardPerson"])
	assertions.Contains(schemas["PersonCardPerson"].Properties, "untrusted_text")
	assertions.Contains(schemas["PersonCardPerson"].Properties, "person_id")
	requirements.NotNil(schemas["PersonCardResult"])
	assertions.Contains(schemas["PersonCardResult"].Properties, "freshness")
	source := schemas["PersonCardSourceFreshness"]
	requirements.NotNil(source)
	assertions.Contains(source.Properties, "can_sync")
	assertions.Contains(source.Properties, "source_id")
}

func TestPersonCardNullableObjectSchemas(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	fields := map[string][]string{
		"PersonCardResult": {"card"},
		"PersonCard":       {"brief", "last_interaction"},
		"PersonCardBrief":  {"untrusted_text"},
	}
	public := OpenAPIDocument().Components.Schemas.Map()
	for name, names := range fields {
		for _, field := range names {
			property := public[name].Properties[field]
			requirements.NotNil(property)
			requirements.Len(property.OneOf, 2, "%s.%s permits null", name, field)
			assertions.NotEmpty(property.OneOf[0].Ref)
			assertions.Equal("null", property.OneOf[1].Type)
		}
	}
	projection, ok := public["PersonCardFreshnessBlock"].Properties["projections"].AdditionalProperties.(*huma.Schema)
	requirements.True(ok)
	requirements.Len(projection.OneOf, 2)
	assertions.Equal("null", projection.OneOf[1].Type)

	client := openAPIClientDocument().Components.Schemas.Map()
	for _, name := range []string{"PersonCard", "PersonCardBrief", "PersonCardLastInteraction", "PersonCardBriefText", "ProjectionFreshness"} {
		requirements.NotNil(client[name])
		assertions.Equal("object", client[name].Type, name)
		assertions.True(client[name].Nullable, name)
	}
	for name, names := range fields {
		for _, field := range names {
			assertions.Contains(client[name].Required, field)
			assertions.NotEmpty(client[name].Properties[field].Ref)
			assertions.False(client[name].Properties[field].Nullable, "nullability belongs to its referenced component")
		}
	}
}
