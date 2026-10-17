package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/authz"
	"go.kenn.io/msgvault/internal/config"
)

func TestAuthenticationPrincipals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, key, token, cookie string
		mode                     AuthMode
		kind                     authz.Kind
		id                       string
	}{
		{name: "missing", mode: AuthModeRequired},
		{name: "invalid key", key: "invalid", mode: AuthModeRequired},
		{name: "invalid agent", token: "invalid", mode: AuthModeRequired},
		{name: "invalid cookie", cookie: "invalid", mode: AuthModeRequired},
		{name: "owner", key: "owner-key", mode: AuthModeAPIKey, kind: authz.KindOwner, id: "owner"},
		{name: "bearer owner", key: "Bearer owner-key", mode: AuthModeAPIKey, kind: authz.KindOwner, id: "owner"},
		{name: "remote", key: "remote-key", mode: AuthModeRemoteClient, kind: authz.KindRemoteClient, id: "remote:test-cli"},
		{name: "session", cookie: "valid", mode: AuthModeSession, kind: authz.KindOwner, id: "owner"},
		{name: "session retains legacy fallback", key: "invalid", cookie: "valid", mode: AuthModeSession, kind: authz.KindOwner, id: "owner"},
		{name: "grant", token: "valid", mode: AuthModeDelegated, kind: authz.KindGrant, id: "grant:"},
		{name: "mixed grant and key", key: "owner-key", token: "valid", mode: AuthModeRequired},
		{name: "mixed grant and cookie", cookie: "valid", token: "valid", mode: AuthModeRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv := &Server{
				cfg:      &config.Config{Server: config.ServerConfig{APIKey: "owner-key", RemoteClients: []config.RemoteClientConfig{{ClientID: "test-cli", APIKey: "remote-key"}}}},
				sessions: newSessionStore(time.Hour), agentGrants: agentgrant.NewRegistry(),
			}
			defer srv.sessions.Close()
			defer srv.agentGrants.Close()
			grantID, token, _, err := srv.agentGrants.Issue("test reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: 1, Type: "test", Identifier: "reader@example.test"}}, time.Time{})
			require.NoError(err)
			cookie, session, err := srv.sessions.create()
			require.NoError(err)
			r := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
			r.Header.Set("Authorization", tc.key)
			if tc.token != "" {
				value := tc.token
				if value == "valid" {
					value = token
				}
				r.Header.Set("X-Msgvault-Agent-Token", value)
			}
			if tc.cookie != "" {
				value := tc.cookie
				if value == "valid" {
					value = cookie
				}
				r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: value, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			}
			auth := srv.classifyAPIRequestDirect(r)
			assert.Equal(tc.mode, auth.Mode)
			assert.Equal(tc.kind, auth.Principal.Kind)
			id := tc.id
			if tc.mode == AuthModeDelegated {
				id += grantID
			}
			assert.Equal(id, auth.Principal.ID)
			switch tc.mode {
			case AuthModeAPIKey, AuthModeSession:
				assert.Equal(authz.RoleAdmin, auth.Principal.Role)
				assert.True(auth.Principal.Has(authz.PermissionArchiveAdmin))
				assert.False(auth.Principal.Restricted())
				if tc.mode == AuthModeSession {
					assert.Equal(session.ExpiresAt, auth.Principal.ExpiresAt)
				}
			case AuthModeRemoteClient:
				assert.Empty(auth.Principal.Role)
				assert.Equal(authz.PermissionSet{authz.PermissionCLIRead: {}}, auth.Principal.Permissions)
				assert.False(auth.Principal.Has(authz.PermissionSystemRead))
			case AuthModeDelegated:
				assert.Empty(auth.Principal.Role)
				assert.Equal("test reader", auth.Principal.DisplayName)
				assert.Equal(authz.PermissionSet{authz.PermissionMessageRead: {}}, auth.Principal.Permissions)
				assert.True(auth.Principal.Restricted())
				assert.False(auth.Principal.Has(authz.PermissionSearchRead))
			case AuthModeRequired:
				assert.Equal(authz.Principal{}, auth.Principal)
			case AuthModeLoopback, AuthModeCaller:
				require.Fail("unexpected authentication mode in direct credential test", "%s", tc.mode)
			}
		})
	}
}

func TestPrincipalResourceBindingsAreIndependent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	srv := &Server{cfg: &config.Config{Server: config.ServerConfig{APIKey: "owner-key"}}, agentGrants: agentgrant.NewRegistry()}
	defer srv.agentGrants.Close()
	expires := time.Now().Add(time.Hour)
	_, token, grant, err := srv.agentGrants.IssueScoped("bounded", []agentgrant.Permission{agentgrant.PermissionPersonRead}, agentgrant.ResourceScopes{
		Sources:      []agentgrant.SourceRef{{ID: 1, Type: "test", Identifier: "reader@example.test", SenderKeys: []string{"sender@example.test"}}},
		Persons:      []agentgrant.PersonRef{{ID: 2, UID: "synthetic-person"}},
		AddressBooks: []agentgrant.AddressBookRef{{AccountID: 3, BookID: 4, CanonicalURL: "https://example.test/contacts/", OwnershipFingerprint: "synthetic-owner"}},
	}, expires)
	require.NoError(err)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/people/2", nil)
	r.Header.Set("X-Msgvault-Agent-Token", token)
	auth := srv.classifyAPIRequestDirect(r)
	require.NotNil(auth.Principal.Sources)
	// Reusable source IDs are resolved inside beginAgentRead's snapshot. Until
	// then the finite scope is empty; pending durable refs remain on the grant.
	assert.Empty(auth.Principal.Sources.IDs)
	require.NotNil(auth.Principal.Persons)
	require.NotNil(auth.Principal.Books)
	assert.Equal(grant.Persons, auth.Principal.Persons.Persons)
	assert.Equal(grant.AddressBooks, auth.Principal.Books.Books)
	assert.Equal(expires, auth.Principal.ExpiresAt)
	auth.Principal.Persons.Persons[0].UID = "changed"
	auth.Principal.Books.Books[0].OwnershipFingerprint = "changed"
	delete(auth.Principal.Permissions, authz.PermissionPersonRead)
	assert.Equal(grant.Persons, auth.Grant.Persons)
	assert.Equal(grant.AddressBooks, auth.Grant.AddressBooks)
	next := srv.classifyAPIRequestDirect(r)
	assert.Equal(grant.Persons, next.Principal.Persons.Persons)
	assert.True(next.Principal.Has(authz.PermissionPersonRead))
}

func TestRemoteWriteAndKeylessPrincipals(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner-key", RemoteClients: []config.RemoteClientConfig{{ClientID: "writer", APIKey: "remote-key", CollectionsWrite: true}}}}
	srv := &Server{cfg: cfg}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	r.Header.Set("X-Api-Key", "remote-key")
	auth := srv.classifyAPIRequestDirect(r)
	assert.Equal(authz.PermissionSet{authz.PermissionCLIRead: {}, authz.PermissionCollectionsWrite: {}}, auth.Principal.Permissions)
	assert.False(auth.Principal.Has(authz.PermissionArchiveAdmin))
	cfg.Server.APIKey = ""
	for _, address := range []string{"127.0.0.1:1234", "192.0.2.1:1234"} {
		r.RemoteAddr = address
		auth = srv.classifyAPIRequestDirect(r)
		assert.Equal(AuthModeLoopback, auth.Mode)
		assert.Equal("loopback", auth.Principal.ID)
		assert.Equal(authz.KindLoopback, auth.Principal.Kind)
		assert.True(auth.Principal.Has(authz.PermissionArchiveAdmin))
	}
}

func TestExpiredCredentialsHaveNoPrincipal(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		srv := &Server{cfg: &config.Config{Server: config.ServerConfig{APIKey: "owner-key"}}, sessions: newSessionStore(time.Second), agentGrants: agentgrant.NewRegistry()}
		defer srv.sessions.Close()
		defer srv.agentGrants.Close()
		_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: 1, Type: "test", Identifier: "reader@example.test"}}, time.Now().Add(time.Second))
		require.NoError(err)
		cookie, _, err := srv.sessions.create()
		require.NoError(err)
		time.Sleep(2 * time.Second)
		for _, agent := range []bool{true, false} {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
			if agent {
				r.Header.Set("X-Msgvault-Agent-Token", token)
			} else {
				r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			}
			auth := srv.classifyAPIRequestDirect(r)
			assert.Equal(AuthModeRequired, auth.Mode)
			assert.Equal(authz.Principal{}, auth.Principal)
		}
	})
}

func TestLibraryCallerPrincipal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	srv, _ := newAgentTokenTestServer(t)
	t.Cleanup(func() { require.NoError(srv.Shutdown(context.Background())) })
	var admitted *http.Request
	handler := srv.Handler(func(_ http.ResponseWriter, r *http.Request, _ Operation) bool {
		admitted = r
		return true
	})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/agent-tokens", nil)
	r.Header.Set("X-Msgvault-Agent-Token", "invalid-daemon-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	require.NotNil(admitted)
	auth := srv.requestAuthentication(admitted)
	assert.Equal(AuthModeCaller, auth.Mode)
	assert.Equal("caller", auth.Principal.ID)
	assert.Equal(authz.KindCaller, auth.Principal.Kind)
	assert.True(auth.Principal.Has(authz.PermissionArchiveAdmin))
}
