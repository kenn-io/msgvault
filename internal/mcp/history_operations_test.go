package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOperationHistoryCatalogUsesOpaqueIDsAndOwnerReads(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	names := []string{"list_operation_runs", "get_operation_run", "get_operation_status"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	tools := toolsByName(t, rawListTools(t, opts, false))
	for _, name := range names {
		requirements.Contains(tools, name)
		annotations, ok := tools[name]["annotations"].(map[string]any)
		requirements.True(ok)
		assertions.Equal(true, annotations["readOnlyHint"])
	}
	schema, ok := tools["get_operation_run"]["inputSchema"].(map[string]any)
	requirements.True(ok)
	properties, ok := schema["properties"].(map[string]any)
	requirements.True(ok)
	id, ok := properties["run_id"].(map[string]any)
	requirements.True(ok)
	assertions.Equal("string", id["type"])
	assertions.Equal(false, schema["additionalProperties"])
	opts.DelegatedOnly = true
	assertions.Empty(operationalCatalog(opts, true))
}
