package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPersonFactCatalogOwnerCapabilitiesAndWriteGate(t *testing.T) {
	assertions := assert.New(t)
	names := []string{"list_person_fact_targets", "list_person_fact_evidence", "list_person_fact_status_events", "list_person_fact_claims", "list_person_fact_decisions", "list_person_fact_pins", "get_person_tracking", "set_person_fact_pin", "set_person_tracking"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	reads := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names[:7] {
		assertions.Contains(reads, name)
	}
	for _, name := range names[7:] {
		assertions.NotContains(reads, name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyRecords}
	assertions.NotContains(toolsByName(t, rawListTools(t, opts, true)), "set_person_tracking")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyPersonFacts}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		assertions.Contains(tools, name)
	}
	assertions.NotContains(toolsByName(t, rawListTools(t, opts, false)), "set_person_tracking")
	opts.OperationCapabilities = []string{"get_person_tracking"}
	assertions.NotContains(toolsByName(t, rawListTools(t, opts, true)), "set_person_tracking")
}
