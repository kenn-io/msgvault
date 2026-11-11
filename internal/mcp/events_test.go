package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mcpevents"
)

type eventTestBackend struct {
	calls        int
	subscribe    mcpevents.MCPEventsSubscribeRequest
	unsubscribe  mcpevents.MCPEventsUnsubscribeRequest
	listErr      error
	subscribeErr error
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
	if b.subscribeErr != nil {
		return mcpevents.MCPEventsSubscribeResult{}, b.subscribeErr
	}
	return mcpevents.MCPEventsSubscribeResult{ID: "fixture", RefreshBefore: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), DeliveryStatus: mcpevents.MCPEventsDeliveryStatus{Active: true}}, nil
}

func TestEventsListPropagatesBackendFailure(t *testing.T) {
	for _, tc := range []struct {
		name, method string
		err          error
		reason       string
		code, status int
	}{
		{"generic list", "events/list", errors.New("daemon offline"), "daemon_unavailable", mcpevents.Unavailable, http.StatusOK},
		{"disabled list", "events/list", &mcpevents.Error{Code: mcpevents.Unavailable, Reason: "disabled"}, "disabled", mcpevents.Unavailable, http.StatusOK},
		{"invalid", "events/subscribe", &mcpevents.Error{Code: mcpevents.InvalidParams, Reason: "invalid_scope"}, "invalid_scope", mcpevents.InvalidParams, http.StatusBadRequest},
		{"not found", "events/subscribe", &mcpevents.Error{Code: mcpevents.NotFound, Reason: "event"}, "event", mcpevents.NotFound, http.StatusOK},
		{"capacity", "events/subscribe", &mcpevents.Error{Code: mcpevents.ResourceExhausted, Reason: "subscriptions"}, "subscriptions", mcpevents.ResourceExhausted, http.StatusOK},
		{"unsupported", "events/subscribe", &mcpevents.Error{Code: mcpevents.Unsupported, Reason: "delivery"}, "delivery", mcpevents.Unsupported, http.StatusOK},
		{"callback", "events/subscribe", &mcpevents.Error{Code: mcpevents.CallbackEndpoint, Reason: "challenge_failed"}, "challenge_failed", mcpevents.CallbackEndpoint, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			backend := &eventTestBackend{}
			if tc.method == "events/list" {
				backend.listErr = tc.err
			} else {
				backend.subscribeErr = tc.err
			}
			params := map[string]any{"_meta": modernRequestMeta()}
			if tc.method != "events/list" {
				params["name"] = mcpevents.Name
				params["arguments"] = map[string]any{"source_id": 1}
				params["delivery"] = map[string]any{"mode": "webhook", "url": "https://example.com/hook", "secret": "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}
			}
			body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": tc.method, "params": params})
			require.NoError(err)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Protocol-Version", modernProtocolVersion)
			req.Header.Set("Mcp-Method", tc.method)
			req.Header.Set("Mcp-Name", mcpevents.Name)
			req.Header.Set("Authorization", "Bearer fixture-key")
			recorder := httptest.NewRecorder()
			newMCPHTTPServer(ServeOptions{Events: backend}, HTTPOptions{APIKey: "fixture-key", AllowWrites: true}).Handler.ServeHTTP(recorder, req)
			require.Equal(tc.status, recorder.Code)
			var response rawRPCResponse
			require.NoError(json.Unmarshal(recorder.Body.Bytes(), &response))
			assert.InDelta(float64(tc.code), response.Error["code"], 0)
			assert.Equal(map[string]any{"reason": tc.reason}, response.Error["data"])
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
	for _, tc := range []struct {
		name, method string
		options      HTTPOptions
		header       bool
		status, code int
		list         bool
	}{
		{"keyed list", "events/list", HTTPOptions{APIKey: "fixture-key"}, false, http.StatusOK, 0, true},
		{"keyed readonly subscribe", "events/subscribe", HTTPOptions{APIKey: "fixture-key"}, true, http.StatusNotFound, -32601, false},
		{"keyed readonly unsubscribe", "events/unsubscribe", HTTPOptions{APIKey: "fixture-key"}, true, http.StatusNotFound, -32601, false},
		{"keyless list", "events/list", HTTPOptions{AllowWrites: true}, false, http.StatusNotFound, -32601, false},
		{"keyless subscribe", "events/subscribe", HTTPOptions{AllowWrites: true}, false, http.StatusNotFound, -32601, false},
		{"keyless unsubscribe", "events/unsubscribe", HTTPOptions{AllowWrites: true}, false, http.StatusNotFound, -32601, false},
		{"missing name header", "events/subscribe", HTTPOptions{APIKey: "fixture-key", AllowWrites: true}, false, http.StatusBadRequest, -32020, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			backend := &eventTestBackend{}
			params := map[string]any{"_meta": modernRequestMeta()}
			if tc.method != "events/list" {
				params["name"] = mcpevents.Name
				params["arguments"] = map[string]any{"source_id": 1}
				params["delivery"] = map[string]any{"mode": "webhook", "url": "https://example.com/hook", "secret": "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}
			}
			body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": tc.method, "params": params})
			require.NoError(err)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Protocol-Version", modernProtocolVersion)
			req.Header.Set("Mcp-Method", tc.method)
			if tc.header {
				req.Header.Set("Mcp-Name", mcpevents.Name)
			}
			if tc.options.APIKey != "" {
				req.Header.Set("Authorization", "Bearer "+tc.options.APIKey)
			}
			recorder := httptest.NewRecorder()
			newMCPHTTPServer(ServeOptions{Events: backend}, tc.options).Handler.ServeHTTP(recorder, req)
			require.Equal(tc.status, recorder.Code)
			var response rawRPCResponse
			require.NoError(json.Unmarshal(recorder.Body.Bytes(), &response))
			if tc.list {
				assert.Len(response.Result["events"], 1)
				assert.Equal(1, backend.calls)
			} else {
				assert.InDelta(tc.code, response.Error["code"], 0)
				assert.Zero(backend.calls)
			}
		})
	}
}

func TestEventsLegacyDoNotReachBackend(t *testing.T) {
	for _, tc := range []struct {
		name              string
		legacy, delegated bool
	}{{"legacy", true, false}} {
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
