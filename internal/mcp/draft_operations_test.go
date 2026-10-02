package mcp

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDraftCatalogHasClosedInputsAndSeparateWriteGate(t *testing.T) {
	names := []string{"draft_reply", "draft_compose", "draft_forward", "get_draft", "list_conversation_drafts", "edit_draft", "delete_draft", "recover_draft", "list_draft_send_as"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names, OperationWriteFamilies: []OperationFamily{OperationFamilyDrafts}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		assert.Contains(t, tools, name)
	}
	read := toolsByName(t, rawListTools(t, opts, false))
	for _, name := range []string{"get_draft", "list_conversation_drafts", "list_draft_send_as"} {
		assert.Contains(t, read, name)
	}
	for _, name := range []string{"draft_reply", "draft_compose", "draft_forward", "edit_draft", "delete_draft", "recover_draft"} {
		assert.NotContains(t, read, name)
	}
	opts.DelegatedOnly = true
	delegated := toolsByName(t, rawListTools(t, opts, true))
	assert.NotContains(t, delegated, "draft_forward")
	assert.NotContains(t, delegated, "list_draft_send_as")
	roots := operationalCatalog(opts, true)
	require.NotEmpty(t, roots)
	for i, root := range operationalCatalog(opts, true) {
		assert.Same(t, roots[i].definition.inputSchema, root.definition.inputSchema)
		assert.Same(t, roots[i].definition.outputSchema, root.definition.outputSchema)
	}
}

type withheldDraftBackend struct{ operationalTestBackend }

func (withheldDraftBackend) ExecuteOperation(context.Context, string, map[string]any) (*OperationResult, error) {
	return &OperationResult{Output: map[string]any{"status": "ok", "draft_id": "draft_example", "revision": 2, "source_id": 3, "chat_id": "chat_example", "content": nil}}, nil
}
func TestDraftSDKPreservesNullContent(t *testing.T) {
	opts := ServeOptions{Operations: withheldDraftBackend{}, OperationCapabilities: []string{"get_draft"}}
	result := rawCallTool(t, opts, "get_draft", map[string]any{"draft_id": "draft_example"})
	assert.NotEqual(t, true, result["isError"])
	output, ok := result["structuredContent"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, output, "content")
	assert.Nil(t, output["content"])
}
