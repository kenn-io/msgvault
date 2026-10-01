package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func sweepOperationalDefinitions() []operationalDefinition {
	history := map[string]*jsonschema.Schema{"person_id": safeIDSchema("Optional durable person filter; absent means all people"), "limit": boundedIntegerSchema("Maximum runs and attempts; default 20, maximum 200", 1, 200)}
	run := map[string]*jsonschema.Schema{"person_id": safeIDSchema("Optional tracked person; absent means eligible tracked people"), "limit": boundedIntegerSchema("Maximum people; default min(25, configured work_batch_size); cannot exceed the configured bound", 1, 9007199254740991), "backstop": booleanSchema("Use the native backstop mode instead of incremental mode; default false")}
	return []operationalDefinition{
		newOperationalDefinition("get_people_sweep_status", "Read safe sweep status, configured batch size and budget caps without a provider request. Uses the fixed owning JSON command.", OperationFamilyInference, closedObject(nil), outputSchemaFor[peoplesweep.StatusOutput](), false, false),
		newOperationalDefinition("list_people_sweep_history", "Read redacted sweep runs and attempts, optionally filtered by person. No excerpts or raw diagnostics; no provider request.", OperationFamilyInference, closedObject(history), outputSchemaFor[peoplesweep.HistoryOutput](), false, false),
		newOperationalDefinition("run_people_sweep", "Run bounded person maintenance after disclosure and approval. Uses the current configured checked/consented provider and its native budgets; may incur cost. Scoped runs require tracking. Attempt failures remain in history; acceptance is not an assertion that every person succeeded.", OperationFamilyInference, closedObject(run), outputSchemaFor[peoplesweep.RunOutput](), true, false),
	}
}
