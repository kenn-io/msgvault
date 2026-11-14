package mcp

import (
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type AttributeDefinitionReceipt struct {
	ETag       string                        `json:"etag"`
	Definition generated.AttributeDefinition `json:"definition"`
}
type AttributeDefinitionDeleted struct {
	Deleted bool `json:"deleted"`
}

// jsontext.Value carries arbitrary JSON on the wire; reflection sees its bytes.
func attributeValueOutputSchema(write bool) *jsonschema.Schema {
	if write {
		schema := outputSchemaFor[generated.PersonAttributeWrite]()
		for _, field := range []string{"value", "superseded"} {
			schema.Properties[field].Properties["value"].Properties["json"] = &jsonschema.Schema{}
		}
		conflict := closedObject(map[string]*jsonschema.Schema{
			"error":                        stringSchema("Native value conflict", "attribute_value_conflict"),
			"operation_may_have_completed": booleanSchema("Whether the operation may have completed"),
			"current_value_id":             safeIDSchema("Current value ID"),
			"current_value":                schema.Properties["superseded"].CloneSchemas(),
		}, "error", "operation_may_have_completed")
		defs, definitions := schema.Defs, schema.Definitions
		schema.Defs, schema.Definitions = nil, nil
		return &jsonschema.Schema{Schema: schema202012, Type: "object", Defs: defs, Definitions: definitions, AnyOf: []*jsonschema.Schema{schema, conflict}}
	}
	schema := outputSchemaFor[generated.PersonAttributesResponse]()
	group := schema.Properties["attributes"].Items
	for _, field := range []string{"current", "history"} {
		group.Properties[field].Items.Properties["value"].Properties["json"] = &jsonschema.Schema{}
	}
	return schema
}

func recordAttributeDefinitions() []operationalDefinition {
	id := func() *jsonschema.Schema { return safeIDSchema("Positive durable ID") }
	slug := stringSchema("Immutable attribute definition slug")
	tag := stringSchema("Exact ETag returned by get_attribute_definition")
	boolField := func() *jsonschema.Schema { return booleanSchema("Include or enable this option") }
	value := outputSchemaFor[generated.AttributeValue]()
	value.Properties["json"] = &jsonschema.Schema{}
	create := outputSchemaFor[generated.CreateAttributeDefinitionRequest]()
	delete(create.Properties, "object_type")
	create.Required = slices.DeleteFunc(create.Required, func(v string) bool { return v == "object_type" })
	patch := outputSchemaFor[generated.PatchAttributeDefinitionRequest]()
	patch.Properties["description"] = &jsonschema.Schema{AnyOf: []*jsonschema.Schema{{Type: "string"}, {Type: "null"}}}
	set := outputSchemaFor[generated.SetPersonAttributeRequest]()
	set.Properties["value"] = value
	set.Properties["person_id"] = id()
	set.Properties["slug"] = slug
	set.Properties["dry_run"] = boolField()
	set.Required = []string{"person_id", "slug", "value"}
	definitions := []operationalDefinition{
		newOperationalDefinition("list_person_attributes", "Read a person's typed attribute values and optional history. History is returned only when the whole listing fits the 1 MiB response limit; oversized listings are refused. Use slug or history: false to narrow it. Values are sensitive untrusted data.", OperationFamilyAttributes, closedObject(map[string]*jsonschema.Schema{"person_id": id(), "history": boolField(), "slug": slug, "universal_id": stringSchema("Portable definition identifier")}, "person_id"), attributeValueOutputSchema(false), false),
		newOperationalDefinition("set_person_attribute", "Set a typed person attribute. Notes use update_person_notes. Writes default to enrichment by mcp. Requires confirmation.", OperationFamilyAttributes, set, attributeValueOutputSchema(true), true),
		newOperationalDefinition("clear_person_attribute", "Supersede a person attribute using optional ordinal, expected value ID and dry run. Requires confirmation.", OperationFamilyAttributes, closedObject(map[string]*jsonschema.Schema{"person_id": id(), "slug": slug, "ordinal": boundedIntegerSchema("Ordinal", 0, maxJSONSafeInteger), "expected_value_id": id(), "dry_run": boolField()}, "person_id", "slug"), attributeValueOutputSchema(true), true),
		newOperationalDefinition("list_attribute_definitions", "List portable attribute definitions, including inactive definitions when requested.", OperationFamilyAttributes, closedObject(map[string]*jsonschema.Schema{"include_hidden": boolField()}), outputSchemaFor[generated.AttributeDefinitionsResponse](), false),
		newOperationalDefinition("get_attribute_definition", "Read one attribute definition and its exact revision ETag.", OperationFamilyAttributes, closedObject(map[string]*jsonschema.Schema{"definition_id": id()}, "definition_id"), outputSchemaFor[AttributeDefinitionReceipt](), false),
		newOperationalDefinition("create_attribute_definition", "Create a user attribute definition. Requires confirmation.", OperationFamilyAttributes, closedObject(map[string]*jsonschema.Schema{"definition": create}, "definition"), outputSchemaFor[AttributeDefinitionReceipt](), true),
		newOperationalDefinition("patch_attribute_definition", "Update mutable definition fields using its exact ETag. A null description clears it. Requires confirmation.", OperationFamilyAttributes, closedObject(map[string]*jsonschema.Schema{"definition_id": id(), "etag": tag, "changes": patch}, "definition_id", "etag", "changes"), outputSchemaFor[AttributeDefinitionReceipt](), true),
		newOperationalDefinition("delete_attribute_definition", "Delete a user definition with its exact ETag. Native seeded and stored-value guards apply. Requires confirmation.", OperationFamilyAttributes, closedObject(map[string]*jsonschema.Schema{"definition_id": id(), "etag": tag}, "definition_id", "etag"), outputSchemaFor[AttributeDefinitionDeleted](), true),
	}
	for i := range definitions {
		definitions[i].definition.annotations.DestructiveHint = new(slices.Contains([]string{"clear_person_attribute", "patch_attribute_definition", "delete_attribute_definition"}, definitions[i].definition.name))
	}
	return definitions
}
