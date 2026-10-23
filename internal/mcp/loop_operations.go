package mcp

import (
	"encoding/json/jsontext"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func loopOperationalDefinitions() []operationalDefinition {
	return []operationalDefinition{
		newOperationalDefinition("list_open_loops", "List unanswered reply_owed and awaiting_reply questions. coverage_incomplete means call again to continue checking retained evidence; truncated means narrow by person_id because there is no cursor; skipped_unparseable means an unreadable original may contain a reply that settled a listed loop. These evidence warnings do not measure source freshness.", OperationFamilyLoops, closedObject(map[string]*jsonschema.Schema{
			"person_id": safeIDSchema("Only questions with this durable person ID"),
			"limit":     {Type: "integer", Minimum: new(float64(1)), Maximum: new(float64(200)), Default: jsontext.Value("50")},
		}), outputSchemaFor[generated.LoopListResponse](), false),
		newOperationalDefinition("close_open_loop", "Mark done the currently eligible questions in the anchor's conversation after rechecking its evidence. Requires approval.", OperationFamilyLoops, closedObject(map[string]*jsonschema.Schema{"message_id": safeIDSchema("Exact anchor_message_id from a list or decision receipt")}, "message_id"), outputSchemaFor[generated.LoopDecisionResponse](), true),
		newOperationalDefinition("snooze_open_loop", "Snooze the currently eligible questions in the anchor's conversation after rechecking its evidence. Requires approval. until accepts RFC3339 or YYYY-MM-DD at local midnight; relative durations are rejected.", OperationFamilyLoops, closedObject(map[string]*jsonschema.Schema{
			"message_id": safeIDSchema("Exact anchor_message_id from a list or decision receipt"),
			"until":      stringSchema("Future RFC3339 instant or YYYY-MM-DD at local midnight"),
		}, "message_id", "until"), outputSchemaFor[generated.LoopDecisionResponse](), true),
		newOperationalDefinition("reopen_open_loop", "Clear the saved decision on one exact question. Use its anchor_message_id from a previous list or decision receipt because hidden questions are absent from the current list. Requires approval.", OperationFamilyLoops, closedObject(map[string]*jsonschema.Schema{"message_id": safeIDSchema("Exact anchor_message_id from a previous list or decision receipt")}, "message_id"), outputSchemaFor[generated.LoopDecisionResponse](), true),
	}
}
