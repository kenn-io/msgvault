package mcp

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
)

func TestNativeMCPRefreshPreservesPendingConfirmation(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	backend := &countingOperationalBackend{}
	opts := ServeOptions{Operations: backend, OperationCapabilities: []string{"sync_source"}, OperationWriteFamilies: []OperationFamily{OperationFamilySources}}
	server := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { checks.NoError(server.Shutdown(context.Background())) })
	var latest http.Handler
	install := func() {
		read, closeRead := NewHTTPHandler(opts, HTTPOptions{})
		write, closeWrite := NewHTTPHandler(opts, HTTPOptions{AllowWrites: true})
		must.NoError(server.InstallMCPHandlers(read, write, func() { closeRead(); closeWrite() }))
		latest = write
	}
	call := func(handler http.Handler, state any) task3RPCResponse {
		t.Helper()
		params := map[string]any{"name": "sync_source", "arguments": map[string]any{"account": "sender@example.com", "source_type": "gmail"}, "_meta": modernRequestMeta()}
		if state != nil {
			params["requestState"] = state
			params["inputResponses"] = map[string]any{"confirm": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}}
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
		must.NoError(err)
		request := task3ModernRequest("tools/call", "sync_source", string(body))
		request.URL.Path = "/mcp/writes"
		request.Header.Set("Authorization", "Bearer owner")
		response, result := task3Serve(handler, request)
		task3RequireSuccess(t, response, result)
		return result
	}
	install()
	pending := call(server.Router(), nil)
	must.NotEmpty(pending.Result["requestState"])
	checks.Zero(backend.calls.Load())
	install()
	accepted := call(server.Router(), pending.Result["requestState"])
	checks.NotEqual(true, accepted.Result["isError"])
	checks.Equal(int32(1), backend.calls.Load())
	pending = call(server.Router(), nil)
	must.NotEmpty(pending.Result["requestState"])
	must.NoError(server.Shutdown(t.Context()))
	refused := call(latest, pending.Result["requestState"])
	checks.Equal(true, refused.Result["isError"], "shutdown releases pending confirmation challenges")
	checks.Equal(int32(1), backend.calls.Load())
}

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
