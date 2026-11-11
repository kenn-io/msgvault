package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeRecoveryCatalogGatesAndClosedSchemas(t *testing.T) {
	names := []string{"list_person_merges", "get_person_merge", "get_person_merge_snapshot", "decide_person_merge_candidate", "split_person_merge"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names[:3] {
		assert.Contains(t, tools, name)
	}
	for _, name := range names[3:] {
		assert.NotContains(t, tools, name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyMergeRecovery}
	tools = toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		assert.Contains(t, tools, name)
	}
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, false)), names[3])
	opts.DelegatedOnly = true
	assert.Empty(t, rawListTools(t, opts, true))
	opts.DelegatedOnly = false
	opts.OperationCapabilities = nil
	assert.Empty(t, operationalCatalog(opts, true))
	for _, def := range mergeRecoveryDefinitions() {
		require.NotNil(t, def.definition.inputSchema.AdditionalProperties)
		assert.NotNil(t, def.definition.inputSchema.AdditionalProperties.Not)
	}
}
