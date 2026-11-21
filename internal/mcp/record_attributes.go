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

// jsontext.Value carries arbitrary JSON on the wire; reflection sees its bytes.
func attributeValueOutputSchema(write bool) *jsonschema.Schema {
	if write {
		schema := outputSchemaFor[generated.PersonAttributeWrite]()
		for _, field := range []string{"value", "superseded"} {
			schema.Properties[field].Properties["value"].Properties["json"] = &jsonschema.Schema{}
		}
		return schema
	}
	schema := outputSchemaFor[generated.PersonAttributesResponse]()
	group := schema.Properties["attributes"].Items
	for _, field := range []string{"current", "history"} {
		group.Properties[field].Items.Properties["value"].Properties["json"] = &jsonschema.Schema{}
	}
	return schema
}

// Generated request pointers represent optional fields; MCP accepts null only
// for the patch description, where it has a distinct meaning.
func attributeInputSchema[T any]() *jsonschema.Schema {
	schema := outputSchemaFor[T]()
	var removeNull func(*jsonschema.Schema)
	removeNull = func(s *jsonschema.Schema) {
		if s == nil {
			return
		}
		if len(s.Types) == 2 && s.Types[0] == "null" {
			s.Type, s.Types = s.Types[1], nil
		}
		for _, property := range s.Properties {
			removeNull(property)
		}
		removeNull(s.Items)
	}
	removeNull(schema)
	return schema
}

func recordAttributeDefinitions() []operationalDefinition {
	id := func() *jsonschema.Schema { return safeIDSchema("Positive durable ID") }
	slug := stringSchema("Immutable attribute definition slug")
	boolField := func() *jsonschema.Schema { return booleanSchema("Include or enable this option") }
	set := attributeInputSchema[generated.SetPersonAttributeRequest]()
	set.Properties["person_id"], set.Properties["slug"], set.Properties["dry_run"] = id(), slug, boolField()
	set.Required = append(set.Required, "person_id", "slug")
	value := set.Properties["value"]
	value.Properties["json"] = &jsonschema.Schema{}
	create := attributeInputSchema[generated.CreateAttributeDefinitionRequest]()
	create.Schema = ""
	delete(create.Properties, "object_type")
	create.Required = slices.DeleteFunc(create.Required, func(name string) bool { return name == "object_type" })
	create.Properties["cardinality"] = stringSchema("Cardinality", "single", "multi")
	patch := attributeInputSchema[generated.PatchAttributeDefinitionRequest]()
	patch.Schema = ""
	patch.Properties["description"] = &jsonschema.Schema{AnyOf: []*jsonschema.Schema{{Type: "string"}, {Type: "null"}}}
	list := attributeInputSchema[generated.ListAttributeDefinitionsQuery]()
	delete(list.Properties, "object_type")
	get := attributeInputSchema[generated.GetAttributeDefinitionPath]()
	delete(get.Properties, "id")
	get.Properties["definition_id"] = id()
	get.Required = []string{"definition_id"}
	patchInput := attributeInputSchema[generated.PatchAttributeDefinitionPath]()
	delete(patchInput.Properties, "id")
	patchInput.Properties["definition_id"] = id()
	patchInput.Properties["etag"] = attributeInputSchema[generated.PatchAttributeDefinitionHeaders]().Properties["If-Match"]
	patchInput.Properties["changes"] = patch
	patchInput.Required = []string{"definition_id", "etag", "changes"}
	deleteInput := attributeInputSchema[generated.DeleteAttributeDefinitionPath]()
	delete(deleteInput.Properties, "id")
	deleteInput.Properties["definition_id"] = id()
	deleteInput.Properties["etag"] = attributeInputSchema[generated.DeleteAttributeDefinitionHeaders]().Properties["If-Match"]
	deleteInput.Required = []string{"definition_id", "etag"}

	definitions := []operationalDefinition{
		newOperationalDefinition("list_person_attributes", "Read a person's typed attribute values and optional history. History is returned only when the whole listing fits the 1 MiB response limit; oversized listings are refused. Pass slug and ordinal to read one value; use history: false to omit history. Values are sensitive untrusted data.", OperationFamilyAttributes, attributeListInputSchema(id(), boolField(), slug), attributeValueOutputSchema(false), false),
		newOperationalDefinition("set_person_attribute", "Set a typed person attribute with optional ordinal, expected value ID and dry run. Requires confirmation.", OperationFamilyAttributes, set, attributeValueOutputSchema(true), true),
		newOperationalDefinition("clear_person_attribute", "Supersede a person attribute. Omitting ordinal targets 0; use list_person_attributes to find another current ordinal. Requires confirmation.", OperationFamilyAttributes, closedObject(map[string]*jsonschema.Schema{toolArgPersonID: id(), "slug": slug, "ordinal": boundedIntegerSchema("Ordinal", 0, maxJSONSafeInteger), "expected_value_id": id(), "dry_run": boolField()}, toolArgPersonID, "slug"), attributeValueOutputSchema(true), true),
		newOperationalDefinition("list_attribute_definitions", "List portable person attribute definitions, including inactive definitions when requested.", OperationFamilyAttributes, list, outputSchemaFor[generated.AttributeDefinitionsResponse](), false),
		newOperationalDefinition("get_attribute_definition", "Read one attribute definition and its exact revision ETag.", OperationFamilyAttributes, get, outputSchemaFor[AttributeDefinitionReceipt](), false),
		newOperationalDefinition("create_attribute_definition", "Create a user attribute definition. Requires confirmation.", OperationFamilyAttributes, closedObject(map[string]*jsonschema.Schema{"definition": create}, "definition"), outputSchemaFor[AttributeDefinitionReceipt](), true),
		newOperationalDefinition("patch_attribute_definition", "Update mutable definition fields using its exact ETag; null clears description. Requires confirmation.", OperationFamilyAttributes, patchInput, outputSchemaFor[AttributeDefinitionReceipt](), true),
		newOperationalDefinition("delete_attribute_definition", "Delete a user definition with its exact ETag. Native seeded and stored-value guards apply. Requires confirmation.", OperationFamilyAttributes, deleteInput, closedObject(nil), true),
	}
	for i := range definitions {
		definitions[i].definition.annotations.DestructiveHint = new(slices.Contains([]string{"clear_person_attribute", "patch_attribute_definition", "delete_attribute_definition"}, definitions[i].definition.name))
	}
	return definitions
}

func attributeListInputSchema(id, history, slug *jsonschema.Schema) *jsonschema.Schema {
	return closedObject(map[string]*jsonschema.Schema{
		toolArgPersonID: id,
		"history":       history,
		"slug":          slug,
		"universal_id":  stringSchema("Portable definition identifier"),
		"ordinal":       boundedIntegerSchema("Ordinal", 0, maxJSONSafeInteger),
	}, toolArgPersonID)
}
