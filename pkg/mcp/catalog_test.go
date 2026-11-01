package mcp_test

import (
	"context"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	catalog "go.kenn.io/msgvault/pkg/mcp"
)

func TestReadToolRejectsUnavailableOperations(t *testing.T) {
	for _, name := range []string{"", "unknown", "stage_deletion", "query_sql", "search_documents"} {
		t.Run(name, func(t *testing.T) {
			tool, err := catalog.ReadTool(name)
			require.Error(t, err)
			assert.Nil(t, tool)
		})
	}
}

func TestReadToolIndependentSDKContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// An embedder may customize its copy without changing other servers.
	customized, err := catalog.ReadTool("list_messages")
	require.NoError(err)
	input, ok := customized.InputSchema.(map[string]any)
	require.True(ok)
	properties, ok := input["properties"].(map[string]any)
	require.True(ok)
	limit, ok := properties["limit"].(map[string]any)
	require.True(ok)
	limit["type"] = "boolean"
	output, ok := customized.OutputSchema.(map[string]any)
	require.True(ok)
	output["required"] = []string{"changed"}
	customized.Annotations.ReadOnlyHint = false

	tool, err := catalog.ReadTool("list_messages")
	require.NoError(err)
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "catalog-test", Version: "1"}, nil)
	var calls atomic.Int32
	sdkmcp.AddTool(server, tool, func(_ context.Context, _ *sdkmcp.CallToolRequest, args map[string]any) (*sdkmcp.CallToolResult, map[string]any, error) {
		calls.Add(1)
		if args["limit"] == float64(3) {
			return nil, map[string]any{"invalid": true}, nil
		}
		return nil, map[string]any{"data": []any{}, "total": -1, "returned": 0, "offset": 0, "has_more": false}, nil
	})

	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := server.Connect(t.Context(), serverTransport, nil)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(serverSession.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "catalog-client", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(session.Close()) })

	listed, err := session.ListTools(t.Context(), nil)
	require.NoError(err)
	require.Len(listed.Tools, 1)
	assert.Equal("list_messages", listed.Tools[0].Name)
	require.NotNil(listed.Tools[0].Annotations)
	assert.True(listed.Tools[0].Annotations.ReadOnlyHint)

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "list_messages", Arguments: map[string]any{"limit": 2},
	})
	require.NoError(err)
	require.False(result.IsError)
	require.Len(result.Content, 1)
	content, ok := result.Content[0].(*sdkmcp.TextContent)
	require.True(ok)
	assert.JSONEq(`{"data":[],"total":-1,"returned":0,"offset":0,"has_more":false}`, content.Text)
	assert.Equal(int32(1), calls.Load())

	result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "list_messages", Arguments: map[string]any{"limit": "invalid"},
	})
	require.NoError(err)
	assert.True(result.IsError)
	assert.Equal(int32(1), calls.Load(), "invalid input must not reach the handler")

	_, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "list_messages", Arguments: map[string]any{"limit": 3},
	})
	require.ErrorContains(err, "validating tool output")
	assert.Equal(int32(2), calls.Load())
}
