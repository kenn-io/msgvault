package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDraftCatalogHasClosedInputsAndSeparateWriteGate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	names := []string{"draft_reply", "draft_compose", "draft_forward", "get_draft", "list_conversation_drafts", "edit_draft", "delete_draft", "recover_draft", "list_draft_send_as"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names, OperationWriteFamilies: []OperationFamily{OperationFamilyDrafts}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range names {
		assertions.Contains(tools, name)
	}
	read := toolsByName(t, rawListTools(t, opts, false))
	for _, name := range []string{"get_draft", "list_conversation_drafts", "list_draft_send_as"} {
		assertions.Contains(read, name)
	}
	for _, name := range []string{"draft_reply", "draft_compose", "draft_forward", "edit_draft", "delete_draft", "recover_draft"} {
		assertions.NotContains(read, name)
	}
	opts.DelegatedOnly = true
	delegated := toolsByName(t, rawListTools(t, opts, true))
	assertions.NotContains(delegated, "draft_forward")
	assertions.NotContains(delegated, "list_draft_send_as")
	roots := operationalCatalog(opts, true)
	requirements.NotEmpty(roots)
	for i, root := range operationalCatalog(opts, true) {
		assertions.Same(roots[i].definition.inputSchema, root.definition.inputSchema)
		assertions.Same(roots[i].definition.outputSchema, root.definition.outputSchema)
	}
}

type withheldDraftBackend struct{ operationalTestBackend }

func (withheldDraftBackend) ExecuteOperation(context.Context, string, map[string]any) (*OperationResult, error) {
	return &OperationResult{Output: map[string]any{"status": "ok", "draft_id": "draft_example", "revision": 2, "source_id": 3, "chat_id": "chat_example", "content": nil}}, nil
}
func TestDraftSDKPreservesNullContent(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	opts := ServeOptions{Operations: withheldDraftBackend{}, OperationCapabilities: []string{"get_draft"}}
	result := rawCallTool(t, opts, "get_draft", map[string]any{"draft_id": "draft_example"})
	assertions.NotEqual(true, result["isError"])
	output, ok := result["structuredContent"].(map[string]any)
	requirements.True(ok)
	assertions.Contains(output, "content")
	assertions.Nil(output["content"])
}
