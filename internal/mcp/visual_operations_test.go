package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVisualOperationsCatalogRequiresExactPolicyAndSeparateGate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	reads := []string{"get_visual_index_status", "get_document_vector_status"}
	writes := []string{"build_visual_index", "resume_visual_index", "retry_visual_attachment", "retire_visual_generation"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: append(reads, writes...)}
	defaults := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range reads {
		assertions.Contains(defaults, name)
	}
	for _, name := range writes {
		assertions.NotContains(defaults, name)
	}
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilyVisual}
	enabled := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range writes {
		requirements.Contains(enabled, name)
		input, ok := enabled[name]["inputSchema"].(map[string]any)
		requirements.True(ok)
		assertions.Equal(false, input["additionalProperties"])
		required, ok := input["required"].([]any)
		requirements.True(ok)
		for _, field := range []string{"expected_generation_id", "expected_generation_fingerprint", "expected_policy_fingerprint"} {
			assertions.Contains(required, field)
		}
		assertions.NotContains(toolsByName(t, rawListTools(t, opts, false)), name)
	}
	opts.DelegatedOnly = true
	assertions.Empty(operationalCatalog(opts, true))
}
