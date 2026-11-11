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
	"go.kenn.io/msgvault/internal/daemonclient"
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

func TestOperationalWriteRecoveryFailureDoesNotExposeUncertainOutcome(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	local := &daemonclient.LocalDaemonUnavailableError{}
	result, err := operationalToolResponse("sync_source", &OperationResult{
		IsError: true,
		Output: map[string]any{
			"error":                        "daemon_unavailable",
			"operation_may_have_completed": false,
		},
	}, local)
	requirements.NoError(err)
	requirements.NotNil(result)
	assertions.True(result.isError)
	assertions.Equal("daemon_unavailable: "+daemonclient.LocalDaemonUnavailableMessage, result.text)
	assertions.JSONEq(`{"error":"daemon_unavailable","operation_may_have_completed":false}`, string(result.structuredContent))
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
	return &OperationResult{Output: map[string]any{"status": "accepted", "message": "scheduled", "disposition": "pending"}}, nil
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
	accepted := call(fresh.RequestState, true)
	assertions.False(accepted.IsError)
	assertions.Equal(map[string]any{"status": "accepted", "message": "scheduled", "disposition": "pending"}, accepted.StructuredContent)
	assertions.Equal(int32(1), backend.calls.Load())
	assertions.True(call(fresh.RequestState, true).IsError)
	assertions.Equal(int32(1), backend.calls.Load())
}

func TestLoopCatalogGatesSchemasAndAnnotations(t *testing.T) {
	for _, tc := range []struct {
		name      string
		names     []string
		family    OperationFamily
		openWorld bool
	}{
		{"loops", []string{"list_open_loops", "close_open_loop", "snooze_open_loop", "reopen_open_loop"}, OperationFamilyLoops, false},
		{"visual", []string{"get_visual_attachment_status", "start_visual_attachment_build", "resume_visual_attachment_build", "retry_visual_attachment_owner", "retire_visual_attachment_generation"}, OperationFamilyVisual, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: tc.names, OperationWriteFamilies: []OperationFamily{tc.family}}
			catalog := operationalCatalog(opts, true)
			again := operationalCatalog(opts, true)
			require.Len(t, catalog, len(tc.names))
			assert.Len(t, operationalCatalog(opts, false), 1)
			other := OperationFamilyLoops
			if tc.family == OperationFamilyLoops {
				other = OperationFamilyVisual
			}
			opts.OperationWriteFamilies = []OperationFamily{other}
			assert.Len(t, operationalCatalog(opts, true), 1)
			opts.OperationWriteFamilies = []OperationFamily{tc.family}
			tools := toolsByName(t, rawListTools(t, opts, true))
			for i, definition := range catalog {
				assert.Equal(t, tc.names[i], definition.definition.name)
				assert.Same(t, definition.definition.inputSchema, again[i].definition.inputSchema)
				assert.Same(t, definition.definition.outputSchema, again[i].definition.outputSchema)
				require.NotNil(t, definition.definition.annotations.OpenWorldHint)
				if i > 0 {
					wantOpen := tc.openWorld && definition.definition.name != "retire_visual_attachment_generation"
					assert.Equal(t, wantOpen, *definition.definition.annotations.OpenWorldHint)
				}
				if definition.definition.name == "retire_visual_attachment_generation" {
					require.NotNil(t, definition.definition.annotations.DestructiveHint)
					assert.True(t, *definition.definition.annotations.DestructiveHint)
				}
				tool, ok := tools[tc.names[i]]
				require.True(t, ok)
				schema, ok := tool["inputSchema"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, false, schema["additionalProperties"])
			}
			opts.DelegatedOnly = true
			assert.Empty(t, operationalCatalog(opts, true))
		})
	}
	opts := ServeOptions{Operations: operationalTestBackend{}, OperationCapabilities: []string{"sync_source"}, OperationWriteFamilies: []OperationFamily{OperationFamilySources}}
	catalog := operationalCatalog(opts, true)
	require.Len(t, catalog, 1)
	require.NotNil(t, catalog[0].definition.annotations.OpenWorldHint)
	assert.True(t, *catalog[0].definition.annotations.OpenWorldHint)
	opts.OperationCapabilities = []string{"sync_source", "close_open_loop", "start_visual_attachment_build"}
	for _, tc := range []struct {
		family                    OperationFamily
		present, absent1, absent2 string
	}{
		{OperationFamilySources, "sync_source", "close_open_loop", "start_visual_attachment_build"},
		{OperationFamilyLoops, "close_open_loop", "sync_source", "start_visual_attachment_build"},
		{OperationFamilyVisual, "start_visual_attachment_build", "sync_source", "close_open_loop"},
	} {
		opts.OperationWriteFamilies = []OperationFamily{tc.family}
		tools := toolsByName(t, rawListTools(t, opts, true))
		assert.Contains(t, tools, tc.present)
		assert.NotContains(t, tools, tc.absent1)
		assert.NotContains(t, tools, tc.absent2)
	}
}
