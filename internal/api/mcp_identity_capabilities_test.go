package api

import (
	"encoding/json/v2"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// The regression is discovery admitting mutations beyond the caller's current
// action grant, or omitting native scoped identity control entirely.
func TestIdentityMCPDiscoveryAdmitsCurrentScopedActions(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: agentTokenTestAPIKey, AgentAccess: true}}, st, nil, testLogger())
	source, err := st.GetOrCreateSource("gmail", "synthetic@example.test")
	requirements.NoError(err)
	member, err := st.EnsureParticipant("synthetic-curated@example.test", "Synthetic Person", "example.test")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(member)
	requirements.NoError(err)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://contacts.example.test/dav", Username: "synthetic", PrincipalURL: "https://contacts.example.test/principal/synthetic/", HomeURL: "https://contacts.example.test/books/synthetic/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/synthetic/personal/", DisplayName: "Synthetic Contacts"}},
	})
	requirements.NoError(err)
	requirements.Len(books, 1)
	selection, err := st.IdentityGrantSelectionContext(t.Context(), nil, []int64{books[0].ID})
	requirements.NoError(err)
	book := selection.AddressBooks[0]
	for _, tc := range []struct {
		name        string
		permissions []agentgrant.Permission
		scope       agentgrant.ResourceScopes
		want        []string
		wantRead    bool
	}{
		{"source reader", []agentgrant.Permission{agentgrant.PermissionIdentityRead}, agentgrant.ResourceScopes{Sources: []agentgrant.SourceRef{{ID: source.ID, Type: "gmail", Identifier: "synthetic@example.test"}}}, []string{}, true},
		{"source link", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityLink}, agentgrant.ResourceScopes{Sources: []agentgrant.SourceRef{{ID: source.ID, Type: "gmail", Identifier: "synthetic@example.test"}}}, []string{"graph-link", "person-link"}, true},
		{"person unlink", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityUnlink}, agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: person.ID, UID: person.VCardUID}}}, []string{"graph-unlink", "person-unlink"}, true},
		{"missing source", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityLink}, agentgrant.ResourceScopes{Sources: []agentgrant.SourceRef{{ID: source.ID + 1000, Type: "gmail", Identifier: "missing@example.test"}}}, []string{}, false},
		{"reused source", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityLink}, agentgrant.ResourceScopes{Sources: []agentgrant.SourceRef{{ID: source.ID, Type: "gmail", Identifier: "prior@example.test"}}}, []string{}, false},
		{"reused person", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityLink}, agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: person.ID, UID: "prior-person-uid"}}}, []string{}, false},
		{"missing person", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityLink}, agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: person.ID + 1000, UID: "missing-person-uid"}}}, []string{}, false},
		{"reused book owner", []agentgrant.Permission{agentgrant.PermissionIdentityRead, agentgrant.PermissionIdentityLink}, agentgrant.ResourceScopes{Persons: []agentgrant.PersonRef{{ID: person.ID, UID: person.VCardUID}}, AddressBooks: []agentgrant.AddressBookRef{{AccountID: book.AccountID, BookID: book.BookID, CanonicalURL: book.CanonicalURL, OwnershipFingerprint: "prior-owner"}}}, []string{}, false},
		{"write without read", []agentgrant.Permission{agentgrant.PermissionIdentityLink}, agentgrant.ResourceScopes{Sources: []agentgrant.SourceRef{{ID: source.ID, Type: "gmail", Identifier: "synthetic@example.test"}}}, []string{}, false},
		{"unrelated read", []agentgrant.Permission{agentgrant.PermissionSearchRead}, agentgrant.ResourceScopes{Sources: []agentgrant.SourceRef{{ID: source.ID, Type: "gmail", Identifier: "synthetic@example.test"}}}, []string{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			_, secret, grant, err := srv.agentGrants.IssueScoped(tc.name, tc.permissions, tc.scope, time.Time{})
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
