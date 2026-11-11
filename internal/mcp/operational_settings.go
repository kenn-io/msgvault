package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// OperationalSettings contains only the fixed nonsecret settings admitted by MCP.
type OperationalSettings struct {
	ETag           string              `json:"etag"`
	PendingRestart bool                `json:"pending_restart"`
	Settings       []generated.Setting `json:"settings"`
}

func operationalSettingValueSchema() *jsonschema.Schema {
	return &jsonschema.Schema{OneOf: []*jsonschema.Schema{
		closedObject(map[string]*jsonschema.Schema{"boolean": booleanSchema("Boolean value")}, "boolean"),
		closedObject(map[string]*jsonschema.Schema{"string": stringSchema("String value")}, "string"),
		closedObject(map[string]*jsonschema.Schema{"integer": {Type: "integer"}}, "integer"),
		closedObject(map[string]*jsonschema.Schema{"number": {Type: "number"}}, "number"),
	}}
}

func operationalSettingsDefinitions() []operationalDefinition {
	output := outputSchemaFor[OperationalSettings]()
	output.Properties["settings"].Items.Properties["value"] = operationalSettingValueSchema()
	update := closedObject(map[string]*jsonschema.Schema{"key": stringSchema("Exact allowlisted setting key"), "value": operationalSettingValueSchema()}, "key", "value")
	return []operationalDefinition{
		newOperationalDefinition("get_operational_settings", "Read allowlisted operational settings, the exact settings ETag and pending restart state. Endpoints, credentials, paths and operator setup are excluded.", OperationFamilySettings, closedObject(nil), output, false),
		newOperationalDefinition("update_operational_settings", "Update allowlisted operational settings using the exact ETag. Omitted settings stay unchanged; false and empty strings are explicit. Requires approval and may require restart.", OperationFamilySettings, closedObject(map[string]*jsonschema.Schema{"etag": stringSchema("Exact ETag from get_operational_settings"), "updates": {Type: schemaTypeArray, Items: update}}, "etag", "updates"), output, true),
	}
}
