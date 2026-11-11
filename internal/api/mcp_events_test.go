package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/testutil"
	pkgclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type countingEventsGate struct{ calls int }

func (g *countingEventsGate) BeginWork() (func(), bool) { g.calls++; return func() {}, true }
func (g *countingEventsGate) BeginWorkContext(context.Context) (func(), bool) {
	g.calls++
	return func() {}, true
}

func TestEventsMutationsUseOperationGate(t *testing.T) {
	gate := &countingEventsGate{}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir()}, MCPEvents: &mcpevents.Service{}, OperationGate: gate, Logger: testLogger()})
	body := `{"name":"msgvault.messages.changed","arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com/hook","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`
	srv.handleMCPEventsList(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	assert.Zero(t, gate.calls)
	srv.handleMCPEventsSubscribe(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body)))
	srv.handleMCPEventsUnsubscribe(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body)))
	assert.Equal(t, 2, gate.calls)
}

func TestOwnerEventsDisabledAndInvalidSubscription(t *testing.T) {
	st := testutil.NewTestStore(t)
	cfg := &config.Config{HomeDir: t.TempDir()}
	events := &mcpevents.Service{Store: st, TokensDir: cfg.TokensDir()}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, MCPEvents: events, Logger: testLogger()})
	request := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/mcp-events/subscribe", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}
	w := request(`{"name":"msgvault.messages.changed","arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	events.Enabled = true
	w = request(`{"name":"msgvault.messages.changed","arguments":{"source_id":1,"source_id":2},"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "-32602")
}

func TestMCPEventsRejectsDelegatedGrant(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t)
	t.Cleanup(reg.Close)
	_, secret, _, err := reg.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "agent@example.com"}}, time.Time{})
	require.NoError(t, err)
	for _, route := range []struct{ method, path string }{{http.MethodGet, "/api/v1/mcp-events/list"}, {http.MethodPost, "/api/v1/mcp-events/subscribe"}, {http.MethodPost, "/api/v1/mcp-events/unsubscribe"}} {
		r := httptest.NewRequestWithContext(t.Context(), route.method, route.path, strings.NewReader(`{}`))
		r.Header.Set(apiprotocol.AgentTokenHeader, secret)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		assert.Equal(t, http.StatusUnauthorized, w.Code, route.path)
	}
}

func TestMCPEventsRejectsRemoteClient(t *testing.T) {
	f := newRemoteClientFixture(t, false)
	for _, route := range []struct{ method, path string }{{http.MethodGet, "/api/v1/mcp-events/list"}, {http.MethodPost, "/api/v1/mcp-events/subscribe"}, {http.MethodPost, "/api/v1/mcp-events/unsubscribe"}} {
		response := f.do(t, remoteClientTestReaderKey, route.method, route.path, "{}")
		assert.Equal(t, http.StatusForbidden, response.Code, route.path)
	}
}

func TestEventsRouterSerialGate(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := &config.Config{HomeDir: t.TempDir()}
	events := &mcpevents.Service{Store: st, TokensDir: cfg.TokensDir(), Enabled: true}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, MCPEvents: events, OperationGate: NewSerialOperationGate(), Logger: testLogger()})
	body := `{"name":"msgvault.messages.changed","arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com/hook","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`
	for _, path := range []string{"/api/v1/mcp-events/subscribe", "/api/v1/mcp-events/unsubscribe"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	}
}

func TestGeneratedEventsCatalogDecodes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := &config.Config{HomeDir: t.TempDir()}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, MCPEvents: &mcpevents.Service{Store: st, TokensDir: cfg.TokensDir(), Enabled: true}, Logger: testLogger()})
	server := httptest.NewServer(srv.Router())
	defer server.Close()
	client, err := pkgclient.New(server.URL)
	requirements.NoError(err)
	catalog, err := client.McpEventsList(t.Context())
	requirements.NoError(err)
	requirements.Len(catalog.Events, 1)
	assertions.True(catalog.Enabled)
	assertions.Equal("object", catalog.Events[0].InputSchema["type"])
	assertions.Equal("object", catalog.Events[0].PayloadSchema["type"])
	response, err := client.McpEventsListWithResponse(t.Context())
	requirements.NoError(err)
	requirements.NotNil(response.JSON200)
	requirements.Len(response.JSON200.Events, 1)
	assertions.Equal("object", response.JSON200.Events[0].InputSchema["type"])
	assertions.Equal("object", response.JSON200.Events[0].PayloadSchema["type"])
}

// decodedEventsError inspects only the generated, decoded response field, not raw bytes.
func decodedEventsError(t *testing.T, response any) map[string]any {
	t.Helper()
	value := reflect.ValueOf(response).Elem()
	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		if strings.HasPrefix(value.Type().Field(i).Name, "JSON") && field.Kind() == reflect.Pointer && !field.IsNil() {
			data, err := json.Marshal(field.Interface())
			require.NoError(t, err)
			var result map[string]any
			require.NoError(t, json.Unmarshal(data, &result))
			return result
		}
	}
	require.FailNow(t, "missing decoded error response")
	return nil
}

func TestGeneratedEventsMutationErrorsDecode(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	cfg := &config.Config{HomeDir: t.TempDir()}
	events := &mcpevents.Service{Store: st, TokensDir: cfg.TokensDir()}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, MCPEvents: events, Logger: testLogger()})
	server := httptest.NewServer(srv.Router())
	defer server.Close()
	client, err := pkgclient.New(server.URL)
	requirements.NoError(err)
	id := int64(1)
	secret := "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"
	arguments := generated.MCPEventsArguments{SourceID: &id}
	delivery := generated.MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: &secret}
	body := generated.MCPEventsSubscribeBody{Name: mcpevents.Name, Arguments: arguments, Delivery: delivery}
	response, err := client.McpEventsSubscribeWithResponse(t.Context(), &generated.McpEventsSubscribeRequestOptions{Body: &body})
	requirements.Error(err)
	requirements.NotNil(response)
	requirements.Equal(http.StatusServiceUnavailable, response.StatusCode)
	assertions.Equal(map[string]any{"code": float64(mcpevents.Unavailable), "reason": "disabled"}, decodedEventsError(t, response))

	// The same status can contain either an Events error or a middleware error.
	source, err := st.GetOrCreateSource("imap", "capacity@example.test")
	requirements.NoError(err)
	id = source.ID
	events.Enabled = true
	events.ConfigTargets = 32
	capacity, err := client.McpEventsSubscribeWithResponse(t.Context(), &generated.McpEventsSubscribeRequestOptions{Body: &body})
	requirements.Error(err)
	requirements.NotNil(capacity)
	requirements.Equal(http.StatusTooManyRequests, capacity.StatusCode)
	assertions.Equal(map[string]any{"code": float64(mcpevents.ResourceExhausted), "reason": "subscriptions"}, decodedEventsError(t, capacity))

	badBody := body
	badBody.Arguments.PersonID = &id
	invalidSubscribe, err := client.McpEventsSubscribeWithResponse(t.Context(), &generated.McpEventsSubscribeRequestOptions{Body: &badBody})
	requirements.Error(err)
	requirements.NotNil(invalidSubscribe)
	requirements.Equal(http.StatusBadRequest, invalidSubscribe.StatusCode)
	assertions.Equal(map[string]any{"code": float64(mcpevents.InvalidParams), "reason": "invalid_arguments"}, decodedEventsError(t, invalidSubscribe))

	missingBody := generated.MCPEventsUnsubscribeBody{Name: mcpevents.Name, Arguments: arguments, Delivery: delivery}
	missing, err := client.McpEventsUnsubscribeWithResponse(t.Context(), &generated.McpEventsUnsubscribeRequestOptions{Body: &missingBody})
	requirements.Error(err)
	requirements.NotNil(missing)
	requirements.Equal(http.StatusNotFound, missing.StatusCode)
	assertions.Equal(map[string]any{"code": float64(mcpevents.NotFound), "reason": "subscription"}, decodedEventsError(t, missing))

	invalid := generated.MCPEventsUnsubscribeBody{Name: "invalid", Arguments: arguments, Delivery: delivery}
	unsub, err := client.McpEventsUnsubscribeWithResponse(t.Context(), &generated.McpEventsUnsubscribeRequestOptions{Body: &invalid})
	requirements.Error(err)
	requirements.NotNil(unsub)
	requirements.Equal(http.StatusNotFound, unsub.StatusCode)
	assertions.Equal(map[string]any{"code": float64(mcpevents.NotFound), "reason": "event"}, decodedEventsError(t, unsub))

	limiter := NewRateLimiter(0, 1)
	defer limiter.Close()
	requirements.True(limiter.Allow("127.0.0.1"))
	limitedServer := httptest.NewServer(RateLimitMiddleware(limiter, nil)(srv.Router()))
	defer limitedServer.Close()
	limitedClient, err := pkgclient.New(limitedServer.URL)
	requirements.NoError(err)
	limited, err := limitedClient.McpEventsSubscribeWithResponse(t.Context(), &generated.McpEventsSubscribeRequestOptions{Body: &body})
	requirements.Error(err)
	requirements.NotNil(limited)
	requirements.Equal(http.StatusTooManyRequests, limited.StatusCode)
	assertions.Equal(map[string]any{"error": "rate_limit_exceeded", "message": "Too many requests. Please slow down."}, decodedEventsError(t, limited))
}
