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
	assert := assert.New(t)
	require := require.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"list_deletions", "get_deletion"}}
	definitions := operationalCatalog(opts, false)
	require.Len(definitions, 2)
	for _, definition := range definitions {
		assert.False(definition.writes)
		assert.True(definition.definition.annotations.ReadOnlyHint)
	}
	schema, err := definitions[0].definition.inputSchema.Resolve(nil)
	require.NoError(err)
	assert.NoError(schema.Validate(map[string]any{"status": "future_status"}))
	require.Error(schema.Validate(map[string]any{"status": "pending", "extra": true}))
	assert.Equal([]string{"batch_id"}, definitions[1].definition.inputSchema.Required)
	opts.OperationCapabilities = []string{"get_deletion"}
	assert.Len(operationalCatalog(opts, false), 1)
	opts.DelegatedOnly = true
	assert.Empty(operationalCatalog(opts, false))
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
	assert := assert.New(t)
	require := require.New(t)
	opts := ServeOptions{Operations: deletionReviewBackend{}, OperationCapabilities: []string{"list_deletions", "get_deletion"}}
	tools := toolsByName(t, rawListTools(t, opts, false))
	require.Contains(tools, "list_deletions")
	require.Contains(tools, "get_deletion")
	ctx := t.Context()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(opts, false).Connect(ctx, serverTransport, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = serverSession.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = session.Close() })
	for _, tc := range []struct {
		name string
		args map[string]any
	}{{"list_deletions", map[string]any{}}, {"get_deletion", map[string]any{"batch_id": "batch"}}} {
		result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		require.NoError(err)
		assert.False(result.IsError)
		assert.NotNil(result.StructuredContent)
	}
}
