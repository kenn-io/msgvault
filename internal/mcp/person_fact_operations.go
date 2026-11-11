package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func personFactOperationalDefinitions() []operationalDefinition {
	page := func(status bool, maximum float64) *jsonschema.Schema {
		p := map[string]*jsonschema.Schema{"person_id": safeIDSchema("Durable person ID"), "limit": boundedIntegerSchema("Maximum rows (default 50)", 1, maximum), "offset": nonNegativeIntegerSchema("Zero-based row offset", 0)}
		if status {
			p["evidence_key"] = stringSchema("Exact evidence key")
			p["supported"] = booleanSchema("Filter by support state")
		} else {
			p["target"] = stringSchema("Exact target as kind:key:sha256:<64 lowercase hex characters>, assembled from kind:key:revision in list_person_fact_targets; keys may contain colons")
		}
		return closedObject(p, "person_id")
	}
	person := func() *jsonschema.Schema {
		return closedObject(map[string]*jsonschema.Schema{"person_id": safeIDSchema("Durable person ID")}, "person_id")
	}
	return []operationalDefinition{
		newOperationalDefinition("list_person_fact_targets", "List automatic fact targets. Set include_sensitive explicitly to include sensitive targets.", OperationFamilyPersonFacts, closedObject(map[string]*jsonschema.Schema{"include_sensitive": booleanSchema("Include sensitive targets")}), outputSchemaFor[generated.Catalog](), false),
		newOperationalDefinition("list_person_fact_evidence", "Read up to 50 rows of person fact evidence, including excerpts and rows for sensitive targets.", OperationFamilyPersonFacts, page(false, 50), outputSchemaFor[generated.PersonFactEvidenceResponse](), false),
		newOperationalDefinition("list_person_fact_status_events", "Read a bounded page of evidence status events.", OperationFamilyPersonFacts, page(true, 200), outputSchemaFor[generated.PersonFactEvidenceStatusEventsResponse](), false),
		newOperationalDefinition("list_person_fact_claims", "Read a bounded page of fact claims and values, including rows for sensitive targets.", OperationFamilyPersonFacts, page(false, 200), outputSchemaFor[generated.PersonFactClaimsResponse](), false),
		newOperationalDefinition("list_person_fact_decisions", "Read a bounded page of fact resolution decisions.", OperationFamilyPersonFacts, page(false, 200), outputSchemaFor[generated.PersonFactDecisionsResponse](), false),
		newOperationalDefinition("list_person_fact_pins", "Read effective person fact pins.", OperationFamilyPersonFacts, person(), outputSchemaFor[generated.PersonFactPinsResponse](), false),
		newOperationalDefinition("get_person_tracking", "Read the person's explicit tracking state.", OperationFamilyPersonFacts, person(), outputSchemaFor[generated.PersonTracking](), false),
		newOperationalDefinition("set_person_fact_pin", "Set or clear an attribute or employment fact pin against the current native target. Requires the person to be tracked and confirmation.", OperationFamilyPersonFacts, closedObject(map[string]*jsonschema.Schema{"person_id": safeIDSchema("Durable person ID"), "kind": {Type: "string", Enum: []any{"attribute", "employment"}}, "key": stringSchema("Exact active target key"), "pinned": booleanSchema("Desired pin state")}, "person_id", "kind", "key", "pinned"), outputSchemaFor[generated.PersonFactPinWrite](), true),
		newOperationalDefinition("set_person_tracking", "Replace the person's tracking state. This native operation has no revision guard. Requires confirmation.", OperationFamilyPersonFacts, closedObject(map[string]*jsonschema.Schema{"person_id": safeIDSchema("Durable person ID"), "tracked": booleanSchema("Desired tracking state")}, "person_id", "tracked"), outputSchemaFor[generated.PersonTracking](), true),
	}
}
