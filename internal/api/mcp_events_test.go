package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func newMCPEventsAPIFixture(t *testing.T) (*Server, *storetest.Fixture) {
	t.Helper()
	f := storetest.New(t)
	svc, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{Enabled: true, Retention: 7 * 24 * time.Hour, Sources: []string{"gmail", "imap", "gcal"}, KeyPath: filepath.Join(t.TempDir(), "mcp-events.key"), OwnerKey: testSessionAPIKey})
	Require.NoError(t, err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: testSessionAPIKey}}, Store: f.Store, Logger: testLogger(), MCPEvents: svc})
	t.Cleanup(func() { Require.NoError(t, srv.Shutdown(context.Background())) })
	return srv, f
}

func mcpEventsAPIRequest(t *testing.T, srv *Server, path, body string, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://example.com/api/v1/mcp/events/"+path, bytes.NewBufferString(body))
	r.RemoteAddr = "192.0.2.10:4242"
	r.Header.Set("Content-Type", "application/json")
	maps.Copy(r.Header, headers)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	return w
}

func TestMCPEventsAPIDerivesOwnerAndRejectsOtherAuthModes(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	srv, _ := newMCPEventsAPIFixture(t)
	owner := http.Header{"Authorization": {"Bearer " + testSessionAPIKey}}
	response := mcpEventsAPIRequest(t, srv, "list", `{}`, owner)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var catalog mcpevents.ListResult
	require.NoError(json.Unmarshal(response.Body.Bytes(), &catalog))
	require.Len(catalog.Events, 3)
	assert.Equal("no-store", response.Header().Get("Cache-Control"))
	unauthenticated := mcpEventsAPIRequest(t, srv, "list", `{}`, nil)
	assert.Equal(http.StatusUnauthorized, unauthenticated.Code)
	session := loginSavedViewSession(t, srv)
	browser := mcpEventsAPIRequest(t, srv, "list", `{}`, session.mutationHeaders())
	assert.Equal(http.StatusForbidden, browser.Code, browser.Body.String())
	registry := agentgrant.NewRegistry()
	t.Cleanup(registry.Close)
	srv.agentGrants = registry
	_, token, _, err := registry.Issue("synthetic-calendar-agent", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}, []agentgrant.SourceRef{{ID: 1, Type: "gcal", Identifier: "calendar@example.net"}}, time.Time{})
	require.NoError(err)
	delegated := mcpEventsAPIRequest(t, srv, "list", `{}`, http.Header{apiprotocol.AgentTokenHeader: {token}})
	assert.Equal(http.StatusForbidden, delegated.Code, delegated.Body.String())
	srv.cfg.Server.RemoteClients = []config.RemoteClientConfig{{ClientID: "fixture-reader", APIKey: remoteClientTestReaderKey}}
	reader := mcpEventsAPIRequest(t, srv, "list", `{}`, http.Header{"Authorization": {"Bearer " + remoteClientTestReaderKey}})
	assert.Equal(http.StatusForbidden, reader.Code, reader.Body.String())
	keyless := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Logger: testLogger(), Store: srv.store, MCPEvents: srv.mcpEvents})
	t.Cleanup(func() { Require.NoError(t, keyless.Shutdown(context.Background())) })
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/mcp/events/list", bytes.NewBufferString(`{}`))
	request.RemoteAddr = "127.0.0.1:4242"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1")
	loopback := httptest.NewRecorder()
	keyless.Router().ServeHTTP(loopback, request)
	assert.Equal(http.StatusForbidden, loopback.Code, loopback.Body.String())
	assert.Contains(loopback.Body.String(), `"reason":"owner_required"`)
}

func TestMCPEventsAPIClosedRequestsRejectForgedPrincipal(t *testing.T) {
	assert := Assert.New(t)
	srv, f := newMCPEventsAPIFixture(t)
	owner := http.Header{"Authorization": {"Bearer " + testSessionAPIKey}}
	for _, body := range []string{`{"principal":"owner:forged"}`, `null`, `{} {}`, `{"Name":"msgvault.message_archived"}`} {
		response := mcpEventsAPIRequest(t, srv, "list", body, owner)
		assert.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	}
	response := mcpEventsAPIRequest(t, srv, "subscribe", fmt.Sprintf(`{"name":"msgvault.message_archived","arguments":{"conversation_id":"%d"},"delivery":{"mode":"webhook","url":"https://receiver.example.net/hook","secret":"whsec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},"principal":"owner:forged"}`, f.ConvID), owner)
	assert.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), `"reason":"invalid_request"`)
	rows, err := f.Store.ListMCPSubscriptions(t.Context(), mcpevents.Principal(testSessionAPIKey))
	Require.NoError(t, err)
	assert.Empty(rows)
}

func TestMCPEventsAPIStatusOmitsCallbackSecretsAndPrincipal(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	srv, f := newMCPEventsAPIFixture(t)
	now := time.Now().UTC()
	principal := mcpevents.Principal(testSessionAPIKey)
	input := store.MCPSubscription{ID: fmt.Sprintf("sub_%064x", 1), Principal: principal, Name: "msgvault.message_archived", Arguments: []byte(`{}`), ScopeKind: "conversation", ScopeID: f.ConvID, SourceID: f.Source.ID, CallbackURL: "https://receiver.example.net/synthetic-private-callback", SecretEnc: []byte("synthetic-ciphertext-marker"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour)}
	require.NoError(f.Store.BindMCPSubscriptionScope(t.Context(), &input))
	_, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/events/status", nil)
	r.Header.Set("Authorization", "Bearer "+testSessionAPIKey)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	var rows []mcpevents.SubscriptionStatus
	require.NoError(json.Unmarshal(w.Body.Bytes(), &rows))
	require.Len(rows, 1)
	assert.Equal(input.ID, rows[0].ID)
	assert.NotContains(w.Body.String(), input.CallbackURL)
	assert.NotContains(w.Body.String(), string(input.SecretEnc))
	assert.NotContains(w.Body.String(), principal)
	assert.NotContains(w.Body.String(), "secret_enc")
	assert.NotContains(w.Body.String(), "callback_url")
}

func TestMCPEventsAPIStatusReportsKeyFailureWithoutAdvertising(t *testing.T) {
	srv, _ := newMCPEventsAPIFixture(t)
	srv.SetMCPEvents(nil)
	srv.SetMCPEventsUnavailable(&mcpevents.Error{Code: -32015, Reason: "events_key_unavailable"})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/events/status", nil)
	request.Header.Set("Authorization", "Bearer "+testSessionAPIKey)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	Require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	Assert.Contains(t, response.Body.String(), `"reason":"events_key_unavailable"`)
}
