package mcp

import (
	"encoding/json/jsontext"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func loopOperationalDefinitions() []operationalDefinition {
	return []operationalDefinition{
		newOperationalDefinition("list_open_loops", "List unanswered questions and cited commitments. coverage_incomplete means call again to continue checking retained evidence; truncated means narrow by person_id because there is no cursor; skipped_unparseable means an unreadable original may contain a reply that settled a listed loop. These evidence warnings do not measure source freshness.", OperationFamilyLoops, closedObject(map[string]*jsonschema.Schema{
			"person_id":      safeIDSchema("Only questions with this durable person ID"),
			"include_closed": {Type: "boolean", Description: "Include done, snoozed and fulfilled promises to find a promise to reopen"},
			"limit":          {Type: "integer", Minimum: new(float64(1)), Maximum: new(float64(200)), Default: jsontext.Value("50")},
		}), outputSchemaFor[generated.LoopListResponse](), false),
		newOperationalDefinition("close_open_loop", "Mark done exactly the listed questions after rechecking the anchor's evidence. Questions that appeared after the list stay open. Requires approval.", OperationFamilyLoops, closedObject(map[string]*jsonschema.Schema{
			"message_id":           safeIDSchema("Exact anchor_message_id from a list or decision receipt"),
			"question_message_ids": listedQuestionsSchema(),
		}, "message_id", "question_message_ids"), outputSchemaFor[generated.LoopDecisionResponse](), true),
		newOperationalDefinition("snooze_open_loop", "Snooze exactly the listed questions after rechecking the anchor's evidence. Questions that appeared after the list stay open. Requires approval. until accepts RFC3339 or YYYY-MM-DD at local midnight; relative durations are rejected.", OperationFamilyLoops, closedObject(map[string]*jsonschema.Schema{
			"message_id":           safeIDSchema("Exact anchor_message_id from a list or decision receipt"),
			"question_message_ids": listedQuestionsSchema(),
			"until":                stringSchema("Future RFC3339 instant or YYYY-MM-DD at local midnight"),
		}, "message_id", "question_message_ids", "until"), outputSchemaFor[generated.LoopDecisionResponse](), true),
		newOperationalDefinition("reopen_open_loop", "Clear the saved decision on one exact question. Use its anchor_message_id from a previous list or decision receipt because hidden questions are absent from the current list. Requires approval.", OperationFamilyLoops, closedObject(map[string]*jsonschema.Schema{"message_id": safeIDSchema("Exact anchor_message_id from a previous list or decision receipt")}, "message_id"), outputSchemaFor[generated.LoopDecisionResponse](), true),
		newOperationalDefinition("set_commitment_decision", "Set the owner decision on one exact cited promise. Requires approval. Snoozed requires a future RFC3339 instant or local date.", OperationFamilyLoops, closedObject(map[string]*jsonschema.Schema{
			"commitment_id": {Type: "string", Pattern: "^c[1-9][0-9]*$", Description: "Exact ID from a list or prior decision"},
			"state":         {Type: "string", Enum: []any{"open", "done", "snoozed"}},
			"until":         stringSchema("Future RFC3339 instant or YYYY-MM-DD at local midnight"),
		}, "commitment_id", "state"), outputSchemaFor[generated.CommitmentDecisionResponse](), true),
	}
}

func listedQuestionsSchema() *jsonschema.Schema {
	one := 1
	return &jsonschema.Schema{Type: schemaTypeArray, Description: "question_message_ids from the same list item", Items: safeIDSchema("Question message ID"), MinItems: &one}
}
