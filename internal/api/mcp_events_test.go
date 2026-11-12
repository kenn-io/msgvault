package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/store"
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

func TestEventsListDoesNotUseOperationGate(t *testing.T) {
	gate := &countingEventsGate{}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir()}, MCPEvents: &mcpevents.Service{}, OperationGate: gate, Logger: testLogger()})
	srv.handleMCPEventsList(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	assert.Zero(t, gate.calls)
}

func TestEventsErrorMapsCallbackFailure(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	w := httptest.NewRecorder()
	eventsError(w, &mcpevents.Error{Code: -32027, Reason: "challenge_failed"})
	assertions.Equal(http.StatusBadGateway, w.Code)
	var response mcpevents.Error
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
	assertions.Equal(-32027, response.Code)
	assertions.Equal("challenge_failed", response.Reason)
}

func TestEventsRouterRejectsUnverifiedCallback(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("imap", "events@example.test")
	requirements.NoError(err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}}
	events := &mcpevents.Service{Store: st, TokensDir: cfg.TokensDir(), Enabled: true}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, MCPEvents: events, Logger: testLogger()})
	srv.verifyEventsCallback = func(context.Context, string, string, []byte) error {
		return &mcpevents.Error{Code: mcpevents.CallbackEndpoint, Reason: "dns_error"}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	body := `{"name":"msgvault.messages.changed","arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://callback.invalid/hook","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/mcp-events/subscribe", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	requirements.Equal(http.StatusBadGateway, w.Code, w.Body.String())
	var response struct {
		Code   int    `json:"code"`
		Reason string `json:"reason"`
	}
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
	assertions.Equal(-32027, response.Code)
	assertions.Equal("dns_error", response.Reason)
	catalog, err := st.LoadMCPEventsCatalog(t.Context())
	requirements.NoError(err)
	assertions.Empty(catalog)
	var positions int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM archive_metadata WHERE key LIKE ?`), "mcpevents:v1:pos:%").Scan(&positions))
	assertions.Zero(positions)
	_, err = os.Stat(filepath.Join(cfg.TokensDir(), providercredentials.EventsFilename))
	assertions.ErrorIs(err, os.ErrNotExist)
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
	source, err := st.GetOrCreateSource("imap", "events@example.test")
	requirements.NoError(err)
	requirements.Equal(int64(1), source.ID)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}}
	events := &mcpevents.Service{Store: st, TokensDir: cfg.TokensDir(), Enabled: true}
	gate := NewSerialOperationGate()
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, MCPEvents: events, OperationGate: gate, Logger: testLogger()})
	srv.verifyEventsCallback = func(ctx context.Context, _ string, _ string, _ []byte) error {
		checkCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		release, ok := gate.BeginWorkContext(checkCtx)
		if !ok {
			return &mcpevents.Error{Code: mcpevents.Unavailable, Reason: "unavailable"}
		}
		release()
		return nil
	}
	body := `{"name":"msgvault.messages.changed","arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com/hook","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`
	unsubscribeBody := `{"name":"msgvault.messages.changed","arguments":{"source_id":1},"delivery":{"url":"https://example.com/hook"}}`
	var subscribed mcpevents.MCPEventsSubscribeResult
	for _, path := range []string{"/api/v1/mcp-events/subscribe", "/api/v1/mcp-events/unsubscribe"} {
		requestBody := body
		if strings.HasSuffix(path, "/unsubscribe") {
			requestBody = unsubscribeBody
		}
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(requestBody))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		if strings.HasSuffix(path, "/subscribe") {
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &subscribed))
			requirements.NotEmpty(subscribed.ID)
			_, position, loadErr := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(subscribed.ID))
			requirements.NoError(loadErr)
			requirements.NotNil(position)
		}
	}
	assertions := assert.New(t)
	catalog, err := st.LoadMCPEventsCatalog(t.Context())
	requirements.NoError(err)
	assertions.JSONEq(`[]`, string(catalog))
	_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(subscribed.ID))
	requirements.NoError(err)
	assertions.Nil(position)
	requirements.NoError(gate.Drain(t.Context()))
	before, err := st.LoadMCPEventsCatalog(t.Context())
	requirements.NoError(err)
	for _, path := range []string{"/api/v1/mcp-events/subscribe", "/api/v1/mcp-events/unsubscribe"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		requirements.Equal(http.StatusServiceUnavailable, w.Code, path)
		var response ErrorResponse
		requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
		assertions.Equal("server_busy", response.Error, path)
	}
	after, err := st.LoadMCPEventsCatalog(t.Context())
	requirements.NoError(err)
	assertions.Equal(before, after)
}

func TestGeneratedEventsCatalogDecodes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}}
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
	for i := range value.NumField() {
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
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}}
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
	body := generated.McpEventsSubscribeBody{Name: mcpevents.Name, Arguments: arguments, Delivery: delivery}
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

	missingBody := generated.McpEventsUnsubscribeBody{Name: mcpevents.Name, Arguments: arguments, Delivery: generated.McpEventsUnsubscribeBodyDeliveryStruct{URL: delivery.URL}}
	missing, err := client.McpEventsUnsubscribeWithResponse(t.Context(), &generated.McpEventsUnsubscribeRequestOptions{Body: &missingBody})
	requirements.Error(err)
	requirements.NotNil(missing)
	requirements.Equal(http.StatusNotFound, missing.StatusCode)
	assertions.Equal(map[string]any{"code": float64(mcpevents.NotFound), "reason": "subscription"}, decodedEventsError(t, missing))

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

func TestMCPEventsDeliverySchemas(t *testing.T) {
	for _, doc := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		requirements := require.New(t)
		assertions := assert.New(t)
		for _, tc := range []struct {
			path         string
			modeRequired bool
		}{
			{"/api/v1/mcp-events/subscribe", true},
			{"/api/v1/mcp-events/unsubscribe", false},
		} {
			path := doc.Paths[tc.path]
			requirements.NotNil(path)
			body := operationBodySchema(t, doc, path.Post)
			delivery := body.Properties["delivery"]
			requirements.NotNil(delivery)
			if delivery.Ref != "" {
				delivery = doc.Components.Schemas.Map()[strings.TrimPrefix(delivery.Ref, "#/components/schemas/")]
				requirements.NotNil(delivery)
			}
			assertions.Contains(delivery.Required, "url")
			if tc.modeRequired {
				assertions.Contains(delivery.Required, "mode")
			} else {
				assertions.NotContains(delivery.Required, "mode")
			}
		}
	}
}
