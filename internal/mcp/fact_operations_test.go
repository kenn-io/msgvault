package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFactCatalogUsesRecordsAndSeparateRecoveryGates(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	reads := []string{"list_fact_targets", "list_person_fact_evidence", "list_person_fact_status_events", "list_person_fact_claims", "list_person_fact_decisions", "list_person_fact_pins", "get_person_tracking", "list_person_merges", "get_person_merge", "get_person_merge_snapshot"}
	writes := []string{"set_person_fact_pin", "set_person_tracking", "review_person_merge_candidate", "split_person"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: append(reads, writes...)}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range reads {
		_, present := tools[name]
		assertions.True(present, "missing read %s", name)
	}
	for _, name := range writes {
		_, present := tools[name]
		assertions.False(present, "write default-off %s", name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyRecords, OperationFamilyMergeRecovery}
	tools = toolsByName(t, rawListTools(t, opts, true))
	for _, name := range writes {
		_, present := tools[name]
		requirements.True(present, "missing opt-in write %s", name)
	}
	tools = toolsByName(t, rawListTools(t, opts, false))
	for _, name := range writes {
		_, present := tools[name]
		assertions.False(present, "HTTP global gate %s", name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyRecords}
	tools = toolsByName(t, rawListTools(t, opts, true))
	for _, name := range []string{"review_person_merge_candidate", "split_person"} {
		_, present := tools[name]
		assertions.False(present, "recovery needs its dedicated gate %s", name)
	}
	opts.DelegatedOnly = true
	tools = toolsByName(t, rawListTools(t, opts, true))
	for _, name := range append(reads, writes...) {
		_, present := tools[name]
		assertions.False(present, "delegated archive excludes owner records %s", name)
	}
}
