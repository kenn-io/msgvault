package mcp

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPHTTPConfirmationIsBoundToAuthenticatedListener(t *testing.T) {
	for _, otherKey := range []string{"key-a", "key-b"} {
		t.Run(otherKey, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			backend := &countingOperationalBackend{}
			opts := ServeOptions{Operations: backend, OperationCapabilities: []string{"sync_source"}, OperationWriteFamilies: []OperationFamily{OperationFamilySources}}
			issuer, closeIssuer := NewHTTPHandler(opts, HTTPOptions{APIKey: "key-a", AllowWrites: true})
			other, closeOther := NewHTTPHandler(opts, HTTPOptions{APIKey: otherKey, AllowWrites: true})
			defer closeIssuer()
			defer closeOther()
			call := func(handler http.Handler, key string, state any) (*httptest.ResponseRecorder, task3RPCResponse) {
				t.Helper()
				params := map[string]any{"name": "sync_source", "arguments": map[string]any{"account": "sender@example.com", "source_type": "gmail"}, "_meta": modernRequestMeta()}
				if state != nil {
					params["requestState"] = state
					params["inputResponses"] = map[string]any{"confirm": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}}
				}
				body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
				must.NoError(err)
				request := task3ModernRequest("tools/call", "sync_source", string(body))
				request.Header.Set("Authorization", "Bearer "+key)
				return task3Serve(handler, request)
			}
			response, pending := call(issuer, "key-a", nil)
			task3RequireSuccess(t, response, pending)
			state := pending.Result["requestState"]
			must.NotEmpty(state)
			checks.Zero(backend.calls.Load())
			response, refused := call(other, otherKey, state)
			task3RequireSuccess(t, response, refused)
			checks.Equal(true, refused.Result["isError"])
			checks.Zero(backend.calls.Load())
			response, accepted := call(issuer, "key-a", state)
			task3RequireSuccess(t, response, accepted)
			checks.NotEqual(true, accepted.Result["isError"])
			checks.Equal(int32(1), backend.calls.Load())
			response, replay := call(issuer, "key-a", state)
			task3RequireSuccess(t, response, replay)
			checks.Equal(true, replay.Result["isError"])
			checks.Equal(int32(1), backend.calls.Load())
			response, pending = call(issuer, "key-a", nil)
			task3RequireSuccess(t, response, pending)
			must.NotEmpty(pending.Result["requestState"])
			closeIssuer()
			closeIssuer()
			response, refused = call(issuer, "key-a", pending.Result["requestState"])
			task3RequireSuccess(t, response, refused)
			checks.Equal(true, refused.Result["isError"])
			checks.Equal(int32(1), backend.calls.Load())
		})
	}
}

func TestDelegatedCatalogHasNoOwnerArchiveToolsOrResources(t *testing.T) {
	opts := ServeOptions{DelegatedOnly: true}
	assert.Empty(t, rawListTools(t, opts, true))
	response := rawModernCall(t, opts, HTTPOptions{}, "server/discover", nil)
	capabilities, ok := response.Result["capabilities"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, capabilities, "resources")
}
