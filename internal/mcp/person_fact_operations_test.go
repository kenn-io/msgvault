package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonFactCatalogOwnerCapabilitiesAndWriteGate(t *testing.T) {
	names := []string{"list_person_fact_targets", "list_person_fact_evidence", "list_person_fact_status_events", "list_person_fact_claims", "list_person_fact_decisions", "list_person_fact_pins", "get_person_tracking", "set_person_fact_pin", "set_person_tracking"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	reads := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names[:7] {
		assert.Contains(t, reads, name)
	}
	for _, name := range names[7:] {
		assert.NotContains(t, reads, name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyRecords}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, true)), "set_person_tracking")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyPersonFacts}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		assert.Contains(t, tools, name)
	}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, false)), "set_person_tracking")
	opts.DelegatedOnly = true
	assert.Empty(t, rawListTools(t, opts, true))
	opts.DelegatedOnly = false
	opts.OperationCapabilities = []string{"get_person_tracking"}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, true)), "set_person_tracking")
	defs := personFactOperationalDefinitions()
	require.Len(t, defs, 9)
	for _, d := range defs {
		require.NotNil(t, d.definition.inputSchema.AdditionalProperties)
		assert.NotNil(t, d.definition.inputSchema.AdditionalProperties.Not)
	}
}
