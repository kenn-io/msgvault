package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonFactCatalogOwnerCapabilitiesAndWriteGate(t *testing.T) {
	assertions := assert.New(t)
	names := []string{"list_person_fact_claims", "list_person_fact_pins", "set_person_fact_pin", "set_person_tracking"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	reads := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names[:2] {
		assertions.Contains(reads, name)
	}
	result := rawCallTool(t, opts, "list_person_fact_claims", map[string]any{"person_id": 1, "target": ""})
	assertions.Equal(true, result["isError"])
	for _, name := range names[2:] {
		assertions.NotContains(reads, name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyPersonFacts}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		assertions.Contains(tools, name)
	}
	for _, name := range names[2:] {
		annotations, ok := tools[name]["annotations"].(map[string]any)
		require.True(t, ok)
		assertions.Equal(true, annotations["destructiveHint"], name)
	}
	annotations := tools["set_person_tracking"]["annotations"].(map[string]any)
	assertions.Equal(true, annotations["openWorldHint"])
}
