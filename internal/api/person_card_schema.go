package api

import (
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// Huma's pointer-to-object schemas use a plain reference. These card fields
// intentionally remain present when absent, so their schemas must permit null.
func hardenPersonCardSchemas(doc *huma.OpenAPI, client bool) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	schemas := doc.Components.Schemas.Map()
	for name, fields := range map[string][]string{
		"PersonCardResult": {"card"},
		"PersonCard":       {"brief", "last_interaction"},
		"PersonCardBrief":  {"untrusted_text"},
	} {
		if schema := schemas[name]; schema != nil {
			for _, field := range fields {
				hardenPersonCardNullableRef(schemas, schema.Properties[field], client)
				if client {
					nullableSchemaProperty(schema, field)
				}
			}
		}
	}
	if schema := schemas["PersonCardFreshnessBlock"]; schema != nil {
		if projections := schema.Properties["projections"]; projections != nil {
			if value, ok := projections.AdditionalProperties.(*huma.Schema); ok {
				hardenPersonCardNullableRef(schemas, value, client)
			}
		}
	}
	if client {
		for name, schema := range schemas {
			if !strings.HasPrefix(name, "PersonCard") && name != "ProjectionFreshness" {
				continue
			}
			for field, property := range schema.Properties {
				if !property.Nullable {
					continue
				}
				// Required nullable fields must retain null on re-encoding.
				// The generator otherwise uses a value for nullable integers.
				if property.Type == huma.TypeInteger {
					setCodegenGoType(property, "*int64")
				}
				nullableSchemaProperty(schema, field)
			}
		}
	}
}

func hardenPersonCardNullableRef(schemas map[string]*huma.Schema, property *huma.Schema, client bool) {
	if property == nil || property.Ref == "" {
		return
	}
	if client {
		// OpenAPI 3.0 cannot express the public object-or-null union. Nullable
		// object components keep references valid and generate pointer values,
		// including nullable projection-map entries, without an empty type.
		if target := schemas[strings.TrimPrefix(property.Ref, "#/components/schemas/")]; target != nil {
			target.Nullable = true
		}
		return
	}
	ref := property.Ref
	property.Ref = ""
	property.OneOf = []*huma.Schema{{Ref: ref}, {Type: "null"}}
}
