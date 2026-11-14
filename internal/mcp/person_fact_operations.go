package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type PersonFactClaimsOutput struct {
	Claims    []generated.PersonFactClaim    `json:"claims"`
	Evidence  []generated.PersonFactEvidence `json:"evidence,omitzero"`
	Decisions []generated.PersonFactDecision `json:"decisions,omitzero"`
}

func personFactOperationalDefinitions() []operationalDefinition {
	pin := newOperationalDefinition("set_person_fact_pin", "Set or clear an attribute or employment fact pin against the current native target. Requires the person to be tracked and confirmation.", OperationFamilyPersonFacts, closedObject(map[string]*jsonschema.Schema{"person_id": safeIDSchema("Durable person ID"), "kind": {Type: "string", Enum: []any{"attribute", "employment"}}, "key": {Type: "string", Description: "Active target key from a claim target or `msgvault person facts catalog`", MinLength: new(1)}, "pinned": booleanSchema("Desired pin state")}, "person_id", "kind", "key", "pinned"), outputSchemaFor[generated.PersonFactPinWrite](), true)
	pin.definition.annotations.DestructiveHint = new(true)
	tracking := newOperationalDefinition("set_person_tracking", "Replace the person's tracking state. This native operation has no revision guard. Requires confirmation.", OperationFamilyPersonFacts, closedObject(map[string]*jsonschema.Schema{"person_id": safeIDSchema("Durable person ID"), "tracked": booleanSchema("Desired tracking state")}, "person_id", "tracked"), outputSchemaFor[generated.PersonTracking](), true)
	tracking.definition.annotations.DestructiveHint = new(true)
	tracking.definition.annotations.OpenWorldHint = new(true)
	return []operationalDefinition{
		newOperationalDefinition("list_person_fact_claims", "Read fact claims, optionally including evidence and decisions. Rows for sensitive attributes are omitted, along with evidence and decisions tied to them; the CLI (`msgvault person facts`) provides access to them.", OperationFamilyPersonFacts, closedObject(map[string]*jsonschema.Schema{
			"person_id":         safeIDSchema("Durable person ID"),
			"target":            {Type: "string", Description: "Exact target as kind:key:revision, assembled from a claim's target; keys may contain colons", MinLength: new(1)},
			"limit":             boundedIntegerSchema("Maximum rows per list (default 50)", 1, 200),
			"offset":            nonNegativeIntegerSchema("Zero-based row offset per list", 0),
			"include_evidence":  booleanSchema("Include a page of evidence"),
			"include_decisions": booleanSchema("Include a page of decisions"),
		}, "person_id"), outputSchemaFor[PersonFactClaimsOutput](), false),
		newOperationalDefinition("list_person_fact_pins", "Read effective person fact pins.", OperationFamilyPersonFacts, closedObject(map[string]*jsonschema.Schema{"person_id": safeIDSchema("Durable person ID")}, "person_id"), outputSchemaFor[generated.PersonFactPinsResponse](), false),
		pin,
		tracking,
	}
}
