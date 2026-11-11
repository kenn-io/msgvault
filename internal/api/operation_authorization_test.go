package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/authz"
	"go.kenn.io/msgvault/internal/config"
)

func policyTestMux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	api := srv.setupHumaAPI(mux)
	v1 := srv.setupAPIV1Group(api)
	for _, id := range []string{"unclassifiedTestOperation"} {
		registerRawHumaRoute(v1, rawAPIV1Operation(id, http.MethodGet, "/policy-test/"+id, "Exercise operation authorization"), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	}
	return mux
}

func TestRoleOperationPolicyAuthorization(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		role                     authz.Role
		read, edit, unclassified bool
	}{
		{authz.RoleAdmin, true, true, true},
		{authz.RoleMember, true, true, false},
		{authz.RoleViewer, true, false, false},
		{"unknown", false, false, false},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			principal := authz.Principal{Role: tc.role}
			for id, allowed := range map[string]bool{
				"getStats":                  tc.read,
				"patchPerson":               tc.edit,
				"unclassifiedTestOperation": tc.unclassified,
			} {
				assert.Equal(t, allowed, principal.Has(operationPolicy(id).Permission), "%s", id)
			}
		})
	}
}

func TestUnknownOperationRetainsCredentialBoundariesHTTP(t *testing.T) {
	t.Parallel()
	srv := &Server{
		cfg:    &config.Config{Server: config.ServerConfig{APIKey: "owner-key", RemoteClients: []config.RemoteClientConfig{{ClientID: "test-cli", APIKey: "remote-key", CollectionsWrite: true}}}},
		logger: testLogger(), sessions: newSessionStore(time.Hour), agentGrants: agentgrant.NewRegistry(),
	}
	defer srv.sessions.Close()
	defer srv.agentGrants.Close()
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: 1, Type: "test", Identifier: "reader@example.test"}}, time.Time{})
	require.NoError(t, err)
	cookie, _, err := srv.sessions.create()
	require.NoError(t, err)
	handler := srv.requestSecurityMiddleware(policyTestMux(srv))
	for _, tc := range []struct {
		name, key, agent, cookie string
		status                   int
	}{
		{name: "owner", key: "owner-key", status: http.StatusNoContent},
		{name: "session", cookie: cookie, status: http.StatusNoContent},
		{name: "remote", key: "remote-key", status: http.StatusForbidden},
		{name: "grant", agent: token, status: http.StatusUnauthorized},
		{name: "mixed", key: "owner-key", agent: token, status: http.StatusUnauthorized},
		{name: "invalid", key: "invalid", status: http.StatusUnauthorized},
		{name: "missing", status: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/policy-test/unclassifiedTestOperation", nil)
			if tc.key != "" {
				r.Header.Set("X-Api-Key", tc.key)
			}
			if tc.agent != "" {
				r.Header.Set("X-Msgvault-Agent-Token", tc.agent)
			}
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tc.cookie, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			assert.Equal(t, tc.status, w.Code, w.Body.String())
		})
	}
}

func TestPrivilegedPredicatesRequirePrincipalAndTransport(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	for _, tc := range []struct {
		name              string
		mode              AuthMode
		principal         authz.Principal
		privileged, owner bool
	}{
		{"key", AuthModeAPIKey, authz.Principal{Kind: authz.KindOwner, Role: authz.RoleAdmin}, true, true},
		{"session", AuthModeSession, authz.Principal{Kind: authz.KindOwner, Role: authz.RoleAdmin}, true, false},
		{"loopback", AuthModeLoopback, authz.Principal{Kind: authz.KindLoopback, Role: authz.RoleAdmin}, true, false},
		{"caller", AuthModeCaller, authz.Principal{Kind: authz.KindCaller, Role: authz.RoleAdmin}, true, true},
		{"remote", AuthModeRemoteClient, authz.Principal{Kind: authz.KindRemoteClient, Permissions: authz.PermissionSet{authz.PermissionCLIRead: {}}}, false, false},
		{"grant", AuthModeDelegated, authz.Principal{Kind: authz.KindGrant, Permissions: authz.PermissionSet{authz.PermissionMessageRead: {}}}, false, false},
		{"unauthenticated", AuthModeRequired, authz.Principal{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/agent-tokens", nil)
			security := requestSecurity{auth: requestAuthentication{Mode: tc.mode, Principal: tc.principal}}
			r = r.WithContext(context.WithValue(r.Context(), requestSecurityContextKey{}, security))
			assert.Equal(t, tc.privileged, srv.apiRequestAuthorized(r))
			assert.Equal(t, tc.owner, srv.ownerAccessAuthorized(r))
		})
	}
}
