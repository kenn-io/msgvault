package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportJobCatalogGatesAndSchemas(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"create_import_job", "get_import_job"}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	assert.Contains(t, tools, "get_import_job")
	assert.NotContains(t, tools, "create_import_job")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyImports}
	defs := operationalCatalog(opts, true)
	require.Len(t, defs, 2)
	assert.True(t, *defs[0].definition.annotations.OpenWorldHint)
	assert.Contains(t, defs[0].definition.inputSchema.Required, "limit")
	assert.Equal(t, new(float64(1)), defs[0].definition.inputSchema.Properties["limit"].Minimum)
	assert.Len(t, operationalCatalog(opts, false), 1)
	opts.DelegatedOnly = true
	assert.Empty(t, operationalCatalog(opts, true))
}
