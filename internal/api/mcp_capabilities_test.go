package api

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMCPDiscoveryFiltersInboxOperationsByCurrentCallerGrant(t *testing.T) {
	backend := &inboxHTTPStore{}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, backend, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	for _, tc := range []struct {
		name        string
		permissions []agentgrant.Permission
		sources     []agentgrant.SourceRef
		want        []string
	}{
		{"read only", []agentgrant.Permission{agentgrant.PermissionInboxRead}, []agentgrant.SourceRef{{ID: 1, Type: "beeper", Identifier: "account-a"}}, []string{"get-capabilities", "get-state", "list-folders", "receipt-get", "reconcile"}},
		{"archive", []agentgrant.Permission{agentgrant.PermissionInboxRead, agentgrant.PermissionInboxArchive}, []agentgrant.SourceRef{{ID: 1, Type: "beeper", Identifier: "account-a"}}, []string{"archive", "get-capabilities", "get-state", "list-folders", "receipt-get", "reconcile", "unarchive"}},
		{"write without read", []agentgrant.Permission{agentgrant.PermissionInboxArchive}, []agentgrant.SourceRef{{ID: 1, Type: "beeper", Identifier: "account-a"}}, []string{}},
		{"unsupported source", []agentgrant.Permission{agentgrant.PermissionInboxRead, agentgrant.PermissionInboxTag}, []agentgrant.SourceRef{{ID: 1, Type: "slack", Identifier: "workspace-a"}}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			_, secret, grant, err := srv.agentGrants.Issue(tc.name, tc.permissions, tc.sources)
			requirements.NoError(err)
			request := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/capabilities", nil)
			request.Header.Set(apiprotocol.AgentTokenHeader, secret)
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			requirements.Equal(http.StatusOK, response.Code, response.Body.String())
			var descriptor struct {
				Version         int      `json:"version"`
				Delegated       bool     `json:"delegated"`
				InboxOperations []string `json:"inbox_operations"`
			}
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &descriptor))
			assertions.Equal(1, descriptor.Version)
			assertions.True(descriptor.Delegated)
			assertions.Equal(tc.want, descriptor.InboxOperations)
			assertions.NotContains(response.Body.String(), "account-a")
			assertions.NotContains(response.Body.String(), secret)
			assertions.Equal(0, backend.calls, "discovery must not observe a chat or dispatch an action")
			requirements.True(srv.agentGrants.Revoke(grant.ID))
			response = httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			assertions.Equal(http.StatusUnauthorized, response.Code)
		})
	}
}

func TestMCPDiscoveryUsesRegisteredSchemaAndControllerPresence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	for _, implemented := range []bool{false, true} {
		var backend MessageStore = &mockStore{}
		if implemented {
			backend = &inboxHTTPStore{}
		}
		srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, backend, nil, testLogger())
		request := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/capabilities", nil)
		request.Header.Set("Authorization", "Bearer synthetic-owner-key")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		requirements.Equal(http.StatusOK, response.Code)
		assertions.Equal("no-store", response.Header().Get("Cache-Control"))
		var descriptor apiprotocol.MCPCapabilities
		requirements.NoError(json.Unmarshal(response.Body.Bytes(), &descriptor))
		assertions.False(descriptor.Delegated)
		found := false
		for _, route := range descriptor.Routes {
			if route.OperationID == "controlInbox" {
				found = true
				assertions.Equal(http.MethodPost, route.Method)
				assertions.Equal("/api/v1/inbox/control", route.Path)
				assertions.Contains(route.RequestProperties, "expected")
				assertions.Contains(route.RequestProperties, "preview_token")
				assertions.Contains(route.RequestProperties, "idempotency_key")
			}
		}
		assertions.Equal(implemented, found)
		assertions.Equal(implemented, descriptor.HasInboxContract(), "real registered schema must satisfy the client admission predicate")
		if implemented {
			assertions.Len(descriptor.InboxOperations, 12)
		} else {
			assertions.Empty(descriptor.InboxOperations)
		}
	}
}

func TestMCPDiscoverySourceOperationsAreOwnerOnly(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, &mockStore{}, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	_, secret, _, err := srv.agentGrants.Issue("synthetic reader", []agentgrant.Permission{agentgrant.PermissionInboxRead}, []agentgrant.SourceRef{{ID: 1, Type: "gmail", Identifier: "sender@example.com"}})
	requirements.NoError(err)
	for _, delegated := range []bool{false, true} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/capabilities", nil)
		if delegated {
			request.Header.Set(apiprotocol.AgentTokenHeader, secret)
		} else {
			request.Header.Set("Authorization", "Bearer synthetic-owner-key")
		}
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		requirements.Equal(http.StatusOK, response.Code)
		var descriptor apiprotocol.MCPCapabilities
		requirements.NoError(json.Unmarshal(response.Body.Bytes(), &descriptor))
		operations := map[string]apiprotocol.MCPRouteDescriptor{}
		for _, route := range descriptor.Routes {
			operations[route.OperationID] = route
		}
		for _, id := range []string{"listSourceStatus", "listSourceIdentities", "getSchedulerStatus", "triggerSync", "getSettings", "patchSettings", "getParticipant", "getCacheBuildStatus", "getPersonProfile", "patchPerson", "createPerson"} {
			if delegated {
				assertions.NotContains(operations, id)
			} else {
				assertions.Contains(operations, id)
			}
		}
		if !delegated {
			assertions.Contains(operations["listSourceStatus"].QueryParameters, "source_type")
			assertions.Contains(operations["createPerson"].RequestProperties, "display_name")
			assertions.Contains(operations["patchSettings"].RequestProperties, "updates")
		}
	}
}

func TestMCPDiscoveryAdmitsReceiptRecoveryWithoutControllerOnlyWithCurrentScopes(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("scoped-discovery@example.test", "Synthetic Discovery Person", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{BaseURL: "https://contacts.example.test/dav", Username: "synthetic-owner", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/synthetic/", DisplayName: "Synthetic Discovery Book"}}})
	requirements.NoError(err)
	requirements.Len(books, 1)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { assertions.NoError(srv.Shutdown(context.Background())) })
	for _, tc := range []struct {
		name                   string
		permissions            []string
		person, book, admitted bool
	}{
		{"exact scopes", []string{"person.read", "carddav.write"}, true, true, true},
		{"without person read", []string{"carddav.write"}, true, true, false},
		{"without carddav write", []string{"person.read"}, true, true, false},
		{"without person selection", []string{"person.read", "carddav.write"}, false, true, false},
		{"without book selection", []string{"person.read", "carddav.write"}, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			body := map[string]any{"label": "Synthetic recovery discovery", "permissions": tc.permissions}
			if tc.person {
				body["person_ids"] = []int64{person.ID}
			}
			if tc.book {
				body["address_book_ids"] = []int64{books[0].ID}
			}
			response := identityNativeRequest(t, srv, http.MethodPost, agentTokensPath, agentTokenTestAPIKey, false, body)
			requirements.Equal(http.StatusCreated, response.Code)
			var issued agentTokenIssueResponse
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &issued))
			discovery := delegatedPersonNativeRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", issued.Secret, "", nil)
			requirements.Equal(http.StatusOK, discovery.Code)
			var descriptor apiprotocol.MCPCapabilities
			requirements.NoError(json.Unmarshal(discovery.Body.Bytes(), &descriptor))
			var operations []string
			for _, route := range descriptor.Routes {
				operations = append(operations, route.OperationID)
			}
			assertions.Equal(tc.admitted, slices.Contains(operations, "reconcileScopedCardDAVPublication"))
			assertions.NotContains(operations, "previewScopedCardDAVPublication")
			assertions.NotContains(operations, "approveScopedCardDAVPublication")
			requirements.True(srv.agentGrants.Revoke(issued.ID))
			revoked := delegatedPersonNativeRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", issued.Secret, "", nil)
			assertions.Equal(http.StatusUnauthorized, revoked.Code)
		})
	}
}
