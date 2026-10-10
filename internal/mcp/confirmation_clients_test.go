package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type confirmationBearerTransport struct{}

func (confirmationBearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header = request.Header.Clone()
	request.Header.Set("Authorization", "Bearer synthetic-key")
	return http.DefaultTransport.RoundTrip(request)
}

func TestConfirmationHTTPClientsCannotTransferApproval(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	clients := newConfirmationClients()
	manager := newConfirmationChallenges()
	var calls atomic.Int32
	transport := sdkmcp.NewStreamableHTTPHandler(func(request *http.Request) *sdkmcp.Server {
		server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "confirmation-test", Version: "1"}, nil)
		sdkmcp.AddTool[map[string]any, any](server, &sdkmcp.Tool{Name: "synthetic_write", InputSchema: closedObject(nil), OutputSchema: closedObject(nil)}, officialToolHandler(func(ctx context.Context, req toolRequest) (*toolResult, error) {
			if err := req.confirmUserAction(ctx, "Perform this synthetic action"); err != nil {
				return confirmationToolError(err)
			}
			calls.Add(1)
			return jsonResult(map[string]any{})
		}, confirmationConfig{manager: manager, sessionKey: confirmationClientKey(request.Context()), requireSessionKey: true}))
		return server
	}, &sdkmcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	server := httptest.NewServer(bearerAuthHandler("synthetic-key", clients.middleware(transport)))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connect := func(name string) *sdkmcp.ClientSession {
		client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: name, Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
		session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: &http.Client{Transport: confirmationBearerTransport{}}}, nil)
		requirements.NoError(err)
		t.Cleanup(func() { _ = session.Close() })
		return session
	}
	a, b := connect("client-a"), connect("client-b")
	call := func(session *sdkmcp.ClientSession, state string, confirm bool) *sdkmcp.CallToolResult {
		params := &sdkmcp.CallToolParams{Name: "synthetic_write", Arguments: map[string]any{}, RequestState: state}
		if confirm {
			params.InputResponses = sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
		}
		result, err := session.CallTool(ctx, params)
		requirements.NoError(err)
		requirements.NotNil(result)
		return result
	}
	first := call(a, "", false)
	requirements.NotEmpty(first.RequestState)
	requirements.Contains(first.InputRequests, "confirm")
	assertions.Equal(int32(0), calls.Load())
	transferred := call(b, first.RequestState, true)
	assertions.True(transferred.IsError)
	assertions.Equal(int32(0), calls.Load())
	fresh := call(a, "", false)
	requirements.NotEmpty(fresh.RequestState)
	accepted := call(a, fresh.RequestState, true)
	assertions.False(accepted.IsError)
	assertions.Equal(int32(1), calls.Load())
	replay := call(a, fresh.RequestState, true)
	assertions.True(replay.IsError)
	assertions.Equal(int32(1), calls.Load())
	assertions.Len(clients.active, 2)
}

func TestConfirmationHTTPClientAdmissionIsBoundedAndAuthenticated(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	clients := newConfirmationClients()
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	clients.now = func() time.Time { return now }
	handler := bearerAuthHandler("synthetic-key", clients.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.NotEmpty(confirmationClientKey(r.Context()))
		w.WriteHeader(http.StatusNoContent)
	})))
	invoke := func(key, identity string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		if identity != "" {
			request.Header.Set("Mcp-Session-Id", identity)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	unauthorized := invoke("", "")
	assertions.Equal(http.StatusUnauthorized, unauthorized.Code)
	assertions.Empty(clients.active)
	first := invoke("synthetic-key", "")
	requirements.Equal(http.StatusNoContent, first.Code)
	id := first.Header().Get("Mcp-Session-Id")
	requirements.NotEmpty(id)
	assertions.Equal(http.StatusNoContent, invoke("synthetic-key", id).Code)
	assertions.Equal(http.StatusNotFound, invoke("synthetic-key", "caller-chosen-id").Code)
	for range maxConfirmationClients - 1 {
		requirements.Equal(http.StatusNoContent, invoke("synthetic-key", "").Code)
	}
	assertions.Equal(http.StatusServiceUnavailable, invoke("synthetic-key", "").Code)
	assertions.Len(clients.active, maxConfirmationClients)
	assertions.Equal(http.StatusNoContent, invoke("synthetic-key", id).Code)
	now = now.Add(confirmationClientIdleTTL)
	assertions.Equal(http.StatusNotFound, invoke("synthetic-key", id).Code)
	assertions.Equal(http.StatusNoContent, invoke("synthetic-key", "").Code)
	assertions.Len(clients.active, 1)
	clients.close()
	assertions.Empty(clients.active)
	closed := invoke("synthetic-key", "")
	assertions.Equal(http.StatusServiceUnavailable, closed.Code)
	data, err := io.ReadAll(closed.Body)
	requirements.NoError(err)
	assertions.NotContains(string(data), id)
}

func TestMCPHTTPProductionValidatesIssuedClientIdentity(t *testing.T) {
	assertions := assert.New(t)
	handler := newMCPHTTPServer(ServeOptions{}, HTTPOptions{APIKey: "synthetic-key", AllowWrites: true}).Handler
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer synthetic-key")
	request.Header.Set("Mcp-Session-Id", "caller-chosen-id")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertions.Equal(http.StatusNotFound, response.Code)
}

func TestConfirmationLegacyHTTPRefusesBeforeExecution(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	clients := newConfirmationClients()
	manager := newConfirmationChallenges()
	var executed atomic.Int32
	transport := sdkmcp.NewStreamableHTTPHandler(func(request *http.Request) *sdkmcp.Server {
		server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "confirmation-test", Version: "1"}, nil)
		sdkmcp.AddTool[map[string]any, any](server, &sdkmcp.Tool{Name: "synthetic_write", InputSchema: closedObject(nil), OutputSchema: closedObject(nil)}, officialToolHandler(func(ctx context.Context, req toolRequest) (*toolResult, error) {
			if err := req.confirmUserAction(ctx, "Perform synthetic action"); err != nil {
				return confirmationToolError(err)
			}
			executed.Add(1)
			return jsonResult(map[string]any{})
		}, confirmationConfig{manager: manager, sessionKey: confirmationClientKey(request.Context()), requireSessionKey: true}))
		return server
	}, &sdkmcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"synthetic_write","arguments":{}}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	response := httptest.NewRecorder()
	clients.middleware(transport).ServeHTTP(response, request)
	requirements.Equal(http.StatusOK, response.Code)
	assertions.Contains(response.Body.String(), "client confirmation is unsupported on legacy HTTP")
	assertions.Equal(int32(0), executed.Load())
}

func TestDelegatedCatalogHasNoOwnerArchiveToolsOrResources(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	opts := ServeOptions{DelegatedOnly: true}
	assertions.Empty(rawListTools(t, opts, true))
	response := rawModernCall(t, opts, HTTPOptions{}, "server/discover", nil)
	capabilities, ok := response.Result["capabilities"].(map[string]any)
	requirements.True(ok)
	assertions.NotContains(capabilities, "resources")
}
