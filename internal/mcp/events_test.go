package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mcpevents"
)

type eventTestBackend struct {
	calls       int
	subscribe   mcpevents.MCPEventsSubscribeRequest
	unsubscribe mcpevents.MCPEventsUnsubscribeRequest
	listErr     error
}

func (b *eventTestBackend) MCPEventsList(context.Context) (mcpevents.MCPEventsListResult, error) {
	b.calls++
	if b.listErr != nil {
		return mcpevents.MCPEventsListResult{}, b.listErr
	}
	return mcpevents.MCPEventsListResult{Enabled: true, Events: mcpevents.Definitions()}, nil
}
func (b *eventTestBackend) MCPEventsSubscribe(_ context.Context, in mcpevents.MCPEventsSubscribeRequest) (mcpevents.MCPEventsSubscribeResult, error) {
	b.calls++
	b.subscribe = in
	return mcpevents.MCPEventsSubscribeResult{ID: "fixture", RefreshBefore: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), DeliveryStatus: mcpevents.MCPEventsDeliveryStatus{Active: true}}, nil
}

func TestEventsListPropagatesBackendFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reason string
		code   int
	}{
		{"generic", errors.New("daemon offline"), "daemon_unavailable", mcpevents.Unavailable},
		{"typed", &mcpevents.Error{Code: mcpevents.Unavailable, Reason: "disabled"}, "disabled", mcpevents.Unavailable},
		{"callback", &mcpevents.Error{Code: -32027, Reason: "challenge_failed"}, "challenge_failed", -32027},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			backend := &eventTestBackend{listErr: tc.err}
			body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/list", "params": map[string]any{"_meta": modernRequestMeta()}})
			requirements.NoError(err)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Protocol-Version", modernProtocolVersion)
			req.Header.Set("Mcp-Method", "events/list")
			req.Header.Set("Authorization", "Bearer fixture-key")
			recorder := httptest.NewRecorder()
			newMCPHTTPServer(ServeOptions{Events: backend}, HTTPOptions{APIKey: "fixture-key"}).Handler.ServeHTTP(recorder, req)
			requirements.Equal(http.StatusOK, recorder.Code)
			var response rawRPCResponse
			requirements.NoError(json.Unmarshal(recorder.Body.Bytes(), &response))
			assertions.InDelta(float64(tc.code), response.Error["code"], 0)
			assertions.Equal(map[string]any{"reason": tc.reason}, response.Error["data"])
		})
	}
}

func (b *eventTestBackend) MCPEventsUnsubscribe(_ context.Context, in mcpevents.MCPEventsUnsubscribeRequest) error {
	b.calls++
	b.unsubscribe = in
	return nil
}

type eventListParams struct{ sdkmcp.ParamsBase }
type eventListResult struct {
	sdkmcp.ResultBase

	Events []mcpevents.MCPEventsDefinition `json:"events"`
}
type eventSubscribeParams struct {
	sdkmcp.ParamsBase

	Name      string                       `json:"name"`
	Arguments mcpevents.MCPEventsArguments `json:"arguments"`
	Delivery  mcpevents.MCPEventsDelivery  `json:"delivery"`
}
type eventSubscribeResult struct {
	sdkmcp.ResultBase

	ID string `json:"id"`
}

func TestEventsStdioAndModernHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &eventTestBackend{}
	delivery := mcpevents.MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}
	opts := ServeOptions{Events: backend}
	for _, tc := range []struct {
		method string
		params map[string]any
	}{
		{"events/list", nil},
		{"events/subscribe", map[string]any{"name": mcpevents.Name, "arguments": map[string]any{"source_id": 1}, "delivery": delivery}},
		{"events/unsubscribe", map[string]any{"name": mcpevents.Name, "arguments": map[string]any{"source_id": 1}, "delivery": map[string]any{"mode": "webhook", "url": delivery.URL}}},
	} {
		response := rawModernCall(t, opts, HTTPOptions{AllowWrites: true, APIKey: "fixture-key"}, tc.method, tc.params)
		switch tc.method {
		case "events/list":
			assert.Len(response.Result["events"], 1)
		case "events/subscribe":
			assert.Equal("fixture", response.Result["id"])
			cursor, exists := response.Result["cursor"]
			assert.True(exists)
			assert.Nil(cursor)
			assert.Equal("complete", response.Result["resultType"])
			assert.Equal(int64(1), backend.subscribe.Arguments.SourceID)
			assert.Equal(delivery.URL, backend.subscribe.Delivery.URL)
		case "events/unsubscribe":
			assert.Equal(int64(1), backend.unsubscribe.Arguments.SourceID)
		}
	}
	ct, st := sdkmcp.NewInMemoryTransports()
	server, err := newMCPServer(opts, true).Connect(t.Context(), st, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = server.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil)
	require.NoError(sdkmcp.AddSendingCustomMethod[*eventListParams, *eventListResult](client, "events/list"))
	require.NoError(sdkmcp.AddSendingCustomMethod[*eventSubscribeParams, *eventSubscribeResult](client, "events/subscribe"))
	require.NoError(sdkmcp.AddSendingCustomMethod[*eventSubscribeParams, *eventSubscribeResult](client, "events/unsubscribe"))
	session, err := client.Connect(t.Context(), ct, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = session.Close() })
	list, err := sdkmcp.CallCustomMethod[*eventListParams, *eventListResult](t.Context(), session, "events/list", &eventListParams{})
	require.NoError(err)
	require.Len(list.Events, 1)
	assert.Equal(mcpevents.Name, list.Events[0].Name)
	params := &eventSubscribeParams{Name: mcpevents.Name, Arguments: mcpevents.MCPEventsArguments{SourceID: 1}, Delivery: delivery}
	result, err := sdkmcp.CallCustomMethod[*eventSubscribeParams, *eventSubscribeResult](t.Context(), session, "events/subscribe", params)
	require.NoError(err)
	assert.Equal("fixture", result.ID)
	backend.unsubscribe = mcpevents.MCPEventsUnsubscribeRequest{}
	params.Delivery.Secret = ""
	_, err = sdkmcp.CallCustomMethod[*eventSubscribeParams, *eventSubscribeResult](t.Context(), session, "events/unsubscribe", params)
	require.NoError(err)
	assert.Equal(int64(1), backend.unsubscribe.Arguments.SourceID)
}

func TestEventsHTTPWriteAdmission(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend := &eventTestBackend{}
	opts := ServeOptions{Events: backend}
	name := mcpevents.Name
	params := map[string]any{"name": name, "arguments": map[string]any{"source_id": 1}, "delivery": map[string]any{"mode": "webhook", "url": "https://example.com/hook", "secret": "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}
	list := rawModernCall(t, opts, HTTPOptions{APIKey: "fixture-key"}, "events/list", nil)
	assertions.Len(list.Result["events"], 1)
	for _, method := range []string{"events/subscribe", "events/unsubscribe"} {
		params["_meta"] = modernRequestMeta()
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		requirements.NoError(err)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", modernProtocolVersion)
		req.Header.Set("Mcp-Method", method)
		req.Header.Set("Mcp-Name", name)
		req.Header.Set("Authorization", "Bearer fixture-key")
		recorder := httptest.NewRecorder()
		newMCPHTTPServer(opts, HTTPOptions{APIKey: "fixture-key"}).Handler.ServeHTTP(recorder, req)
		requirements.Equal(http.StatusOK, recorder.Code)
		var response rawRPCResponse
		requirements.NoError(json.Unmarshal(recorder.Body.Bytes(), &response))
		assertions.NotEmpty(response.Error, method)
	}
	assertions.Equal(1, backend.calls)
}

func TestEventsLegacyAndDelegatedDoNotReachBackend(t *testing.T) {
	for _, tc := range []struct {
		name              string
		legacy, delegated bool
	}{{"legacy", true, false}, {"delegated", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			backend := &eventTestBackend{}
			ct, st := sdkmcp.NewInMemoryTransports()
			server, err := newMCPServer(ServeOptions{Events: backend, legacyHTTP: tc.legacy, DelegatedOnly: tc.delegated}, false).Connect(t.Context(), st, nil)
			require.NoError(err)
			t.Cleanup(func() { _ = server.Close() })
			client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil)
			require.NoError(sdkmcp.AddSendingCustomMethod[*eventListParams, *eventListResult](client, "events/list"))
			session, err := client.Connect(t.Context(), ct, nil)
			require.NoError(err)
			t.Cleanup(func() { _ = session.Close() })
			_, err = sdkmcp.CallCustomMethod[*eventListParams, *eventListResult](t.Context(), session, "events/list", &eventListParams{})
			var rpcErr *jsonrpc.Error
			require.ErrorAs(err, &rpcErr)
			assert.Equal(t, int64(-32601), rpcErr.Code)
			assert.Zero(t, backend.calls)
		})
	}
}

func TestKeylessStandaloneHTTPEventsUnavailable(t *testing.T) {
	backend := &eventTestBackend{}
	server := newMCPHTTPServer(ServeOptions{Events: backend}, HTTPOptions{AllowWrites: true})
	for _, method := range []string{"events/list", "events/subscribe", "events/unsubscribe"} {
		t.Run(method, func(t *testing.T) {
			params := map[string]any{"_meta": modernRequestMeta(), "name": mcpevents.Name, "arguments": map[string]any{"source_id": 1}, "delivery": map[string]any{"mode": "webhook", "url": "https://example.com/hook", "secret": "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}
			body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
			require.NoError(t, err)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Protocol-Version", modernProtocolVersion)
			req.Header.Set("Mcp-Method", method)
			recorder := httptest.NewRecorder()
			server.Handler.ServeHTTP(recorder, req)
			require.Equal(t, http.StatusOK, recorder.Code)
			var response rawRPCResponse
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			assert.InDelta(t, -32601, response.Error["code"], 0)
			assert.Zero(t, backend.calls)
		})
	}
}
