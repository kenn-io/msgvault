package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/mcpevents"
)

func TestMCPEventsHealthAdvertisesOnlyToOwner(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	srv, _ := newMCPEventsAPIFixture(t)
	get := func(path, key string) map[string]any {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, req)
		Require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var got map[string]any
		Require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
		return got
	}
	owner := get("/api/v1/health", testSessionAPIKey)
	assert.Equal(true, owner["mcp_events"])
	caps, ok := owner["mcp_event_capabilities"].([]any)
	require.True(ok)
	assert.Len(caps, 5)
	for _, raw := range caps {
		capability, ok := raw.(map[string]any)
		require.True(ok)
		assert.Contains(capability, "family")
		assert.Contains(capability, "source_type")
		assert.Contains(capability, "kinds")
		assert.Contains(capability, "read_tools")
	}
	assert.Equal(APISchemaVersion, owner["api_schema_version"])
	public := get("/health", "")
	assert.NotContains(public, "mcp_events")
	assert.NotContains(public, "mcp_event_capabilities")
	srv.SetMCPEvents(nil)
	disabled := get("/api/v1/health", testSessionAPIKey)
	assert.Equal(false, disabled["mcp_events"])
	assert.NotContains(disabled, "mcp_event_capabilities")
}

func TestMCPEventsHealthOmitsDiscoveryForBrowserAndDelegated(t *testing.T) {
	srv, _ := newMCPEventsAPIFixture(t)
	session := loginSavedViewSession(t, srv)
	registry := agentgrant.NewRegistry()
	t.Cleanup(registry.Close)
	srv.agentGrants = registry
	_, token, _, err := registry.Issue("synthetic-calendar-agent", []agentgrant.Permission{agentgrant.PermissionCalendarEventRead}, []agentgrant.SourceRef{{ID: 1, Type: "gcal", Identifier: "calendar@example.net"}})
	Require.NoError(t, err)
	for name, headers := range map[string]http.Header{"browser": session.mutationHeaders(), "delegated": {apiprotocol.AgentTokenHeader: {token}}} {
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
			request.Header = headers
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			var got map[string]any
			require.NoError(json.Unmarshal(response.Body.Bytes(), &got))
			assert.NotContains(got, "mcp_events")
			assert.NotContains(got, "mcp_event_capabilities")
		})
	}
}

func TestMCPEventsHealthUsesManagedDraftReaders(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	srv, f := newMCPEventsAPIFixture(t)
	service, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{Enabled: true, Sources: []string{"beeper", "slack", "slackdump", "teams", "discord"}, OwnerKey: testSessionAPIKey, KeyPath: filepath.Join(t.TempDir(), "events.key")})
	require.NoError(err)
	srv.SetMCPEvents(service)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	request.Header.Set("Authorization", "Bearer "+testSessionAPIKey)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var health HealthResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &health))
	require.NotNil(health.MCPEvents)
	assert.True(*health.MCPEvents)
	require.Len(health.MCPEventCapabilities, 9)
	for _, capability := range health.MCPEventCapabilities {
		if capability.Family == "msgvault.message_archived" {
			assert.Contains([]string{"beeper", "slack", "teams", "discord"}, capability.SourceType)
			assert.Equal([]string{"message"}, capability.Kinds)
			assert.Equal([]string{"get_message", "list_thread", "get_attachment"}, capability.ReadTools)
			continue
		}
		assert.Equal("msgvault.draft_changed", capability.Family)
		assert.Equal([]string{"draft_get"}, capability.ReadTools)
	}
}

func TestMCPEventsHealthMatrixReaders(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	srv, f := newMCPEventsAPIFixture(t)
	service, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{Enabled: true, Sources: []string{"matrix"}, OwnerKey: testSessionAPIKey, KeyPath: filepath.Join(t.TempDir(), "events.key")})
	require.NoError(err)
	srv.SetMCPEvents(service)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	request.Header.Set("Authorization", "Bearer "+testSessionAPIKey)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var health HealthResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &health))
	require.Len(health.MCPEventCapabilities, 1)
	capability := health.MCPEventCapabilities[0]
	assert.Equal("msgvault.message_archived", capability.Family)
	assert.Equal("matrix", capability.SourceType)
	assert.Equal([]string{"message"}, capability.Kinds)
	assert.Equal([]string{"get_message", "list_thread"}, capability.ReadTools)
}

func TestMCPEventsHealthMSMailReaders(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	srv, f := newMCPEventsAPIFixture(t)
	service, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{Enabled: true, Sources: []string{"msmail"}, OwnerKey: testSessionAPIKey, KeyPath: filepath.Join(t.TempDir(), "events.key")})
	require.NoError(err)
	srv.SetMCPEvents(service)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	request.Header.Set("Authorization", "Bearer "+testSessionAPIKey)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var health HealthResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &health))
	require.Len(health.MCPEventCapabilities, 1)
	capability := health.MCPEventCapabilities[0]
	assert.Equal("msgvault.message_archived", capability.Family)
	assert.Equal("msmail", capability.SourceType)
	assert.Equal([]string{"message"}, capability.Kinds)
	assert.Equal([]string{"get_message", "list_thread", "get_attachment"}, capability.ReadTools)
}
