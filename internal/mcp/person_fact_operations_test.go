package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonFactCatalogOwnerCapabilitiesAndWriteGate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	names := []string{"list_person_fact_claims", "list_person_fact_pins", "set_person_fact_pin", "set_person_tracking"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	reads := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names[:2] {
		assertions.Contains(reads, name)
	}
	result := rawCallTool(t, opts, "list_person_fact_claims", map[string]any{"person_id": 1})
	assertions.NotEqual(true, result["isError"])
	result = rawCallTool(t, opts, "list_person_fact_claims", map[string]any{"person_id": 1, "target": ""})
	assertions.Equal(true, result["isError"])
	for _, name := range names[2:] {
		assertions.NotContains(reads, name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyPersonFacts}
	tools := toolsByName(t, rawListTools(t, opts, true))
	trackingAnnotations, ok := tools["set_person_tracking"]["annotations"].(map[string]any)
	requirements.True(ok)
	assertions.Equal(true, trackingAnnotations["openWorldHint"])
	for _, name := range names {
		assertions.Contains(tools, name)
	}
}
