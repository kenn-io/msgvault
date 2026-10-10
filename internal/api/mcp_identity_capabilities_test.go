package api

import (
	"encoding/json/v2"
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

// The regression is discovery admitting mutations beyond the caller's current
// action grant, or omitting native scoped identity control entirely.
func TestIdentityMCPDiscoveryAdmitsCurrentScopedActions(t *testing.T) {
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	for _, tc := range []struct {
		name        string
		permissions []agentgrant.Permission
		scope       agentgrant.ResourceScopes
		want        []string
		wantRead    bool
	}{
		{"source reader", []agentgrant.Permission{agentgrant.PermissionIdentityRead}, agentgrant.ResourceScopes{Sources: []agentgrant.SourceRef{{ID: 1, Type: "gmail", Identifier: "synthetic@example.test"}}}, []string{}, true},
		{"source link", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityLink}, agentgrant.ResourceScopes{Sources: []agentgrant.SourceRef{{ID: 1, Type: "gmail", Identifier: "synthetic@example.test"}}}, []string{"graph-link", "person-link"}, true},
		{"person unlink", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityUnlink}, agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: 1, UID: "synthetic-person"}}}, []string{"graph-unlink", "person-unlink"}, true},
		{"write without read", []agentgrant.Permission{agentgrant.PermissionIdentityLink}, agentgrant.ResourceScopes{Sources: []agentgrant.SourceRef{{ID: 1, Type: "gmail", Identifier: "synthetic@example.test"}}}, []string{}, false},
		{"unrelated read", []agentgrant.Permission{agentgrant.PermissionInboxRead}, agentgrant.ResourceScopes{Sources: []agentgrant.SourceRef{{ID: 1, Type: "gmail", Identifier: "synthetic@example.test"}}}, []string{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			_, secret, grant, err := srv.agentGrants.IssueScoped(tc.name, tc.permissions, tc.scope)
			requirements.NoError(err)
			response := identityNativeRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", secret, true, nil)
			requirements.Equal(http.StatusOK, response.Code, response.Body.String())
			var descriptor struct {
				IdentityOperations []string                         `json:"identity_operations"`
				Routes             []apiprotocol.MCPRouteDescriptor `json:"routes"`
			}
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &descriptor))
			assertions.Equal(tc.want, descriptor.IdentityOperations)
			ids := []string{}
			for _, route := range descriptor.Routes {
				ids = append(ids, route.OperationID)
			}
			if tc.wantRead {
				assertions.Contains(ids, "previewIdentityOperation")
				assertions.Contains(ids, "getIdentityOperationReceipt")
			} else {
				assertions.NotContains(ids, "previewIdentityOperation")
				assertions.NotContains(ids, "getIdentityOperationReceipt")
			}
			assertions.Equal(len(tc.want) > 0, slices.Contains(ids, "applyIdentityOperation"))
			assertions.NotContains(response.Body.String(), "synthetic@example.test")
			requirements.True(srv.agentGrants.Revoke(grant.ID))
			revoked := identityNativeRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", secret, true, nil)
			assertions.Equal(http.StatusUnauthorized, revoked.Code)
		})
	}
}

func TestIdentityMCPDiscoveryRequiresNativeBackend(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, implemented := range []bool{false, true} {
		var backend MessageStore = &mockStore{}
		if implemented {
			backend = testutil.NewTestStore(t)
		}
		srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey}}, backend, nil, testLogger())
		response := identityNativeRequest(t, srv, http.MethodGet, "/api/v1/mcp/capabilities", agentTokenTestAPIKey, false, nil)
		requirements.Equal(http.StatusOK, response.Code)
		var descriptor apiprotocol.MCPCapabilities
		requirements.NoError(json.Unmarshal(response.Body.Bytes(), &descriptor))
		assertions.Equal(implemented, descriptor.HasIdentityPreviewContract())
		assertions.Equal(implemented, descriptor.HasIdentityApplyContract())
		assertions.Equal(implemented, descriptor.HasIdentityReceiptContract())
		if implemented {
			assertions.Equal([]string{"graph-link", "graph-unlink", "person-link", "person-unlink"}, descriptor.IdentityOperations)
		} else {
			assertions.Empty(descriptor.IdentityOperations)
		}
	}
}
