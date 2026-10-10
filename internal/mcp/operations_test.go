package mcp

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type operationalTestBackend struct{}

func (operationalTestBackend) ExecuteOperation(context.Context, string, map[string]any) (*OperationResult, error) {
	return &OperationResult{Output: map[string]any{"running": false, "accounts": []any{}}}, nil
}
func (operationalTestBackend) OperationDisclosure(context.Context, string, map[string]any) (string, error) {
	return "Sync the selected configured source", nil
}

func TestOperationalCatalogUsesFixedNamesAndWriteGates(t *testing.T) {
	assertions := assert.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"get_source_scheduler_status", "sync_source", "arbitrary_command"}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	assertions.Contains(tools, "get_source_scheduler_status")
	assertions.NotContains(tools, "sync_source")
	assertions.NotContains(tools, "arbitrary_command")
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilySources}
	assertions.Contains(toolsByName(t, rawListTools(t, opts, true)), "sync_source")
	assertions.NotContains(toolsByName(t, rawListTools(t, opts, false)), "sync_source")
	opts.DelegatedOnly = true
	assertions.Empty(rawListTools(t, opts, true))
}

func TestOperationalCatalogSchemaRootsAreStable(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"get_source_scheduler_status"}}
	first := operationalCatalog(opts, true)
	second := operationalCatalog(opts, true)
	requirements.Len(first, 1)
	requirements.Len(second, 1)
	assertions.Same(first[0].definition.inputSchema, second[0].definition.inputSchema)
	assertions.Same(first[0].definition.outputSchema, second[0].definition.outputSchema)
}

func TestOperationalReadRetainsTypedStructuredOutput(t *testing.T) {
	assertions := assert.New(t)
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"get_source_scheduler_status"}}
	result := rawCallTool(t, opts, "get_source_scheduler_status", map[string]any{})
	assertions.Equal(map[string]any{"running": false, "accounts": []any{}}, result["structuredContent"])
}

type countingOperationalBackend struct {
	policy atomic.Int32
	calls  atomic.Int32
}

func (b *countingOperationalBackend) OperationDisclosure(context.Context, string, map[string]any) (string, error) {
	return fmt.Sprintf("Synchronize synthetic configured source under policy %d", b.policy.Load()), nil
}

func (b *countingOperationalBackend) ExecuteOperation(context.Context, string, map[string]any) (*OperationResult, error) {
	b.calls.Add(1)
	return &OperationResult{Output: map[string]any{"status": "accepted", "message": "scheduled"}}, nil
}

func TestOperationalWriteConfirmsCurrentDisclosureBeforeExecution(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend := &countingOperationalBackend{}
	opts := ServeOptions{Operations: backend, OperationCapabilities: []string{"sync_source"}, OperationWriteFamilies: []OperationFamily{OperationFamilySources}}
	server := httptest.NewServer(newMCPHTTPServer(opts, HTTPOptions{AllowWrites: true}).Handler)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-client", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	requirements.NoError(err)
	t.Cleanup(func() { _ = session.Close() })
	call := func(state string, accepted bool) *sdkmcp.CallToolResult {
		params := &sdkmcp.CallToolParams{Name: "sync_source", Arguments: map[string]any{"account": "sender@example.com", "source_type": "gmail"}, RequestState: state}
		if accepted {
			params.InputResponses = sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
		}
		result, err := session.CallTool(ctx, params)
		requirements.NoError(err)
		requirements.NotNil(result)
		return result
	}
	first := call("", false)
	requirements.NotEmpty(first.RequestState)
	assertions.Equal(int32(0), backend.calls.Load())
	backend.policy.Store(1)
	assertions.True(call(first.RequestState, true).IsError)
	assertions.Equal(int32(0), backend.calls.Load())
	fresh := call("", false)
	requirements.NotEmpty(fresh.RequestState)
	assertions.False(call(fresh.RequestState, true).IsError)
	assertions.Equal(int32(1), backend.calls.Load())
	assertions.True(call(fresh.RequestState, true).IsError)
	assertions.Equal(int32(1), backend.calls.Load())
}
