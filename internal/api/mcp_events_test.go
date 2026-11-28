package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
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

func TestEventsRouterRejectsUnverifiedCallback(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("imap", "events@example.test")
	require.NoError(t, err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}}
	gate := NewSerialOperationGate()
	events := &mcpevents.Service{Store: st, TokensDir: cfg.TokensDir(), Enabled: true}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, MCPEvents: events, Logger: testLogger(), OperationGate: gate})
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	original := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		once.Do(func() { close(entered) })
		select {
		case <-resume:
			return nil, errors.New("synthetic DNS failure")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	t.Cleanup(func() { net.DefaultResolver = original })
	body := `{"name":"msgvault.messages.changed","arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://callback.invalid/hook","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/mcp-events/subscribe", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- request() }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(resume)
		require.FailNow(t, "verification did not reach DNS")
	}
	gateCtx, cancelGate := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelGate()
	release, ok := gate.BeginWorkContext(gateCtx)
	if !ok {
		close(resume)
		<-result
	}
	require.True(t, ok)
	release()
	gate.StartDrain()
	busy := request()
	assert.Equal(t, http.StatusServiceUnavailable, busy.Code)
	assert.Contains(t, busy.Body.String(), "server_busy")
	close(resume)
	w := <-result
	require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	var response struct {
		Code   int    `json:"code"`
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, -32027, response.Code)
	assert.Equal(t, "connection_refused", response.Reason)
}

func TestMCPEventsRejectsRemoteClient(t *testing.T) {
	f := newRemoteClientFixture(t, false)
	for _, route := range []struct{ method, path string }{{http.MethodGet, "/api/v1/mcp-events/list"}, {http.MethodPost, "/api/v1/mcp-events/subscribe"}, {http.MethodPost, "/api/v1/mcp-events/unsubscribe"}} {
		response := f.do(t, remoteClientTestReaderKey, route.method, route.path, "{}")
		assert.Equal(t, http.StatusForbidden, response.Code, route.path)
	}
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
	delivery := generated.MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: secret}
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

func TestMCPEventsInputSchemasValidate(t *testing.T) {
	for name, doc := range map[string]*huma.OpenAPI{"public": OpenAPIDocument(), "client": openAPIClientDocument()} {
		t.Run(name, func(t *testing.T) {
			registry := doc.Components.Schemas
			arguments := registry.Map()["MCPEventsArguments"]
			require.NotNil(t, arguments)
			for _, tc := range []struct {
				name  string
				value map[string]any
				valid bool
			}{
				{"person", map[string]any{"person_id": 1}, true},
				{"source", map[string]any{"source_id": 1}, true},
				{"neither", map[string]any{}, false},
				{"both", map[string]any{"person_id": 1, "source_id": 1}, false},
				{"zero", map[string]any{"person_id": 0}, false},
				{"negative", map[string]any{"source_id": -1}, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					result := &huma.ValidateResult{}
					huma.Validate(registry, arguments, huma.NewPathBuffer(nil, 0), huma.ModeWriteToServer, tc.value, result)
					assert.Equal(t, tc.valid, len(result.Errors) == 0, "%v", result.Errors)
				})
			}
			for _, tc := range []struct {
				path  string
				value map[string]any
				valid bool
			}{
				{"/api/v1/mcp-events/subscribe", map[string]any{"url": "https://example.com/hook", "mode": "webhook", "secret": "fixture"}, true},
				{"/api/v1/mcp-events/subscribe", map[string]any{"url": "https://example.com/hook", "mode": "webhook"}, false},
				{"/api/v1/mcp-events/subscribe", map[string]any{"url": "https://example.com/hook", "secret": "fixture"}, false},
				{"/api/v1/mcp-events/unsubscribe", map[string]any{"url": "https://example.com/hook", "secret": "fixture"}, true},
				{"/api/v1/mcp-events/unsubscribe", map[string]any{"url": "https://example.com/hook"}, true},
			} {
				body := operationBodySchema(t, doc, doc.Paths[tc.path].Post)
				result := &huma.ValidateResult{}
				huma.Validate(registry, body.Properties["delivery"], huma.NewPathBuffer(nil, 0), huma.ModeWriteToServer, tc.value, result)
				assert.Equal(t, tc.valid, len(result.Errors) == 0, "%s: %v", tc.path, result.Errors)
			}
		})
	}
}

func TestDelegatedSourceScopeAdmission(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st := testutil.NewTestStore(t)
	allowed, _, err := testutil.CreateIndexedSourceMessage(ctx, st, "allowed@example.test", "allowed", "allowed", "")
	require.NoError(err)
	outside, _, err := testutil.CreateIndexedSourceMessage(ctx, st, "outside@example.test", "outside", "outside", "")
	require.NoError(err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true}}
	cfg.MCPEvents.Enabled = true
	registry := agentgrant.NewRegistry()
	t.Cleanup(registry.Close)
	events := &mcpevents.Service{Store: st, TokensDir: cfg.TokensDir(), Enabled: true, Grants: registry}
	grantID, token, _, err := registry.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: allowed.ID, Type: allowed.SourceType, Identifier: allowed.Identifier}}, time.Time{})
	require.NoError(err)
	body := fmt.Sprintf(`{"name":"msgvault.messages.changed","arguments":{"source_id":%d},"delivery":{"mode":"webhook","url":"https://example.com/hook","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`, outside.ID)
	in, err := mcpevents.DecodeSubscribe([]byte(body))
	require.NoError(err)
	_, err = events.Subscribe(ctx, in, agentgrant.Owner(grantID), func(context.Context, string, string, []byte) error { return nil }, nil)
	require.NoError(err)
	// Restart the service to discard its verification cache before the refresh.
	events = &mcpevents.Service{Store: st, TokensDir: cfg.TokensDir(), Enabled: true, Grants: registry}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, MCPEvents: events, AgentGrants: registry, Logger: testLogger()})
	original := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("synthetic DNS failure")
	}}
	t.Cleanup(func() { net.DefaultResolver = original })
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/mcp-events/subscribe", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(apiprotocol.AgentTokenHeader, token)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	assert.Equal(http.StatusForbidden, w.Code)
	assert.Contains(w.Body.String(), "permission_denied")
	raw, err := st.LoadMCPEventsCatalog(ctx)
	require.NoError(err)
	assert.NotEmpty(raw)
}

func TestDelegatedPersonScopeAdmission(t *testing.T) {
	st := testutil.NewTestStore(t)
	allowed, allowedMessage, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "allowed@example.test", "allowed", "allowed", "")
	require.NoError(t, err)
	_, outsideMessage, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "outside@example.test", "outside", "outside", "")
	require.NoError(t, err)
	people := make([]int64, 3)
	participants := make([]int64, 3)
	for i, name := range []string{"inside", "both", "outside"} {
		participants[i], err = st.EnsureParticipant(name+"@example.test", name, "example.test")
		require.NoError(t, err)
		person, _, err := st.CreatePersonFromParticipant(participants[i])
		require.NoError(t, err)
		people[i] = person.ID
	}
	require.NoError(t, st.ReplaceMessageRecipients(allowedMessage, "to", participants[:2], nil))
	require.NoError(t, st.ReplaceMessageRecipients(outsideMessage, "to", participants[1:], nil))
	registry := agentgrant.NewRegistry()
	t.Cleanup(registry.Close)
	_, _, grant, err := registry.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: allowed.ID, Type: allowed.SourceType, Identifier: allowed.Identifier}}, time.Time{})
	require.NoError(t, err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}}
	events := &mcpevents.Service{Store: st, TokensDir: cfg.TokensDir(), Enabled: true, Grants: registry}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, MCPEvents: events, AgentGrants: registry, Logger: testLogger()})
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/mcp-events/subscribe", nil)
	check := func(id int64) error {
		return srv.authorizeMCPEventsGrant(t.Context(), r, &grant, mcpevents.MCPEventsSubscribeRequest{Arguments: mcpevents.MCPEventsArguments{PersonID: id}}, true)
	}
	assert.NoError(t, check(people[0]))
	assert.NoError(t, check(people[1]))
	var outsideErr, missingErr *mcpevents.Error
	require.ErrorAs(t, check(people[2]), &outsideErr)
	require.ErrorAs(t, check(999999999), &missingErr)
	assert.Equal(t, mcpevents.Forbidden, outsideErr.Code)
	assert.Equal(t, "permission_denied", outsideErr.Reason)
	assert.Equal(t, outsideErr, missingErr)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET identifier=? WHERE id=?`), "replacement@example.test", allowed.ID)
	require.NoError(t, err)
	err = srv.authorizeMCPEventsGrant(t.Context(), r, &grant, mcpevents.MCPEventsSubscribeRequest{Arguments: mcpevents.MCPEventsArguments{SourceID: allowed.ID}}, true)
	var renamedErr *mcpevents.Error
	require.ErrorAs(t, err, &renamedErr)
	assert.Equal(t, mcpevents.Forbidden, renamedErr.Code)
	assert.Equal(t, "permission_denied", renamedErr.Reason)
}

func TestMCPEventsRequireSearchPermission(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t)
	t.Cleanup(reg.Close)
	_, secret, _, err := reg.Issue("reader", []agentgrant.Permission{agentgrant.PermissionStatsRead}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "agent@example.com"}}, time.Time{})
	require.NoError(t, err)
	for _, route := range []struct{ method, path string }{{http.MethodGet, "/api/v1/mcp-events/list"}, {http.MethodPost, "/api/v1/mcp-events/subscribe"}, {http.MethodPost, "/api/v1/mcp-events/unsubscribe"}} {
		r := httptest.NewRequestWithContext(t.Context(), route.method, route.path, strings.NewReader(`{}`))
		r.Header.Set(apiprotocol.AgentTokenHeader, secret)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		assert.Equal(t, http.StatusForbidden, w.Code, route.path)
	}
}

func TestEventsInitialSourceAdmissionPreservesDaemonClientReason(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("test", "events@example.test")
	requirements.NoError(err)
	wrapped := &agentReadGateStore{Store: st, sourceErr: errors.New("fixture source failure")}
	registry := agentgrant.NewRegistry()
	t.Cleanup(registry.Close)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true}}
	events := &mcpevents.Service{Store: st, TokensDir: cfg.TokensDir(), Enabled: true, Grants: registry}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: wrapped, MCPEvents: events, AgentGrants: registry, Logger: testLogger()})
	_, token, _, err := registry.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	requirements.NoError(err)

	body := fmt.Sprintf(`{"name":"msgvault.messages.changed","arguments":{"source_id":%d},"delivery":{"mode":"webhook","url":"https://example.com/hook","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`, source.ID)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/mcp-events/subscribe", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(apiprotocol.AgentTokenHeader, token)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	assertions.Equal(http.StatusServiceUnavailable, w.Code)
	var eventErr mcpevents.Error
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &eventErr))
	assertions.Equal(mcpevents.Unavailable, eventErr.Code)
	assertions.Equal("source_scope_unavailable", eventErr.Reason)
}
