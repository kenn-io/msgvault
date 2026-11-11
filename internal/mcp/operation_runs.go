package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func operationRunDefinitions() []operationalDefinition {
	return []operationalDefinition{
		newOperationalDefinition("get_operation_status", "Read operation lane readiness and latest runs.", OperationFamilySources, closedObject(nil), outputSchemaFor[generated.OperationStatusResponse](), false),
		newOperationalDefinition("list_operation_runs", "List durable operation history. Cursors are bound to the archive and filters; repeat the same filters on subsequent pages.", OperationFamilySources, closedObject(map[string]*jsonschema.Schema{
			"kind":           stringSchema("Exact operation kind", "carddav_sync", "message_embedding", "person_embedding", "person_enrichment", "person_sweep", "source_sync", "visual_embedding"),
			"lane":           stringSchema("Exact operation lane", "contacts", "messages", "person_facts", "visual_attachments"),
			"state":          stringSchema("Exact operation state", "cancelled", "failed", "partial", "paused", "queued", "running", "succeeded"),
			"started_from":   stringSchema("Inclusive canonical UTC RFC3339 lower bound"),
			"started_before": stringSchema("Exclusive canonical UTC RFC3339 upper bound"),
			"limit":          {Type: "integer", Minimum: new(float64(1)), Maximum: new(float64(100))},
			"cursor":         stringSchema("Opaque cursor from the previous page"),
		}), outputSchemaFor[generated.OperationRunsResponse](), false),
		newOperationalDefinition("get_operation_run", "Read one durable run using its opaque archive-bound ID.", OperationFamilySources, closedObject(map[string]*jsonschema.Schema{"id": stringSchema("Opaque run ID returned by operation history or status")}, "id"), outputSchemaFor[generated.OperationRunDetail](), false),
	}
}
