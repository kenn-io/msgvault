package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/query"
)

// The backend fake models the external daemon contract; protocol/auth/cache
// behavior is exercised through the real SDK HTTP handler.
type eventsTestBackend struct{ err error }

func (b eventsTestBackend) MCPEventsList(context.Context) (mcpevents.ListResult, error) {
	return mcpevents.ListResult{Events: []mcpevents.Definition{{Name: "msgvault.message_archived", Delivery: []string{"webhook"}}}}, b.err
}
func (b eventsTestBackend) MCPEventsSubscribe(context.Context, mcpevents.SubscribeRequest) (mcpevents.SubscribeResult, error) {
	return mcpevents.SubscribeResult{ID: "sub_fixture", Cursor: "cursor_fixture"}, b.err
}
func (b eventsTestBackend) MCPEventsUnsubscribe(context.Context, mcpevents.UnsubscribeRequest) error {
	return b.err
}
func (b eventsTestBackend) GetMCPEvent(context.Context, string) (mcpevents.Envelope, error) {
	return mcpevents.Envelope{EventID: "event_fixture"}, b.err
}
func (b eventsTestBackend) ListMCPCalendarSources(context.Context) ([]mcpevents.CalendarSource, error) {
	return []mcpevents.CalendarSource{{SourceID: "42"}}, b.err
}
func (b eventsTestBackend) GetMCPEventMessage(context.Context, string, int64) (*query.MessageDetail, error) {
	return nil, b.err
}

func eventsHTTPCall(t *testing.T, h http.Handler, version, method, params string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	require := Require.New(t)
	encoded := strings.TrimSpace(params)
	require.True(strings.HasPrefix(encoded, "{") && strings.HasSuffix(encoded, "}"))
	if version == "2026-07-28" {
		members := strings.TrimSpace(encoded[1 : len(encoded)-1])
		encoded = `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
		if members != "" {
			encoded += "," + members
		}
		encoded += "}"
	}
	req := task3ModernRequest(method, "", `{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+encoded+`}`)
	req.Header.Set("Mcp-Protocol-Version", version)
	req.Header.Set("Authorization", "Bearer owner-fixture")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var result map[string]any
	if rec.Code != http.StatusBadRequest || json.Valid(rec.Body.Bytes()) {
		decoder := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
		decoder.UseNumber()
		require.NoError(decoder.Decode(&result), rec.Body.String())
	}
	return rec, result
}

func eventsWireObject(t *testing.T, value any) map[string]any {
	t.Helper()
	require := Require.New(t)
	object, ok := value.(map[string]any)
	require.True(ok, "expected JSON object, got %T", value)
	return object
}

func eventsWireArray(t *testing.T, value any) []any {
	t.Helper()
	require := Require.New(t)
	array, ok := value.([]any)
	require.True(ok, "expected JSON array, got %T", value)
	return array
}

func eventsWireString(t *testing.T, value any) string {
	t.Helper()
	require := Require.New(t)
	text, ok := value.(string)
	require.True(ok, "expected JSON string, got %T", value)
	return text
}

func TestEventsModernDiscoveryAndMethods(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	h := newMCPHTTPServer(ServeOptions{Events: eventsTestBackend{}}, HTTPOptions{APIKey: "owner-fixture"}).Handler
	rec, wire := eventsHTTPCall(t, h, "2026-07-28", "server/discover", `{}`)
	require.Equal(http.StatusOK, rec.Code)
	require.Nil(wire["error"])
	result := eventsWireObject(t, wire["result"])
	assert.Equal(map[string]any{}, eventsWireObject(t, result["capabilities"])["events"])
	assert.Equal(json.Number("3600000"), result["ttlMs"])
	assert.Equal("public", result["cacheScope"])
	assert.Equal("no-store", rec.Header().Get("Cache-Control"))
	assert.Contains(result["instructions"], "Archived messages")
	assert.Contains(result["instructions"], "eventId")

	_, initWire := eventsHTTPCall(t, h, "2026-07-28", "initialize", `{"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}`)
	// The pinned SDK removes initialize on the modern protocol.
	require.NotNil(initWire["error"])
	assert.Equal(json.Number("-32601"), eventsWireObject(t, initWire["error"])["code"])
	_, legacyInit := eventsHTTPCall(t, h, "2025-11-25", "initialize", `{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}`)
	require.Nil(legacyInit["error"])
	assert.NotContains(eventsWireObject(t, legacyInit["result"])["capabilities"], "events")
	_, wire = eventsHTTPCall(t, h, "2026-07-28", "events/list", `{}`)
	require.Nil(wire["error"])
	assert.Len(eventsWireObject(t, wire["result"])["events"], 1)
	_, wire = eventsHTTPCall(t, h, "2026-07-28", "events/subscribe", `{"name":"msgvault.message_archived","arguments":{"conversation_id":"42"},"delivery":{"mode":"webhook","url":"https://callback.example/hook","secret":"whsec_fixture"}}`)
	require.Nil(wire["error"])
	assert.Equal("sub_fixture", eventsWireObject(t, wire["result"])["id"])
	_, wire = eventsHTTPCall(t, h, "2026-07-28", "events/unsubscribe", `{"name":"msgvault.message_archived","arguments":{"conversation_id":"42"},"delivery":{"mode":"webhook","url":"https://callback.example/hook"}}`)
	require.Nil(wire["error"])
}
func TestEventsUnavailableAndClosedParams(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	for _, tc := range []struct {
		name, version string
		options       ServeOptions
		http          HTTPOptions
		code          int64
	}{
		{"disabled", "2026-07-28", ServeOptions{}, HTTPOptions{APIKey: "owner-fixture"}, -32601},
		{"old protocol", "2025-11-25", ServeOptions{Events: eventsTestBackend{}}, HTTPOptions{APIKey: "owner-fixture"}, -32601},
		{"independent token", "2026-07-28", ServeOptions{Events: eventsTestBackend{}}, HTTPOptions{APIKey: "owner-fixture", IndependentCredential: true}, -32601},
		{"keyless", "2026-07-28", ServeOptions{Events: eventsTestBackend{}}, HTTPOptions{}, -32601},
		{"delegated", "2026-07-28", ServeOptions{Events: eventsTestBackend{}, DelegatedOnly: true}, HTTPOptions{APIKey: "owner-fixture"}, -32601},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			h := newMCPHTTPServer(tc.options, tc.http).Handler
			rec, wire := eventsHTTPCall(t, h, tc.version, "events/list", `{}`)
			if tc.version != "2026-07-28" {
				require.Equal(http.StatusBadRequest, rec.Code)
				return
			}
			require.NotNil(wire["error"])
			assert.Equal(json.Number(strconv.FormatInt(tc.code, 10)), eventsWireObject(t, wire["error"])["code"])
		})
	}
	h := newMCPHTTPServer(ServeOptions{Events: eventsTestBackend{}}, HTTPOptions{APIKey: "owner-fixture"}).Handler
	_, wire := eventsHTTPCall(t, h, "2026-07-28", "events/subscribe", `{"principal":"owner:forged"}`)
	require.NotNil(wire["error"])
	assert.Equal(json.Number("-32602"), eventsWireObject(t, wire["error"])["code"])
}

func TestEventsErrorsAndReadTools(t *testing.T) {
	assert := Assert.New(t)
	for _, tc := range []struct {
		name   string
		err    error
		code   int64
		reason string
	}{
		{"safe", &mcpevents.Error{Code: -32602, Reason: "unknown_scope"}, -32602, "unknown_scope"},
		{"unsafe", &mcpevents.Error{Code: -32602, Reason: "private callback secret"}, -32015, "events_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			h := newMCPHTTPServer(ServeOptions{Events: eventsTestBackend{err: tc.err}}, HTTPOptions{APIKey: "owner-fixture"}).Handler
			rec, wire := eventsHTTPCall(t, h, "2026-07-28", "events/list", `{}`)
			status := http.StatusOK
			if tc.code == -32602 {
				status = http.StatusBadRequest
			}
			require.Equal(status, rec.Code)
			rpc := eventsWireObject(t, wire["error"])
			assert.Equal(json.Number(strconv.FormatInt(tc.code, 10)), rpc["code"])
			assert.Equal(tc.reason, eventsWireObject(t, rpc["data"])["reason"])
			assert.NotContains(rec.Body.String(), "private callback")
		})
	}
	h := newMCPHTTPServer(ServeOptions{Events: eventsTestBackend{}}, HTTPOptions{APIKey: "owner-fixture"}).Handler
	for _, name := range []string{"get_mcp_event", "list_calendar_sources"} {
		args := `{}`
		if name == "get_mcp_event" {
			args = `{"event_id":"event_fixture"}`
		}
		req := task3ModernRequest("tools/call", name, task3ToolCallBody(1, name, args))
		req.Header.Set("Authorization", "Bearer owner-fixture")
		rec, wire := task3Serve(h, req)
		task3RequireSuccess(t, rec, wire)
		assert.NotEqual(true, wire.Result["isError"])
	}
}

func TestEventsReadToolsAnnounceReadOnlyAnnotations(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	h := newMCPHTTPServer(ServeOptions{Events: eventsTestBackend{}}, HTTPOptions{APIKey: "owner-fixture"}).Handler
	rec, wire := eventsHTTPCall(t, h, "2026-07-28", "tools/list", `{}`)
	require.Equal(http.StatusOK, rec.Code)
	require.Nil(wire["error"])
	annotations := map[string]any{}
	for _, item := range eventsWireArray(t, eventsWireObject(t, wire["result"])["tools"]) {
		tool := eventsWireObject(t, item)
		annotations[eventsWireString(t, tool["name"])] = tool["annotations"]
	}
	require.Contains(annotations, "get_message")
	getMessage := eventsWireObject(t, annotations["get_message"])
	assert.Equal(true, getMessage["readOnlyHint"])
	for _, name := range []string{"get_mcp_event", "list_calendar_sources"} {
		require.Contains(annotations, name)
		assert.Equal(getMessage, eventsWireObject(t, annotations[name]), name)
	}
}

func TestEventMessageReadRetainsSafeRPCError(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	h := newMCPHTTPServer(ServeOptions{Events: eventsTestBackend{err: &mcpevents.Error{Code: -32012, Reason: "owner_required"}}}, HTTPOptions{APIKey: "owner-fixture"}).Handler
	req := task3ModernRequest("tools/call", "get_message", task3ToolCallBody(1, "get_message", `{"id":42,"event_id":"event_fixture"}`))
	req.Header.Set("Authorization", "Bearer owner-fixture")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var wire map[string]any
	decoder := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	decoder.UseNumber()
	require.NoError(decoder.Decode(&wire))
	require.NotNil(wire["error"], rec.Body.String())
	rpc := eventsWireObject(t, wire["error"])
	require.Equal(json.Number("-32012"), rpc["code"])
	assert.Equal("owner_required", eventsWireObject(t, rpc["data"])["reason"])
}

type messageIDEventsBackend struct {
	eventsTestBackend

	ids []int64
}

func (b *messageIDEventsBackend) GetMCPEventMessage(_ context.Context, eventID string, id int64) (*query.MessageDetail, error) {
	b.ids = append(b.ids, id)
	return &query.MessageDetail{ID: id, BodyText: eventID}, nil
}

func TestEventMessageIDsPreserveDecimalInt64(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input any
		want  int64
	}{
		{"numeric", float64(42), 42},
		{"decimal", "42", 42},
		{"above JS safe", "9007199254740993", 9007199254740993},
		{"max int64", "9223372036854775807", 9223372036854775807},
		{"zero", "0", 0},
		{"leading zero", "042", 0},
		{"overflow", "9223372036854775808", 0},
		{"negative", "-42", 0},
		{"plus", "+42", 0},
		{"whitespace", " 42", 0},
		{"fraction", "42.0", 0},
		{"exponent", "4.2e1", 0},
		{"empty", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			backend := &messageIDEventsBackend{}
			h := &handlers{events: backend}
			result, err := h.getMessage(t.Context(), toolRequest{arguments: map[string]any{"id": tc.input, "event_id": "event_fixture"}})
			require.NoError(err)
			require.NotNil(result)
			if tc.want == 0 {
				assert.True(result.isError)
				assert.Empty(backend.ids, "invalid ID must never reach receipt authorization")
				return
			}
			assert.False(result.isError)
			assert.Equal([]int64{tc.want}, backend.ids)
		})
	}
}

type blockedEventsBackend struct {
	eventsTestBackend

	entered chan struct{}
	release chan struct{}
	calls   *atomic.Int32
}

func (b blockedEventsBackend) MCPEventsList(ctx context.Context) (mcpevents.ListResult, error) {
	if b.calls.Add(1) != 1 {
		return b.eventsTestBackend.MCPEventsList(ctx)
	}
	close(b.entered)
	select {
	case <-b.release:
		return b.eventsTestBackend.MCPEventsList(ctx)
	case <-ctx.Done():
		return mcpevents.ListResult{}, ctx.Err()
	}
}
func TestEventsConcurrencyLimitReturnsSafeProtocolError(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	backend := blockedEventsBackend{entered: make(chan struct{}), release: make(chan struct{}), calls: new(atomic.Int32)}
	h := newMCPHTTPServerWithPolicy(ServeOptions{Events: backend}, HTTPOptions{APIKey: "owner-fixture"}, newInvocationPolicy(20, 40, 1)).Handler
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := task3ModernRequest("events/list", "", `{"jsonrpc":"2.0","id":1,"method":"events/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
		req.Header.Set("Authorization", "Bearer owner-fixture")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	defer func() {
		close(backend.release)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			assert.Fail("Events call did not finish after release")
		}
	}()
	select {
	case <-backend.entered:
	case <-time.After(10 * time.Second):
		require.FailNow("first Events call did not enter backend")
	}
	_, wire := eventsHTTPCall(t, h, "2026-07-28", "events/list", `{}`)
	require.NotNil(wire["error"])
	rpc := eventsWireObject(t, wire["error"])
	assert.Equal(json.Number("-32015"), rpc["code"])
	require.NotNil(rpc["data"])
	assert.Equal("events_unavailable", eventsWireObject(t, rpc["data"])["reason"])
}

func TestEventsStdioPolicyDoesNotExposeMethodsOrReadTools(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := newMCPServer(ServeOptions{Events: eventsTestBackend{}}, true).Connect(ctx, serverTransport, nil)
	require.NoError(err)
	defer func() { assert.NoError(serverSession.Close()) }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-stdio", Version: "1"}, nil)
	require.NoError(sdkmcp.AddSendingCustomMethod[*sdkmcp.ParamsBase, *eventResult](client, "events/list"))
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(err)
	defer func() { assert.NoError(session.Close()) }()
	tools, err := session.ListTools(ctx, nil)
	require.NoError(err)
	for _, tool := range tools.Tools {
		assert.NotEqual("get_mcp_event", tool.Name)
		assert.NotEqual("list_calendar_sources", tool.Name)
	}
	_, err = sdkmcp.CallCustomMethod[*sdkmcp.ParamsBase, *eventResult](ctx, session, "events/list", &sdkmcp.ParamsBase{})
	var rpc *jsonrpc.Error
	require.ErrorAs(err, &rpc)
	assert.Equal(int64(-32601), rpc.Code)
}

func TestEventsParameterNamesAreCaseSensitive(t *testing.T) {
	h := newMCPHTTPServer(ServeOptions{Events: eventsTestBackend{}}, HTTPOptions{APIKey: "owner-fixture"}).Handler
	for _, params := range []string{
		`{"Name":"msgvault.message_archived","arguments":{},"delivery":{"mode":"webhook","url":"https://callback.example/hook"}}`,
		`{"name":"msgvault.message_archived","arguments":{},"delivery":{"mode":"webhook","URL":"https://callback.example/hook"}}`,
	} {
		t.Run(params, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			_, wire := eventsHTTPCall(t, h, "2026-07-28", "events/subscribe", params)
			require.NotNil(wire["error"])
			assert.Equal(json.Number("-32602"), eventsWireObject(t, wire["error"])["code"])
		})
	}
}

func TestEventsDuplicateParametersAreRejectedOnTheWire(t *testing.T) {
	h := newMCPHTTPServer(ServeOptions{Events: eventsTestBackend{}}, HTTPOptions{APIKey: "owner-fixture"}).Handler
	for _, params := range []string{
		`{"name":"msgvault.message_archived","name":"msgvault.draft_changed","arguments":{},"delivery":{"mode":"webhook","url":"https://callback.example/hook"}}`,
		`{"name":"msgvault.message_archived","arguments":{},"delivery":{"mode":"webhook","url":"https://callback.example/first","url":"https://callback.example/second"}}`,
		`{"name":"msgvault.message_archived","arguments":{"conversation_id":"42","conversation_id":"43"},"delivery":{"mode":"webhook","url":"https://callback.example/hook"}}`,
	} {
		t.Run(params, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			_, wire := eventsHTTPCall(t, h, "2026-07-28", "events/subscribe", params)
			require.NotNil(wire["error"])
			assert.Equal(json.Number("-32602"), eventsWireObject(t, wire["error"])["code"])
		})
	}
}

func TestEventsDirectServeTransportSuppressesEvents(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- ServeTransport(ctx, ServeOptions{Events: eventsTestBackend{}}, serverTransport) }()
	var session *sdkmcp.ClientSession
	t.Cleanup(func() {
		if session != nil {
			assert.NoError(session.Close())
		}
		cancel()
		select {
		case err := <-done:
			assert.True(err == nil || errors.Is(err, context.Canceled), "%v", err)
		case <-time.After(30 * time.Second):
			assert.Fail("transport did not stop")
		}
	})
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-direct-transport", Version: "1"}, nil)
	require.NoError(sdkmcp.AddSendingCustomMethod[*sdkmcp.ParamsBase, *eventResult](client, "events/list"))
	var err error
	session, err = client.Connect(ctx, clientTransport, nil)
	require.NoError(err)
	tools, err := session.ListTools(ctx, nil)
	require.NoError(err)
	for _, tool := range tools.Tools {
		assert.NotEqual("get_mcp_event", tool.Name)
		assert.NotEqual("list_calendar_sources", tool.Name)
	}
	_, err = sdkmcp.CallCustomMethod[*sdkmcp.ParamsBase, *eventResult](ctx, session, "events/list", &sdkmcp.ParamsBase{})
	var rpc *jsonrpc.Error
	require.ErrorAs(err, &rpc)
	assert.Equal(int64(-32601), rpc.Code)
}

// The fixture supplies the daemon's millisecond contract. The actual SDK HTTP
// response must expose the profile's ISO8601 expiration, including milliseconds.
type subscriptionExpirationEventsBackend struct {
	eventsTestBackend

	result mcpevents.SubscribeResult
}

func (b subscriptionExpirationEventsBackend) MCPEventsSubscribe(context.Context, mcpevents.SubscribeRequest) (mcpevents.SubscribeResult, error) {
	return b.result, nil
}

func TestEventsSubscribeWireExpirationIsISO8601(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expires   time.Time
		want      string
		truncated bool
	}{
		{"whole seconds", time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC), "2026-10-02T12:00:00Z", false},
		{"millisecond precision", time.Date(2026, time.October, 2, 14, 0, 0, 123000000, time.FixedZone("fixture", 2*60*60)), "2026-10-02T12:00:00.123Z", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			backend := subscriptionExpirationEventsBackend{result: mcpevents.SubscribeResult{
				ID: "sub_fixture", RefreshBefore: tc.expires.UnixMilli(), Cursor: "cursor_fixture", Truncated: tc.truncated,
			}}
			h := newMCPHTTPServer(ServeOptions{Events: backend}, HTTPOptions{APIKey: "owner-fixture"}).Handler
			rec, wire := eventsHTTPCall(t, h, "2026-07-28", "events/subscribe", `{"name":"msgvault.message_archived","arguments":{"conversation_id":"42"},"delivery":{"mode":"webhook","url":"https://callback.example/hook","secret":"whsec_fixture"}}`)
			require.Equal(http.StatusOK, rec.Code)
			require.Nil(wire["error"])
			var response struct {
				Result struct {
					ID            string `json:"id"`
					RefreshBefore string `json:"refreshBefore"`
					Cursor        string `json:"cursor"`
					Truncated     bool   `json:"truncated"`
				} `json:"result"`
			}
			// Decode the raw wire bytes into the client-side timestamp contract:
			// a numeric expiration must fail even if a loose map accepts it.
			require.NoError(json.Unmarshal(rec.Body.Bytes(), &response))
			assert.Equal(tc.want, response.Result.RefreshBefore)
			expiration, err := time.Parse(time.RFC3339Nano, response.Result.RefreshBefore)
			require.NoError(err)
			assert.Equal(tc.expires.UnixMilli(), expiration.UnixMilli())
			assert.Equal(backend.result.ID, response.Result.ID)
			assert.Equal(backend.result.Cursor, response.Result.Cursor)
			assert.Equal(tc.truncated, response.Result.Truncated)
		})
	}
}
