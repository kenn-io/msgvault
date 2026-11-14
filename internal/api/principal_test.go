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
			cookie, _, err := srv.sessions.create()
			require.NoError(err)
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil)
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
			if tc.mode == AuthModeDelegated {
				assert.Equal("test reader", auth.Principal.DisplayName)
			}
		})
	}
}

func TestRemoteAndKeylessPrincipals(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner-key", RemoteClients: []config.RemoteClientConfig{{ClientID: "writer", APIKey: "remote-key", CollectionsWrite: true}}}}
	srv := &Server{cfg: cfg}
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil)
	r.Header.Set("X-Api-Key", "remote-key")
	auth := srv.classifyAPIRequestDirect(r)
	assert.Equal(authz.KindRemoteClient, auth.Principal.Kind)
	assert.Equal("remote:writer", auth.Principal.ID)
	cfg.Server.APIKey = ""
	for _, address := range []string{"127.0.0.1:1234", "192.0.2.1:1234"} {
		r.RemoteAddr = address
		auth = srv.classifyAPIRequestDirect(r)
		assert.Equal(AuthModeLoopback, auth.Mode)
		assert.Equal("loopback", auth.Principal.ID)
		assert.Equal(authz.KindLoopback, auth.Principal.Kind)
	}
}

func TestLibraryCallerPrincipal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	srv, _ := newAgentTokenTestServer(t)
	t.Cleanup(func() { require.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	var admitted *http.Request
	handler := srv.Handler(func(_ http.ResponseWriter, r *http.Request, _ Operation) bool {
		admitted = r
		return true
	})
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/agent-tokens", nil)
	r.Header.Set("X-Msgvault-Agent-Token", "invalid-daemon-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	require.NotNil(admitted)
	auth := srv.requestAuthentication(admitted)
	assert.Equal(AuthModeCaller, auth.Mode)
	assert.Equal("caller", auth.Principal.ID)
	assert.Equal(authz.KindCaller, auth.Principal.Kind)
}
