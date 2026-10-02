package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaneReadinessCatalogIsClosedReadOnlyAndNondelegated(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"get_lane_readiness"}}
	tools := toolsByName(t, rawListTools(t, opts, false))
	requirements.Contains(tools, "get_lane_readiness")
	tool := tools["get_lane_readiness"]
	annotations, ok := tool["annotations"].(map[string]any)
	requirements.True(ok)
	assertions.Equal(true, annotations["readOnlyHint"])
	schema, ok := tool["inputSchema"].(map[string]any)
	requirements.True(ok)
	assertions.Equal(false, schema["additionalProperties"])
	assertions.Empty(schema["properties"])
	opts.DelegatedOnly = true
	assertions.Empty(operationalCatalog(opts, true))
}
