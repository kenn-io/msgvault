package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOperationReadsCatalogIsClosedAndReadOnly(t *testing.T) {
	assert := assert.New(t)
	names := []string{"get_operation_status", "list_operation_runs", "get_operation_run"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	tools := toolsByName(t, rawListTools(t, opts, false))
	for _, name := range names {
		assert.Contains(tools, name)
		annotations := tools[name]["annotations"].(map[string]any)
		assert.Equal(true, annotations["readOnlyHint"])
	}
	for _, name := range names {
		schema := tools[name]["inputSchema"].(map[string]any)
		assert.Equal(false, schema["additionalProperties"])
	}
	opts.DelegatedOnly = true
	delegated := toolsByName(t, rawListTools(t, opts, false))
	for _, name := range names {
		assert.NotContains(delegated, name)
	}
}
