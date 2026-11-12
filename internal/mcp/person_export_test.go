package mcp

import (
	"context"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type personExportTestBackend struct{}

func (personExportTestBackend) ExecuteOperation(context.Context, string, map[string]any) (*OperationResult, error) {
	return &OperationResult{Output: PersonExport{JSONL: "synthetic\n"}}, nil
}
func (personExportTestBackend) OperationDisclosure(context.Context, string, map[string]any) (string, error) {
	return "", nil
}

func TestPersonExportSDKReadWithoutWriteOptIn(t *testing.T) {
	opts := ServeOptions{Operations: personExportTestBackend{}, OperationCapabilities: []string{"export_person_messages"}}
	server := httptest.NewServer(newMCPHTTPServer(opts, HTTPOptions{}).Handler)
	t.Cleanup(server.Close)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "export_person_messages", Arguments: map[string]any{"person_id": 1, "start": "2026-07-20T00:00:00Z", "end": "2026-07-21T00:00:00Z"}})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Equal(t, map[string]any{"jsonl": "synthetic\n"}, result.StructuredContent)
}
