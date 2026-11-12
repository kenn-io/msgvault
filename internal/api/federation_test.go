package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestFederationHandshakeCredential(t *testing.T) {
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Federation.Role = "hub"
	cfg.Server.APIKey = "owner-fixture"
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	uid := strings.Repeat("a", 64)
	enrollment, token, err := st.CreateFederationEnrollmentContext(t.Context(), uid, "alice", "")
	require.NoError(t, err)
	request := func(path, instance string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		if instance != "" {
			r.Header.Set("X-Msgvault-Spoke-Instance", instance)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}
	w := request("/api/v1/federation/handshake", uid)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body federationHandshakeResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, enrollment.EnrollmentUID, body.Enrollment.EnrollmentUID)
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_federation_touch BEFORE UPDATE OF last_seen_at ON federation_enrollments BEGIN SELECT RAISE(FAIL, 'fixture failure'); END`)
	require.NoError(t, err)
	w = request("/api/v1/federation/handshake", uid)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	bad := request("/api/v1/federation/handshake", "")
	wrong := request("/api/v1/federation/handshake", strings.Repeat("b", 64))
	assert.Equal(t, http.StatusUnauthorized, bad.Code)
	assert.Equal(t, bad.Body.String(), wrong.Body.String())
}

func TestFederationRoleAndCredentialConflicts(t *testing.T) {
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "owner-fixture"
	cfg.Federation.Role = "hub"
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	uid := strings.Repeat("a", 64)
	e, token, err := st.CreateFederationEnrollmentContext(t.Context(), uid, "alice", "")
	require.NoError(t, err)
	do := func(method, path string, headers http.Header) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), method, path, nil)
		r.Header = headers
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}
	invalid := []http.Header{
		{"Authorization": {"Bearer owner-fixture", "Bearer " + token}, "X-Msgvault-Spoke-Instance": {uid}},
		{"Authorization": {"Bearer " + token, "Bearer owner-fixture"}, "X-Msgvault-Spoke-Instance": {uid}},
		{"Authorization": {"Bearer " + token}, "X-Msgvault-Spoke-Instance": {uid}, "Cookie": {sessionCookieName + "=fixture"}},
		{"X-Api-Key": {"owner-fixture"}, "X-Msgvault-Spoke-Instance": {uid}},
		{"Authorization": {"Bearer " + token}, "X-Api-Key": {"owner-fixture"}, "X-Msgvault-Spoke-Instance": {uid}},
	}
	var first string
	for _, h := range invalid {
		w := do(http.MethodGet, "/api/v1/federation/handshake", h)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
		if first == "" {
			first = w.Body.String()
		}
		assert.Equal(t, first, w.Body.String())
	}
	require.NoError(t, st.RevokeFederationEnrollmentContext(t.Context(), e.EnrollmentUID))
	revoked := do(http.MethodGet, "/api/v1/federation/handshake", http.Header{"Authorization": {"Bearer " + token}, "X-Msgvault-Spoke-Instance": {uid}})
	assert.Equal(t, first, revoked.Body.String())
}

func TestFederationKeylessSpokeIdentity(t *testing.T) {
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Federation.Role = "spoke"
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/federation/identity", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestFederationOwnerLifecycle(t *testing.T) {
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "owner-fixture"
	cfg.Federation.Role = "hub"
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	do := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		r.Header.Set("X-Api-Key", "owner-fixture")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}
	w := do(http.MethodPost, "/api/v1/federation/enrollments", `{"spoke_instance":"`+strings.Repeat("a", 64)+`","member":"alice"}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var created federationCreateResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	require.NotEmpty(t, created.Token)
	list := do(http.MethodGet, "/api/v1/federation/enrollments", "")
	require.Equal(t, http.StatusOK, list.Code)
	assert.NotContains(t, list.Body.String(), created.Token)
	revoked := do(http.MethodDelete, "/api/v1/federation/enrollments/"+created.Enrollment.EnrollmentUID, "")
	assert.Equal(t, http.StatusNoContent, revoked.Code)
	for _, tc := range []struct{ body, message string }{
		{`{"spoke_instance":"bad","member":"alice"}`, "spoke_instance must be 64 lowercase hexadecimal digits"},
		{`{"spoke_instance":"` + strings.Repeat("a", 64) + `","member":"Alice"}`, "member must be 1–64"},
		{`{"spoke_instance":"` + strings.Repeat("a", 64) + `","member":"alice","spoke_name":"bad\nname"}`, "spoke_name must be at most 128 bytes"},
		{`{"spoke_instance":"` + strings.Repeat("a", 64) + `","member":"alice","spoke_name":"` + strings.Repeat("x", 129) + `"}`, "spoke_name must be at most 128 bytes"},
	} {
		w := do(http.MethodPost, "/api/v1/federation/enrollments", tc.body)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), tc.message)
	}
	_, err := st.DB().ExecContext(t.Context(), "DROP TABLE federation_enrollments")
	require.NoError(t, err)
	w = do(http.MethodPost, "/api/v1/federation/enrollments", `{"spoke_instance":"`+strings.Repeat("a", 64)+`","member":"alice"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "Could not create enrollment")
}

func TestFederationOperationGate(t *testing.T) {
	st := testutil.NewTestStore(t)
	gate := NewSerialOperationGate()
	release, ok := gate.BeginWorkContext(t.Context())
	require.True(t, ok)

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "owner-fixture"
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, OperationGate: gate, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	for _, role := range []string{"off", "spoke"} {
		cfg.Federation.Role = role
		for _, tc := range []struct{ method, path string }{{http.MethodPost, "/api/v1/federation/enrollments"}, {http.MethodDelete, "/api/v1/federation/enrollments/fixture"}} {
			r := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)
			r.Header.Set("X-Api-Key", "owner-fixture")
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			assert.Equal(t, http.StatusNotFound, w.Code)
			assert.False(t, gate.HasRequestWaiters())
		}
	}
	cfg.Federation.Role = "hub"
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/federation/enrollments", strings.NewReader("{}"))
		r.Header.Set("X-Api-Key", "owner-fixture")
		srv.Router().ServeHTTP(httptest.NewRecorder(), r)
	}()
	assert.Eventually(t, gate.HasRequestWaiters, 5*time.Second, 10*time.Millisecond)
	cancel()
	<-done
	release()
	assert.False(t, gate.HasRequestWaiters())
}

func TestFederationCredentialOnlyHandshake(t *testing.T) {
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "owner-fixture"
	cfg.Federation.Role = "hub"
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	uid := strings.Repeat("a", 64)
	_, token, err := st.CreateFederationEnrollmentContext(t.Context(), uid, "alice", "")
	require.NoError(t, err)
	document := OpenAPIDocument()
	for _, path := range []string{"/api/v1/federation/handshake", "/api/v1/federation/identity", "/api/v1/federation/enrollments", "/api/v1/federation/enrollments/{uid}"} {
		require.NotNil(t, document.Paths[path], path)
	}
	matcher := regexp.MustCompile(`\{[^}]+\}`)
	requests := 0
	seen := false
	for path, item := range document.Paths {
		operations := map[string]bool{"GET": item.Get != nil, "POST": item.Post != nil, "PUT": item.Put != nil, "PATCH": item.Patch != nil, "DELETE": item.Delete != nil}
		for method, present := range operations {
			if !present {
				continue
			}
			target := matcher.ReplaceAllString(path, "1")
			r := httptest.NewRequestWithContext(t.Context(), method, target, nil)
			r.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:1234", requests/65536, (requests/256)%256, requests%256)
			requests++
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-Msgvault-Spoke-Instance", uid)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			if path == "/api/v1/federation/handshake" && method == http.MethodGet {
				seen = true
				assert.Equal(t, http.StatusOK, w.Code)
			} else {
				assert.Equal(t, http.StatusForbidden, w.Code, "%s %s: %s", method, path, w.Body.String())
			}
		}
	}
	assert.True(t, seen)
	assert.Greater(t, requests, 10)
	for _, path := range []string{"/", "/health", "/mcp", "/debug/pprof/"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Msgvault-Spoke-Instance", uid)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		assert.Equal(t, http.StatusForbidden, w.Code, path)
	}
}
