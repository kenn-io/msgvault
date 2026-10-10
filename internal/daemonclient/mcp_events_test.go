package daemonclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPEventsAuthenticatedClientContract(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("fixture-owner", r.Header.Get("X-Api-Key"))
		switch r.URL.Path {
		case "/api/v1/mcp/events/list":
			assert.Equal(http.MethodPost, r.Method)
			var body map[string]any
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Empty(body)
			_, _ = w.Write([]byte(`{"events":[{"name":"msgvault.message_archived","delivery":["webhook"]}]}`))
		case "/api/v1/mcp/events/subscribe":
			var body map[string]any
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.NotContains(body, "principal")
			assert.Equal("msgvault.message_archived", body["name"])
			_, _ = w.Write([]byte(`{"id":"sub_fixture","cursor":"cursor_fixture","refreshBefore":100,"truncated":false}`))
		case "/api/v1/mcp/events/unsubscribe":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/mcp/events/event":
			assert.Equal("event/fixture", r.URL.Query().Get("event_id"))
			_, _ = w.Write([]byte(`{"eventId":"event/fixture","name":"msgvault.message_archived","timestamp":"2026-10-05T00:00:00Z","data":{},"cursor":"cursor_fixture"}`))
		case "/api/v1/mcp/events/calendar-sources":
			_, _ = w.Write([]byte(`[{"source_id":"42","summary":"Example calendar","account":"owner@example.org"}]`))
		case "/api/v1/mcp/events/status":
			_, _ = w.Write([]byte(`[{"id":"sub_fixture","name":"msgvault.message_archived","state":"active","scope_id":"42"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := newTestStore(srv, "fixture-owner")
	list, err := c.MCPEventsList(context.Background())
	require.NoError(err)
	require.Len(list.Events, 1)
	sub, err := c.MCPEventsSubscribe(context.Background(), mcpevents.SubscribeRequest{Name: "msgvault.message_archived", Arguments: map[string]any{"conversation_id": "42"}, Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://callback.example/hook", Secret: "whsec_fixture"}})
	require.NoError(err)
	assert.Equal("sub_fixture", sub.ID)
	require.NoError(c.MCPEventsUnsubscribe(context.Background(), mcpevents.UnsubscribeRequest{}))
	event, err := c.GetMCPEvent(context.Background(), "event/fixture")
	require.NoError(err)
	assert.Equal("event/fixture", event.EventID)
	calendars, err := c.ListMCPCalendarSources(context.Background())
	require.NoError(err)
	require.Len(calendars, 1)
	assert.Equal("42", calendars[0].SourceID)
	status, err := c.MCPEventsStatus(context.Background())
	require.NoError(err)
	require.Len(status, 1)
	assert.Equal("active", status[0].State)
}
func TestMCPEventsErrorsNeverExposeDaemonDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		status, code int
		reason       string
	}{
		{"auth", "private daemon diagnostic", 401, -32012, "owner_required"},
		{"safe", "{\"code\":-32602,\"reason\":\"unknown_scope\"}", 400, -32602, "unknown_scope"},
		{"unsafe reason", "{\"code\":-32602,\"reason\":\"https://private.example/secret\"}", 400, -32015, "events_unavailable"},
		{"invalid JSON", "private daemon diagnostic", 500, -32015, "events_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := newTestStore(srv, "fixture-owner")
			_, err := c.MCPEventsList(context.Background())
			var safe *mcpevents.Error
			require.ErrorAs(err, &safe)
			assert.Equal(tc.code, safe.Code)
			assert.Equal(tc.reason, safe.Reason)
			assert.NotContains(err.Error(), "private")
			assert.NotContains(err.Error(), "https://")
		})
	}
}

func TestMCPEventMessageUsesOnlyAuthorizedRoute(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("fixture-owner", r.Header.Get("X-Api-Key"))
		assert.Equal("/api/v1/mcp/events/messages/9007199254740993", r.URL.Path)
		assert.Equal("event/fixture?token", r.URL.Query().Get("event_id"))
		_, _ = w.Write([]byte(`{"id":9007199254740993,"body_text":"synthetic body","body_html":"<p>synthetic body</p>","rfc822_message_id":"fixture@example.org","from":[{"email":"sender@example.org","name":"Example Sender"}]}`))
	}))
	defer srv.Close()
	c := newTestStore(srv, "fixture-owner")
	detail, err := c.GetMCPEventMessage(context.Background(), "event/fixture?token", 9007199254740993)
	require.NoError(err)
	require.NotNil(detail)
	assert.Equal(int64(9007199254740993), detail.ID)
	assert.Equal("synthetic body", detail.BodyText)
	assert.Equal("<p>synthetic body</p>", detail.BodyHTML)
	assert.Equal("fixture@example.org", detail.RFC822MessageID)
	require.Len(detail.From, 1)
	assert.Equal("Example Sender", detail.From[0].Name)
}

func TestMCPEventMessageKeepsLongBodyAvailableForPagination(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	const length = (8 << 20) + 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": int64(42), "body_text": strings.Repeat("x", length)})
	}))
	defer srv.Close()
	c := newTestStore(srv, "fixture-owner")
	detail, err := c.GetMCPEventMessage(context.Background(), "event_fixture", 42)
	require.NoError(err)
	require.NotNil(detail)
	assert.Len(detail.BodyText, length)
}

func TestMCPEventsGeneratedWireContractsPreserveArgumentsAndPayload(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wire   string
		target any
	}{
		{"subscribe", `{"name":"msgvault.draft_changed","arguments":{"conversation_id":"42","kinds":["created"]},"delivery":{"mode":"webhook","url":"https://callback.example/hook"}}`, new(generated.SubscribeMCPEventsBody)},
		{"unsubscribe", `{"name":"msgvault.draft_changed","arguments":{"conversation_id":"42","kinds":["created"]},"delivery":{"mode":"webhook","url":"https://callback.example/hook"}}`, new(generated.UnsubscribeMCPEventsBody)},
		{"envelope", `{"eventId":"event_fixture","name":"msgvault.message_archived","timestamp":"2026-10-06T00:00:00Z","cursor":"cursor_fixture","data":{"message_id":"42","from_me":false}}`, new(generated.GetMCPEventResponse)},
		{"catalog", `{"events":[{"name":"msgvault.message_archived","description":"synthetic","delivery":["webhook"],"inputSchema":{"type":"object","additionalProperties":false},"payloadSchema":{"type":"object"}}]}`, new(generated.ListMCPEventsResponse)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			require.NoError(json.Unmarshal([]byte(tc.wire), tc.target))
			roundtrip, err := json.Marshal(tc.target)
			require.NoError(err)
			assert.JSONEq(tc.wire, string(roundtrip))
		})
	}
}
