package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportCatalogUsesSourceWriteGateAndClosedInputs(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"create_import_job", "get_import_job"}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	requirements.Contains(tools, "get_import_job")
	assertions.NotContains(tools, "create_import_job")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilySources}
	tools = toolsByName(t, rawListTools(t, opts, true))
	requirements.Contains(tools, "create_import_job")
	schema, ok := tools["create_import_job"]["inputSchema"].(map[string]any)
	requirements.True(ok)
	assertions.Equal(false, schema["additionalProperties"])
	properties, ok := schema["properties"].(map[string]any)
	requirements.True(ok)
	assertions.Len(properties, 6)
	for _, forbidden := range []string{"path", "env", "command", "source_type", "cancel", "source_id"} {
		assertions.NotContains(properties, forbidden)
	}
	tools = toolsByName(t, rawListTools(t, opts, false))
	assertions.NotContains(tools, "create_import_job")
	requirements.Contains(tools, "get_import_job")
	opts.DelegatedOnly = true
	assertions.Empty(operationalCatalog(opts, true))
}
