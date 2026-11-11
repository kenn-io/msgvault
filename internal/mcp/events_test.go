package mcp

import (
	"context"
	"encoding/json/v2"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mcpevents"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNativeEventParamsRejectDuplicateBeforeForwarding(t *testing.T) {
	var p nativeEventParams
	require.NoError(t, p.UnmarshalJSON([]byte(`{"name":"msgvault.messages.changed","arguments":{"source_id":1,"source_id":2},"delivery":{"mode":"webhook","url":"https://example.com","secret":"x"}}`)))
	_, err := p.subscribe()
	assert.Error(t, err)
}
func TestNativeEventResultIncludesNullCursor(t *testing.T) {
	b, err := json.Marshal(&nativeEventResult{value: map[string]any{"cursor": nil, "truncated": false}, complete: true})
	require.NoError(t, err)
	assert.JSONEq(t, `{"cursor":null,"truncated":false,"resultType":"complete"}`, string(b))
}

type eventTestBackend struct{ calls int }

func (b *eventTestBackend) MCPEventsList(context.Context) (mcpevents.ListResult, error) {
	b.calls++
	return mcpevents.ListResult{Enabled: true, Events: mcpevents.Definitions()}, nil
}
func (b *eventTestBackend) MCPEventsSubscribe(_ context.Context, _ mcpevents.SubscribeRequest) (mcpevents.SubscribeResult, error) {
	b.calls++
	return mcpevents.SubscribeResult{ID: "fixture", RefreshBefore: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), DeliveryStatus: mcpevents.DeliveryStatus{Active: true}}, nil
}
func (b *eventTestBackend) MCPEventsUnsubscribe(context.Context, mcpevents.UnsubscribeRequest) error {
	b.calls++
	return nil
}

type eventListParams struct{ sdkmcp.ParamsBase }
type eventListResult struct {
	sdkmcp.ResultBase

	Events []mcpevents.Definition `json:"events"`
}
type eventSubscribeParams struct {
	sdkmcp.ParamsBase

	Name      string              `json:"name"`
	Arguments mcpevents.Arguments `json:"arguments"`
	Delivery  mcpevents.Delivery  `json:"delivery"`
}
type eventSubscribeResult struct {
	sdkmcp.ResultBase

	ID string `json:"id"`
}

func TestEventsListStdioAndModernHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &eventTestBackend{}
	opts := ServeOptions{Events: backend}
	response := rawModernCall(t, opts, HTTPOptions{}, "events/list", nil)
	assert.Len(response.Result["events"], 1)
	ct, st := sdkmcp.NewInMemoryTransports()
	server, err := newMCPServer(opts, false).Connect(t.Context(), st, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = server.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil)
	require.NoError(sdkmcp.AddSendingCustomMethod[*eventListParams, *eventListResult](client, "events/list"))
	session, err := client.Connect(t.Context(), ct, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = session.Close() })
	result, err := sdkmcp.CallCustomMethod[*eventListParams, *eventListResult](t.Context(), session, "events/list", &eventListParams{})
	require.NoError(err)
	require.Len(result.Events, 1)
	assert.Equal(mcpevents.Name, result.Events[0].Name)
}

func TestEventsSubscribeStdioAndModernHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &eventTestBackend{}
	delivery := mcpevents.Delivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}
	response := rawModernCall(t, ServeOptions{Events: backend}, HTTPOptions{}, "events/subscribe", map[string]any{
		"name": mcpevents.Name, "arguments": map[string]any{"source_id": 1}, "delivery": delivery,
	})
	assert.Equal("fixture", response.Result["id"])
	ct, st := sdkmcp.NewInMemoryTransports()
	server, err := newMCPServer(ServeOptions{Events: backend}, false).Connect(t.Context(), st, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = server.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil)
	require.NoError(sdkmcp.AddSendingCustomMethod[*eventSubscribeParams, *eventSubscribeResult](client, "events/subscribe"))
	session, err := client.Connect(t.Context(), ct, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = session.Close() })
	result, err := sdkmcp.CallCustomMethod[*eventSubscribeParams, *eventSubscribeResult](t.Context(), session, "events/subscribe", &eventSubscribeParams{Name: mcpevents.Name, Arguments: mcpevents.Arguments{SourceID: 1}, Delivery: delivery})
	require.NoError(err)
	assert.Equal("fixture", result.ID)
	assert.Equal(2, backend.calls)
}
func TestEventsLegacyAndDisabledDoNotReachBackend(t *testing.T) {
	backend := &eventTestBackend{}
	for _, tc := range []struct {
		name, version string
		opts          ServeOptions
	}{{"disabled", modernProtocolVersion, ServeOptions{}}, {"legacy", "2025-06-18", ServeOptions{Events: backend}}, {"delegated", modernProtocolVersion, ServeOptions{Events: backend, DelegatedOnly: true}}} {
		t.Run(tc.name, func(t *testing.T) {
			h, closeHandler := NewHTTPHandler(tc.opts, HTTPOptions{})
			defer closeHandler()
			body := `{"jsonrpc":"2.0","id":1,"method":"events/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"test","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}}`
			r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Accept", "application/json, text/event-stream")
			r.Header.Set("Mcp-Protocol-Version", tc.version)
			r.Header.Set("Mcp-Method", "events/list")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			assert.Contains(t, w.Body.String(), `"error"`)
			assert.Zero(t, backend.calls)
		})
	}
}
