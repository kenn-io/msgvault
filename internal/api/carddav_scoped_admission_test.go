package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mscontacts"
	"go.kenn.io/msgvault/internal/testutil"
)

func publicationNativeOwnerRequest(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+agentTokenTestAPIKey)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	return w
}

func TestScopedCardDAVRoutesValidatePersonBeforeProviderWork(t *testing.T) {
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey}},
		Store:  st, Logger: testLogger(),
	})
	t.Cleanup(func() { assert.NoError(t, srv.Shutdown(context.Background())) })
	for _, endpoint := range []struct{ method, suffix string }{
		{http.MethodGet, "preview"},
		{http.MethodPost, "approve"},
		{http.MethodPost, "reconcile"},
	} {
		t.Run(endpoint.suffix, func(t *testing.T) {
			response := publicationNativeOwnerRequest(t, srv, endpoint.method,
				"/api/v1/carddav/scoped/publications/0/"+endpoint.suffix,
				map[string]any{"approval_token": "synthetic", "idempotency_key": "synthetic"})
			require.Equal(t, http.StatusBadRequest, response.Code, "%s", response.Body.String())
			assert.Contains(t, response.Body.String(), "bad_request")
		})
	}
}

func TestScopedCardDAVDiscoveryRetainsRecoveryWithoutTransport(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey}},
		Store:  st, Logger: testLogger(),
	})
	t.Cleanup(func() {
		assertions.NoError(srv.Shutdown(context.Background()))
	})
	response := publicationNativeOwnerRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", nil)
	requirements.Equal(http.StatusOK, response.Code, "%s", response.Body.String())
	assertions.Equal("no-store", response.Header().Get("Cache-Control"))
	var discovery apiprotocol.MCPCapabilities
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &discovery))
	discovery.Routes = publicationDiscoveryRoutes(discovery.Routes)
	assertions.Equal(1, discovery.Version)
	assertions.False(discovery.Delegated)
	requirements.Len(discovery.Routes, 1)
	assertions.Equal("reconcileScopedCardDAVPublication", discovery.Routes[0].OperationID)
	assertions.Equal(http.MethodPost, discovery.Routes[0].Method)
	assertions.Equal("/api/v1/carddav/scoped/publications/{person_id}/reconcile", discovery.Routes[0].Path)
	assertions.ElementsMatch([]string{"approval_token", "idempotency_key"}, discovery.Routes[0].RequestProperties)
}

func TestScopedCardDAVDiscoveryOmitsUnavailableControllerWrites(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey}}
	// Native startup retains a controller even when credentials are absent.
	controller, err := NewCardDAVController(cfg, st, testLogger())
	requirements.NoError(err)
	requirements.Nil(controller.Current())
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Logger: testLogger(), CardDAV: controller})
	t.Cleanup(func() {
		assertions.NoError(srv.Shutdown(context.Background()))
	})
	response := publicationNativeOwnerRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", nil)
	requirements.Equal(http.StatusOK, response.Code, "%s", response.Body.String())
	var descriptor apiprotocol.MCPCapabilities
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &descriptor))
	descriptor.Routes = publicationDiscoveryRoutes(descriptor.Routes)
	requirements.Len(descriptor.Routes, 1, "an unavailable live controller cannot admit preview or approval")
	assertions.Equal("reconcileScopedCardDAVPublication", descriptor.Routes[0].OperationID)
}

func TestScopedCardDAVDiscoveryOmitsOwnerOnlyMicrosoftTransport(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey}, CardDAV: config.CardDAVConfig{BaseURL: mscontacts.GraphBaseURL, Username: "synthetic-graph@example.test"}}
	// Use the actual owner-only Graph transport. Discovery must make no request;
	// its token source intentionally cannot authorize any provider work.
	service := carddav.NewRemoteService(st, mscontacts.NewRemote(mscontacts.GraphBaseURL, nil))
	controller := &CardDAVController{cfg: cfg, store: st, service: service}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Logger: testLogger(), CardDAV: controller})
	t.Cleanup(func() {
		assertions.NoError(srv.Shutdown(context.Background()))
	})
	response := publicationNativeOwnerRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", nil)
	requirements.Equal(http.StatusOK, response.Code, "%s", response.Body.String())
	var descriptor apiprotocol.MCPCapabilities
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &descriptor))
	descriptor.Routes = publicationDiscoveryRoutes(descriptor.Routes)
	requirements.Len(descriptor.Routes, 1, "owner-only transports cannot admit scoped provider work")
	assertions.Equal("reconcileScopedCardDAVPublication", descriptor.Routes[0].OperationID)
}

// Assert the publication subset without excluding the identity contracts served
// by the shared endpoint. The Handler test checks their combined admission.
func publicationDiscoveryRoutes(routes []apiprotocol.MCPRouteDescriptor) []apiprotocol.MCPRouteDescriptor {
	var publication []apiprotocol.MCPRouteDescriptor
	for _, route := range routes {
		if strings.HasPrefix(route.Path, "/api/v1/carddav/scoped/publications/") {
			publication = append(publication, route)
		}
	}
	return publication
}
