package mcp

import (
	"context"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPeopleSweepCatalogGates(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	names := []string{"get_people_sweep_status", "list_people_sweep_history", "run_people_sweep"}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: names}
	tools := toolsByName(t, rawListTools(t, opts, true))
	assert.Contains(tools, names[0])
	assert.Contains(tools, names[1])
	assert.NotContains(tools, names[2])
	opts.OperationWriteFamilies = []OperationFamily{OperationFamilySweep}
	assert.Contains(toolsByName(t, rawListTools(t, opts, true)), names[2])
	assert.NotContains(toolsByName(t, rawListTools(t, opts, false)), names[2])
	opts.DelegatedOnly = true
	assert.Empty(operationalCatalog(opts, true))
	opts.DelegatedOnly = false
	opts.OperationCapabilities = nil
	assert.Empty(operationalCatalog(opts, true))
	opts.OperationCapabilities = names
	defs := operationalCatalog(opts, true)
	require.Len(defs, 3)
	assert.True(*defs[2].definition.annotations.OpenWorldHint)
}

type sweepApprovalBackend struct {
	calls      int
	disclosure string
}

func (b *sweepApprovalBackend) OperationDisclosure(_ context.Context, _ string, _ map[string]any) (string, error) {
	return b.disclosure, nil
}
func (b *sweepApprovalBackend) ExecuteOperation(_ context.Context, _ string, _ map[string]any) (*OperationResult, error) {
	b.calls++
	return &OperationResult{Output: SweepRun{}}, nil
}

func TestPeopleSweepApprovalIsPerCall(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	backend := &sweepApprovalBackend{disclosure: "Run the configured sweep"}
	opts := ServeOptions{Operations: backend, OperationCapabilities: []string{"run_people_sweep"}, OperationWriteFamilies: []OperationFamily{OperationFamilySweep}}
	server := httptest.NewServer(newMCPHTTPServer(opts, HTTPOptions{AllowWrites: true}).Handler)
	t.Cleanup(server.Close)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "sweep-test", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = session.Close() })
	call := func(state string, accept bool) *sdkmcp.CallToolResult {
		params := &sdkmcp.CallToolParams{Name: "run_people_sweep", Arguments: map[string]any{"person_id": 1}, RequestState: state}
		if accept {
			params.InputResponses = sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
		}
		result, err := session.CallTool(t.Context(), params)
		require.NoError(err)
		return result
	}
	first := call("", false)
	require.NotEmpty(first.RequestState)
	assert.Zero(backend.calls)
	backend.disclosure = "Changed sweep scope"
	assert.True(call(first.RequestState, true).IsError)
	assert.Zero(backend.calls)
	fresh := call("", false)
	require.NotEmpty(fresh.RequestState)
	assert.False(call(fresh.RequestState, true).IsError)
	assert.Equal(1, backend.calls)
	assert.True(call(fresh.RequestState, true).IsError)
	assert.Equal(1, backend.calls)
}
