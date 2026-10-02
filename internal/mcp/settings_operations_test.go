package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsCatalogHasIndependentDefaultFalseWriteGatesAndClosedValues(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	names := []string{"get_operational_settings", "update_operational_settings", "get_enrichment_policy", "update_enrichment_controls", "update_enrichment_policy"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	tools := toolsByName(t, rawListTools(t, opts, false))
	requirements.Contains(tools, "get_operational_settings")
	requirements.Contains(tools, "get_enrichment_policy")
	assertions.NotContains(tools, "update_operational_settings")
	assertions.NotContains(tools, "update_enrichment_controls")
	assertions.NotContains(tools, "update_enrichment_policy")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilySettings}
	tools = toolsByName(t, rawListTools(t, opts, true))
	requirements.Contains(tools, "update_operational_settings")
	assertions.NotContains(tools, "update_enrichment_controls")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyEnrichment}
	tools = toolsByName(t, rawListTools(t, opts, true))
	requirements.Contains(tools, "update_enrichment_controls")
	requirements.Contains(tools, "update_enrichment_policy")
	assertions.NotContains(tools, "update_operational_settings")
	disabledHTTP := toolsByName(t, rawListTools(t, opts, false))
	assertions.NotContains(disabledHTTP, "update_enrichment_policy")
	schema, ok := tools["update_enrichment_policy"]["inputSchema"].(map[string]any)
	requirements.True(ok)
	properties, ok := schema["properties"].(map[string]any)
	requirements.True(ok)
	changes, ok := properties["changes"].(map[string]any)
	requirements.True(ok)
	assertions.Equal(false, changes["additionalProperties"])
	changeFields, ok := changes["properties"].(map[string]any)
	requirements.True(ok)
	for _, host := range []string{"endpoint", "poll_endpoint", "kind", "api_key_env", "credential"} {
		assertions.NotContains(changeFields, host)
	}
	opts.DelegatedOnly = true
	assertions.Empty(operationalCatalog(opts, true))
}
