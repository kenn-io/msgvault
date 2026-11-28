package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/config"
)

func policyTestMux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	api := srv.setupHumaAPI(mux)
	v1 := srv.setupAPIV1Group(api)
	registerRawHumaRoute(v1, rawAPIV1Operation("unclassifiedTestOperation", http.MethodGet, "/policy-test/unclassifiedTestOperation", "Exercise operation authorization"), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
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
		keyless                  bool
	}{
		{name: "owner", key: "owner-key", status: http.StatusNoContent},
		{name: "session", cookie: cookie, status: http.StatusNoContent},
		{name: "remote", key: "remote-key", status: http.StatusForbidden},
		{name: "grant", agent: token, status: http.StatusUnauthorized},
		{name: "keyless", keyless: true, status: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.keyless {
				srv.cfg.Server.APIKey = ""
				defer func() { srv.cfg.Server.APIKey = "owner-key" }()
			}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/policy-test/unclassifiedTestOperation", nil)
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
