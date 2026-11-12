package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
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

func TestDeletionSelectionStageSchemaCapability(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	require.Contains(t, tools, ToolStageDeletion)
	assert.NotContains(t, toolPropertyNames(t, tools[ToolStageDeletion]), "selection")
	opts.OperationCapabilities = []string{"stage_deletion_selection"}
	tools = toolsByName(t, rawListTools(t, opts, true))
	require.Contains(t, tools, ToolStageDeletion)
	assert.Contains(t, toolPropertyNames(t, tools[ToolStageDeletion]), "selection")
	assert.Contains(t, toolPropertyNames(t, tools[ToolStageDeletion]), "operation_token")
}

type deletionExploreCapture struct{ args map[string]any }

func (b *deletionExploreCapture) ExecuteOperation(_ context.Context, _ string, args map[string]any) (*OperationResult, error) {
	b.args = args
	return &OperationResult{Output: map[string]any{}}, nil
}
func (*deletionExploreCapture) OperationDisclosure(context.Context, string, map[string]any) (string, error) {
	return "", nil
}

func TestDeletionExploreCollectionResolution(t *testing.T) {
	e := &collectionTestEngine{Engine: &querytest.MockEngine{}, collections: []query.CollectionScope{{Name: "one", SourceIDs: []int64{42}}, {Name: "empty", SourceIDs: []int64{}}}}
	backend := &deletionExploreCapture{}
	h := newTestHandlers(e)
	h.operations = backend
	result, err := h.exploreDeletionSelection(t.Context(), toolRequest{arguments: map[string]any{"collection": "one"}})
	require.NoError(t, err)
	require.False(t, result.isError)
	assert.Equal(t, []any{map[string]any{"dimension": "source", "values": []string{"42"}}}, backend.args["filters"])
	assert.NotContains(t, backend.args, "collection")
	for _, args := range []map[string]any{
		{"collection": "empty"}, {"collection": "missing"},
		{"collection": "one", "filters": []any{map[string]any{"dimension": "account", "values": []string{"a"}}}},
		{"collection": "one", "filters": []any{map[string]any{"dimension": "source", "values": []string{"42"}}}},
	} {
		backend.args = nil
		result, err = h.exploreDeletionSelection(t.Context(), toolRequest{arguments: args})
		require.NoError(t, err)
		assert.True(t, result.isError)
		assert.Nil(t, backend.args)
	}
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
