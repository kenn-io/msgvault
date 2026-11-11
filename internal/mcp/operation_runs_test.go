package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOperationReadsCatalogIsClosedReadOnlyAndOwnerOnly(t *testing.T) {
	names := []string{"get_operation_status", "list_operation_runs", "get_operation_run"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	tools := toolsByName(t, rawListTools(t, opts, false))
	for _, name := range names {
		assert.Contains(t, tools, name)
		annotations := tools[name]["annotations"].(map[string]any)
		assert.Equal(t, true, annotations["readOnlyHint"])
	}
	catalog := operationalCatalog(opts, false)
	require.Len(t, catalog, 3)
	for _, entry := range catalog {
		assert.Contains(t, names, entry.definition.name)
		assert.False(t, entry.writes)
		require.NotNil(t, entry.definition.inputSchema.AdditionalProperties)
		assert.NotNil(t, entry.definition.inputSchema.AdditionalProperties.Not)
	}
	opts.DelegatedOnly = true
	assert.Empty(t, operationalCatalog(opts, false))
}
