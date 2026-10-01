package api

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
)

func TestMCPCapabilitiesDelegatedProjection(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, registry := newAgentTokenTestServer(t)
	_, secret, _, err := registry.Issue("synthetic-mcp-client",
		[]agentgrant.Permission{agentgrant.PermissionDraftCreate},
		[]agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "sender@example.com"}})
	requirements.NoError(err)
	router := srv.Router()
	read := func(owner, delegated bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/capabilities", nil)
		if owner {
			req.Header.Set("X-Api-Key", agentTokenTestAPIKey)
		}
		if delegated {
			req.Header.Set(apiprotocol.AgentTokenHeader, secret)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	unauthenticated := read(false, false)
	assertions.Equal(http.StatusUnauthorized, unauthenticated.Code)
	owner := read(true, false)
	requirements.Equal(http.StatusOK, owner.Code, owner.Body.String())
	var ownerBody MCPCapabilitiesResponse
	requirements.NoError(json.Unmarshal(owner.Body.Bytes(), &ownerBody))
	assertions.False(ownerBody.Delegated)
	routes := make(map[string]apiprotocol.MCPRouteDescriptor)
	for _, route := range ownerBody.Routes {
		routes[route.OperationID] = route
	}
	requirements.Contains(routes, "listSourceStatus")
	assertions.Equal("/api/v1/sources/status", routes["listSourceStatus"].Path)
	assertions.Equal(http.MethodGet, routes["listSourceStatus"].Method)
	assertions.Contains(routes["listSourceStatus"].QueryParameters, "source_type")
	assertions.NotContains(routes, "issueAgentToken")
	assertions.NotContains(routes, "savePeopleProviderAPIKey")
	delegated := read(false, true)
	requirements.Equal(http.StatusOK, delegated.Code, delegated.Body.String())
	var delegatedBody MCPCapabilitiesResponse
	requirements.NoError(json.Unmarshal(delegated.Body.Bytes(), &delegatedBody))
	assertions.True(delegatedBody.Delegated)
	assertions.Empty(delegatedBody.Routes)
	assertions.Empty(delegatedBody.Commands, "a store without a command descriptor must not invent delegated modes")
	assertions.NotContains(delegated.Body.String(), "sender@example.com")
	assertions.NotContains(delegated.Body.String(), agentTokenTestAPIKey)
}

func TestMCPCapabilitiesReportsActualRegisteredParameters(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	doc := OpenAPIDocument()
	operation := doc.Paths["/api/v1/mcp/capabilities"].Get
	requirements.NotNil(operation)
	assertions.Equal("getMCPCapabilities", operation.OperationID)
	for _, descriptor := range mcpRouteDescriptors(doc) {
		requirements.Contains(doc.Paths, descriptor.Path)
		assertions.NotEmpty(descriptor.OperationID)
	}
}
