package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func operationRunDefinitions() []operationalDefinition {
	return []operationalDefinition{
		newOperationalDefinition("get_operation_status", "Read operation lane readiness and latest runs. supported_actions and related_status name daemon capabilities; this tool does not invoke them.", OperationFamilySources, closedObject(nil), outputSchemaFor[generated.OperationStatusResponse](), false),
		newOperationalDefinition("list_operation_runs", "List durable operation history. Cursors are bound to the archive and filters; repeat the same filters on subsequent pages. On invalid_cursor or operation_history_conflict, restart from the first page.", OperationFamilySources, closedObject(map[string]*jsonschema.Schema{
			"kind":           stringSchema("Exact native operation kind"),
			"lane":           stringSchema("Exact native operation lane"),
			"state":          stringSchema("Exact native operation state"),
			"started_from":   stringSchema("Inclusive canonical UTC RFC3339 lower bound with Z"),
			"started_before": stringSchema("Exclusive canonical UTC RFC3339 upper bound with Z"),
			"limit":          {Type: "integer", Minimum: new(float64(1)), Maximum: new(float64(100))},
			"cursor":         stringSchema("Opaque cursor from the previous page"),
		}), outputSchemaFor[generated.OperationRunsResponse](), false),
		newOperationalDefinition("get_operation_run", "Read one durable run using its opaque archive-bound ID. supported_actions and related_status name daemon capabilities; this tool does not invoke them.", OperationFamilySources, closedObject(map[string]*jsonschema.Schema{"id": stringSchema("Opaque run ID returned by operation history or status")}, "id"), outputSchemaFor[generated.OperationRunDetail](), false),
	}
}
