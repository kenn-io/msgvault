package mcp

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
)

func TestMCPRefreshRetainsPendingConfirmation(t *testing.T) {
	backend := &countingOperationalBackend{}
	opts := ServeOptions{Operations: backend, OperationCapabilities: []string{"sync_source"}, OperationWriteFamilies: []OperationFamily{OperationFamilySources}}
	server := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, server.Shutdown(ctx))
	})
	previous, closePrevious := NewHTTPHandler(opts, HTTPOptions{AllowWrites: true})
	require.NoError(t, server.InstallMCPHandlers(previous, previous, closePrevious))
	call := func(state any) task3RPCResponse {
		params := map[string]any{"name": "sync_source", "arguments": map[string]any{"account": "sender@example.com", "source_type": "gmail"}, "_meta": modernRequestMeta()}
		if state != nil {
			params["requestState"] = state
			params["inputResponses"] = map[string]any{"confirm": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}}
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
		require.NoError(t, err)
		request := task3ModernRequest(t.Context(), "tools/call", "sync_source", string(body))
		request.URL.Path = "/mcp/writes"
		request.Header.Set("Authorization", "Bearer owner")
		request.Host = "localhost"
		request.RemoteAddr = "127.0.0.1:1234"
		response, result := task3Serve(server.Router(), request)
		task3RequireSuccess(t, response, result)
		return result
	}
	control := call(nil)
	require.NotEmpty(t, control.Result["requestState"])
	accepted := call(control.Result["requestState"])
	require.NotEqual(t, true, accepted.Result["isError"])
	require.Equal(t, int32(1), backend.calls.Load())
	pending := call(nil)
	require.NotEmpty(t, pending.Result["requestState"])
	next, closeNext := NewHTTPHandler(opts, HTTPOptions{AllowWrites: true})
	require.NoError(t, server.InstallMCPHandlers(next, next, closeNext))
	accepted = call(pending.Result["requestState"])
	assert.NotEqual(t, true, accepted.Result["isError"])
	assert.Equal(t, int32(2), backend.calls.Load())
}

func TestMCPRefreshRetainsChunkedDownload(t *testing.T) {
	fixture := newExportFixture(t)
	opts := ServeOptions{Engine: fixture.engine}
	server := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, server.Shutdown(ctx))
	})
	previous, closePrevious := NewHTTPHandler(opts, HTTPOptions{})
	require.NoError(t, server.InstallMCPHandlers(previous, nil, closePrevious))
	listener := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer owner")
		r.Host = "localhost"
		r.RemoteAddr = "127.0.0.1:1234"
		server.Router().ServeHTTP(w, r)
	})
	response, raw := task4RawRequest(t, listener, "tools/call", map[string]any{"name": ToolExportEML, "arguments": map[string]any{"id": fixture.withRaw, "offset": 0, "length": 100}}, nil)
	require.Empty(t, response.Error, "%s", raw)
	require.NotEqual(t, true, response.Result["isError"], "%s", raw)
	encoded, err := json.Marshal(response.Result["structuredContent"])
	require.NoError(t, err)
	var first exportChunkResp
	require.NoError(t, json.Unmarshal(encoded, &first))
	require.False(t, first.Complete)
	next, closeNext := NewHTTPHandler(opts, HTTPOptions{})
	require.NoError(t, server.InstallMCPHandlers(next, nil, closeNext))
	response, raw = task4RawRequest(t, listener, "tools/call", map[string]any{"name": ToolExportEML, "arguments": map[string]any{"id": fixture.withRaw, "offset": first.Length, "length": 100, "sha256": first.SHA256}}, nil)
	require.Empty(t, response.Error, "%s", raw)
	assert.NotEqual(t, true, response.Result["isError"], "%v", response.Result["content"])
}
