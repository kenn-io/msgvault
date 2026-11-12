package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestDeletionSelectionStageSchemaCapability(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	require.Contains(t, tools, ToolStageDeletion)
	assert.NotContains(t, toolPropertyNames(t, tools[ToolStageDeletion]), "selection")
	assert.NotContains(t, tools[ToolStageDeletion]["description"], "Alternatively pass")
	output, ok := tools[ToolStageDeletion]["outputSchema"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, output, "anyOf")
	opts.OperationCapabilities = []string{"explore_deletion_selection", "preflight_deletion_selection", "stage_deletion_selection"}
	tools = toolsByName(t, rawListTools(t, opts, true))
	assert.Contains(t, tools, "explore_deletion_selection")
	assert.Contains(t, tools, "preflight_deletion_selection")
	require.Contains(t, tools, ToolStageDeletion)
	assert.Contains(t, toolPropertyNames(t, tools[ToolStageDeletion]), "selection")
	assert.Contains(t, toolPropertyNames(t, tools[ToolStageDeletion]), "operation_token")
	assert.Contains(t, tools[ToolStageDeletion]["description"], "Alternatively pass")
	output, ok = tools[ToolStageDeletion]["outputSchema"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, output, "anyOf")
}

type deletionExploreCapture struct{ args map[string]any }

func (b *deletionExploreCapture) ExecuteOperation(_ context.Context, _ string, args map[string]any) (*OperationResult, error) {
	b.args = args
	return &OperationResult{Output: generated.ExploreHTTPResponse{}}, nil
}
func (*deletionExploreCapture) OperationDisclosure(context.Context, string, map[string]any) (string, error) {
	return "", nil
}

func TestDeletionExploreCollectionResolution(t *testing.T) {
	e := &collectionTestEngine{Engine: &querytest.MockEngine{}, collections: []query.CollectionScope{{Name: "one", SourceIDs: []int64{42}}, {Name: "empty", SourceIDs: []int64{}}}}
	backend := &deletionExploreCapture{}
	h := newTestHandlers(e)
	h.operations = backend
	result, err := h.exploreDeletionSelection(t.Context(), toolRequest{arguments: map[string]any{"collection": "one", "cursor": "opaque"}})
	require.NoError(t, err)
	require.False(t, result.isError)
	assert.Equal(t, []any{map[string]any{"dimension": "source", "values": []string{"42"}}}, backend.args["filters"])
	assert.NotContains(t, backend.args, "collection")
	var response struct {
		Predicate map[string]any `json:"predicate"`
	}
	require.NoError(t, json.Unmarshal(result.structuredContent, &response))
	assert.Equal(t, []any{map[string]any{"dimension": "source", "values": []any{"42"}}}, response.Predicate["filters"])
	assert.NotContains(t, response.Predicate, "collection")
	assert.NotContains(t, response.Predicate, "cursor")
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
	h := &handlers{operations: operationalTestBackend{}}
	result, err := h.stageDeletion(t.Context(), toolRequest{arguments: map[string]any{"selection": map[string]any{}, "operation_token": "token", "query": "example"}})
	require.NoError(t, err)
	assert.True(t, result.isError)
	result, err = h.stageDeletion(t.Context(), toolRequest{arguments: map[string]any{"operation_token": "token", "query": "example"}})
	require.NoError(t, err)
	assert.True(t, result.isError)
}
