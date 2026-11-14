package mcp

import "github.com/google/jsonschema-go/jsonschema"

// PersonExport is the complete native JSONL stream, including its manifest and receipt.
type PersonExport struct {
	JSONL string `json:"jsonl"`
}

func personExportDefinitions() []operationalDefinition {
	return []operationalDefinition{newOperationalDefinition(
		"export_person_messages",
		"Return native JSONL for one person's messages in a time window, up to 512 KiB. "+
			"A single message over the limit cannot be returned. Narrow the window when several messages exceed it. "+
			"Dates must be RFC3339 and start must precede end. Sources use type:identifier. "+
			"Blank message types are refused; unknown nonempty types match nothing. "+
			"Exporter errors use invalid_start, invalid_end, invalid_window, invalid_message_type, "+
			"invalid_source_N, unknown_source_N, unknown_person, or export_limit_exceeded. "+
			"N is the source position, starting at 1.",
		OperationFamilyRecords,
		closedObject(map[string]*jsonschema.Schema{
			"person_id":     safeIDSchema("Durable person ID"),
			"start":         stringSchema("Inclusive RFC3339 timestamp"),
			"end":           stringSchema("Exclusive RFC3339 timestamp"),
			"sources":       {Type: schemaTypeArray, Items: stringSchema("Exact source selector type:identifier")},
			"message_types": {Type: schemaTypeArray, Items: stringSchema("Exact type, such as email, sms, whatsapp, or meeting_transcript. Unknown nonempty values match nothing.")},
		}, "person_id", "start", "end"),
		outputSchemaFor[PersonExport](),
		false,
	)}
}
