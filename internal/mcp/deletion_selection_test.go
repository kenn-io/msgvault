package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDeletionSelectionCatalogRequiresOwnerAndCapability(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"explore_deletion_selection", "preflight_deletion_selection"}}
	tools := toolsByName(t, rawListTools(t, opts, false))
	assert.Contains(t, tools, "explore_deletion_selection")
	assert.Contains(t, tools, "preflight_deletion_selection")
	assert.NotContains(t, tools, ToolStageDeletion)
	opts.DelegatedOnly = true
	tools = toolsByName(t, rawListTools(t, opts, true))
	assert.NotContains(t, tools, "explore_deletion_selection")
	assert.NotContains(t, tools, "preflight_deletion_selection")
	opts.DelegatedOnly = false
	opts.OperationCapabilities = nil
	tools = toolsByName(t, rawListTools(t, opts, true))
	assert.NotContains(t, tools, "preflight_deletion_selection")
}

func TestDeletionSelectionRejectsMixedInputsBeforeAnyOperation(t *testing.T) {
	h := &handlers{operations: operationalTestBackend{}, operationCapabilities: []string{"stage_deletion_selection"}}
	result, err := h.stageDeletion(t.Context(), toolRequest{arguments: map[string]any{"selection": map[string]any{}, "operation_token": "token", "query": "example"}})
	assert.NoError(t, err)
	assert.True(t, result.isError)
	result, err = h.stageDeletion(t.Context(), toolRequest{arguments: map[string]any{"operation_token": "token", "query": "example"}})
	assert.NoError(t, err)
	assert.True(t, result.isError)
}
