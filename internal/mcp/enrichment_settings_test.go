package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnrichmentSettingsInputSchemaRejectsProviderSetup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	definitions := enrichmentSettingsDefinitions()
	require.Len(definitions, 2)
	assert.False(definitions[0].writes)
	assert.True(definitions[1].writes)
	assert.Equal(OperationFamilyEnrichmentSettings, definitions[1].family)
	schema, err := definitions[1].definition.inputSchema.Resolve(nil)
	require.NoError(err)
	assert.NoError(schema.Validate(map[string]any{"name": "example", "etag": `"revision"`, "enabled": false, "max_requests_per_day": float64(0)}))
	for _, field := range []string{"endpoint", "poll_endpoint", "credential", "target_keys", "allowed_identifiers", "kind"} {
		assert.Error(schema.Validate(map[string]any{"name": "example", "etag": `"revision"`, field: "example"}), field)
	}
	assert.Error(schema.Validate(map[string]any{"name": "example", "etag": `"revision"`, "max_retries": float64(-1)}))
}

func TestEnrichmentCatalogRequiresOwnWriteAllowance(t *testing.T) {
	assert := assert.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"get_enrichment_policy", "update_enrichment_policy"}}
	names := func(writes bool) map[string]bool {
		result := map[string]bool{}
		for _, d := range operationalCatalog(opts, writes) {
			result[d.definition.name] = true
		}
		return result
	}
	assert.Equal(map[string]bool{"get_enrichment_policy": true}, names(true))
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilySources}
	assert.Equal(map[string]bool{"get_enrichment_policy": true}, names(true))
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyEnrichmentSettings}
	assert.True(names(true)["update_enrichment_policy"])
	assert.False(names(false)["update_enrichment_policy"])
	opts.DelegatedOnly = true
	assert.Empty(names(true))
}
