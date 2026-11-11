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

func operationalSettingsDefinitions() []operationalDefinition {
	output := outputSchemaFor[OperationalSettings]()
	output.Properties["settings"].Items.Properties["value"] = settingValueSchema("boolean", "string", "integer", "number")
	update := closedObject(map[string]*jsonschema.Schema{"key": stringSchema("Exact allowlisted setting key"), "value": settingValueSchema("boolean", "string", "integer", "number")}, "key", "value")
	return []operationalDefinition{
		newOperationalDefinition("get_operational_settings", "Read allowlisted operational settings, the exact settings ETag and pending restart state. Endpoints, credentials, paths and operator setup are excluded.", OperationFamilySettings, closedObject(nil), output, false),
		newOperationalDefinition("update_operational_settings", "Update allowlisted operational settings using the exact ETag. Omitted settings stay unchanged; false and empty strings are explicit. Requires approval and may require restart.", OperationFamilySettings, closedObject(map[string]*jsonschema.Schema{"etag": stringSchema("Exact ETag from get_operational_settings"), "updates": {Type: schemaTypeArray, Items: update}}, "etag", "updates"), output, true),
	}
}
