package mcp

import (
	"context"
	"errors"
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
	subscribe    mcpevents.SubscribeRequest
	unsubscribe  mcpevents.UnsubscribeRequest
	listErr      error
	subscribeErr error
	disabled     bool
}

func (b *eventTestBackend) MCPEventsList(context.Context) (mcpevents.ListResult, error) {
	b.calls++
	if b.listErr != nil {
		return mcpevents.ListResult{}, b.listErr
	}
	if b.disabled {
		return mcpevents.ListResult{Events: []mcpevents.Definition{}}, nil
	}
	return mcpevents.ListResult{Enabled: true, Events: mcpevents.Definitions()}, nil
}
func (b *eventTestBackend) MCPEventsSubscribe(_ context.Context, in mcpevents.SubscribeRequest) (mcpevents.SubscribeResult, error) {
	b.calls++
	b.subscribe = in
	if b.subscribeErr != nil {
		return mcpevents.SubscribeResult{}, b.subscribeErr
	}
	return mcpevents.SubscribeResult{ID: "fixture", RefreshBefore: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), DeliveryStatus: mcpevents.DeliveryStatus{Active: true}}, nil
}

func TestEventsUnavailableStillListsEmpty(t *testing.T) {
	backend := &eventTestBackend{disabled: true}
	response := rawModernCall(t, ServeOptions{Events: backend}, HTTPOptions{}, "events/list", nil)
	assert.Empty(t, response.Result["events"])
	backend.listErr = errors.New("daemon offline")
	response = rawModernCall(t, ServeOptions{Events: backend}, HTTPOptions{}, "events/list", nil)
	assert.Empty(t, response.Result["events"])
}
func (b *eventTestBackend) MCPEventsUnsubscribe(_ context.Context, in mcpevents.UnsubscribeRequest) error {
	b.calls++
	b.unsubscribe = in
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

func TestEventsStdioAndModernHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &eventTestBackend{}
	delivery := mcpevents.Delivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}
	opts := ServeOptions{Events: backend}
	for _, tc := range []struct {
		method string
		params map[string]any
	}{
		{"events/list", nil},
		{"events/subscribe", map[string]any{"name": mcpevents.Name, "arguments": map[string]any{"source_id": 1}, "delivery": delivery}},
		{"events/unsubscribe", map[string]any{"name": mcpevents.Name, "arguments": map[string]any{"source_id": 1}, "delivery": map[string]any{"mode": "webhook", "url": delivery.URL}}},
	} {
		response := rawModernCall(t, opts, HTTPOptions{}, tc.method, tc.params)
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
	server, err := newMCPServer(opts, false).Connect(t.Context(), st, nil)
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
	params := &eventSubscribeParams{Name: mcpevents.Name, Arguments: mcpevents.Arguments{SourceID: 1}, Delivery: delivery}
	result, err := sdkmcp.CallCustomMethod[*eventSubscribeParams, *eventSubscribeResult](t.Context(), session, "events/subscribe", params)
	require.NoError(err)
	assert.Equal("fixture", result.ID)
	params.Delivery.Secret = ""
	_, err = sdkmcp.CallCustomMethod[*eventSubscribeParams, *eventSubscribeResult](t.Context(), session, "events/unsubscribe", params)
	require.NoError(err)
	assert.Equal(int64(1), backend.unsubscribe.Arguments.SourceID)
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
