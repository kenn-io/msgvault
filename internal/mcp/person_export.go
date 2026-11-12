package mcp

import "github.com/google/jsonschema-go/jsonschema"

// PersonExport is the complete native JSONL stream, including its manifest and receipt.
type PersonExport struct {
	JSONL string `json:"jsonl"`
}

func personExportDefinitions() []operationalDefinition {
	return []operationalDefinition{newOperationalDefinition("export_person_messages", "Export a person's messages as complete native JSONL for an inclusive start and exclusive end. Output is limited to 512 KiB.", OperationFamilyRecords, closedObject(map[string]*jsonschema.Schema{
		"person_id":     safeIDSchema("Durable person ID"),
		"start":         stringSchema("Inclusive RFC3339 timestamp"),
		"end":           stringSchema("Exclusive RFC3339 timestamp"),
		"sources":       {Type: schemaTypeArray, Items: stringSchema("Exact source selector type:identifier")},
		"message_types": {Type: schemaTypeArray, Items: stringSchema("Exact message type")},
	}, "person_id", "start", "end"), outputSchemaFor[PersonExport](), false)}
}
