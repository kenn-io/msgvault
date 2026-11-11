package mcp

import (
	"context"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestDeletionReviewCatalogReadOnly(t *testing.T) {
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"list_deletions", "get_deletion"}}
	definitions := operationalCatalog(opts, false)
	require.Len(t, definitions, 2)
	for _, definition := range definitions {
		assert.False(t, definition.writes)
		assert.True(t, definition.definition.annotations.ReadOnlyHint)
	}
	assert.Equal(t, []any{"pending", "in_progress", "completed", "failed", "cancelled"}, definitions[0].definition.inputSchema.Properties["status"].Enum)
	assert.Equal(t, []string{"batch_id"}, definitions[1].definition.inputSchema.Required)
	opts.OperationCapabilities = []string{"get_deletion"}
	assert.Len(t, operationalCatalog(opts, false), 1)
	opts.DelegatedOnly = true
	assert.Empty(t, operationalCatalog(opts, false))
}

type deletionReviewBackend struct{}

func (deletionReviewBackend) ExecuteOperation(_ context.Context, name string, _ map[string]any) (*OperationResult, error) {
	if name == "list_deletions" {
		return &OperationResult{Output: generated.ListDeletionsResponse{Manifests: []generated.DeletionManifestSummary{}}}, nil
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &OperationResult{Output: generated.DeletionManifestDetail{ID: "batch", Status: "failed", CreatedAt: now, CreatedBy: "cli", Description: "batch", Summary: &generated.Summary{MessageCount: 1, DateRange: []string{"2026-01-01", "2026-01-01"}}, Execution: &generated.Execution{StartedAt: now, Method: "trash", Failed: 1, FailedIds: []string{"synthetic-1"}}, Source: &generated.SourceReference{ID: 1, Type: "gmail", Identifier: "owner@example.test"}}}, nil
}
func (deletionReviewBackend) OperationDisclosure(context.Context, string, map[string]any) (string, error) {
	return "", nil
}

func TestDeletionReviewSDKReadOnly(t *testing.T) {
	opts := ServeOptions{Operations: deletionReviewBackend{}, OperationCapabilities: []string{"list_deletions", "get_deletion"}}
	tools := toolsByName(t, rawListTools(t, opts, false))
	require.Contains(t, tools, "list_deletions")
	require.Contains(t, tools, "get_deletion")
	ctx := t.Context()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(opts, false).Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	for _, tc := range []struct {
		name string
		args map[string]any
	}{{"list_deletions", map[string]any{}}, {"get_deletion", map[string]any{"batch_id": "batch"}}} {
		result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		require.NoError(t, err)
		assert.False(t, result.IsError)
		assert.NotNil(t, result.StructuredContent)
	}
}
