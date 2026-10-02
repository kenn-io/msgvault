package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"

	"go.kenn.io/msgvault/pkg/client/generated"
)

func historyOperationalDefinitions() []operationalDefinition {
	filters := map[string]*jsonschema.Schema{
		"kind":           {Type: mcpSchemaString, Enum: []any{"source_sync", "person_sweep", "carddav_sync", "message_embedding", "person_embedding", "document_extraction", "document_embedding", "visual_embedding", "person_enrichment"}},
		"lane":           {Type: mcpSchemaString, Enum: []any{"messages", "person_facts", "contacts", "documents", "visual_attachments"}},
		"state":          {Type: mcpSchemaString, Enum: []any{"queued", "running", "succeeded", "partial", "failed", "cancelled"}},
		"started_from":   {Type: mcpSchemaString, Format: "date-time", Description: "Inclusive canonical UTC RFC3339 bound"},
		"started_before": {Type: mcpSchemaString, Format: "date-time", Description: "Exclusive canonical UTC RFC3339 bound"},
		toolArgLimit:     {Type: mcpSchemaInteger, Minimum: new(float64(1)), Maximum: new(float64(100)), Description: "Maximum runs; default25"},
		"cursor":         stringSchema("Opaque cursor returned by a preceding page with the same filters"),
	}
	return []operationalDefinition{
		newOperationalDefinition("list_operation_runs", "Read durable operation history with archive-bound, filter-bound cursors, sanitized failures and explicit unavailable lanes. Does not start work.", OperationFamilySources, closedObject(filters), outputSchemaFor[generated.OperationRunsResponse](), false, false),
		newOperationalDefinition("get_operation_run", "Read a durable operation using the exact opaque run ID returned by list_operation_runs. Supported actions are metadata.", OperationFamilySources, closedObject(map[string]*jsonschema.Schema{"run_id": stringSchema("Exact opaque run ID; never a database integer")}, "run_id"), outputSchemaFor[generated.OperationRunDetail](), false, false),
		newOperationalDefinition("get_operation_status", "Read durable lane availability, active/latest/latest-successful runs and supported action metadata. Does not start work.", OperationFamilySources, closedObject(nil), outputSchemaFor[generated.OperationStatusResponse](), false, false),
	}
}
