package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestFederationHandshakeCredential(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Federation.Role = "hub"
	cfg.Server.APIKey = "owner-fixture"
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	uid := strings.Repeat("a", 64)
	enrollment, token, err := st.CreateFederationEnrollmentContext(t.Context(), uid, "alice", "")
	requirements.NoError(err)
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
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var body federationHandshakeResponse
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &body))
	hubUID, err := st.ArchiveUIDContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(hubUID, body.HubInstance)
	assertions.Equal([]int{1}, body.ProtocolVersions)
	assertions.Equal("active", body.EnrollmentState)
	entries, err := st.ListFederationEnrollmentsContext(t.Context())
	requirements.NoError(err)
	requirements.Len(entries, 1)
	assertions.Equal(enrollment.EnrollmentUID, entries[0].EnrollmentUID)
	do := func(method, path string, headers http.Header) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), method, path, nil)
		r.Header = headers
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}
	invalid := []http.Header{
		{"Authorization": {"Bearer " + token}},
		{"Authorization": {"Bearer " + token}, "X-Msgvault-Spoke-Instance": {strings.Repeat("b", 64)}},
		{"Authorization": {"Bearer mvf1_unknown"}, "X-Msgvault-Spoke-Instance": {uid}},
		{"Authorization": {"Bearer owner-fixture", "Bearer " + token}, "X-Msgvault-Spoke-Instance": {uid}},
		{"Authorization": {"Bearer " + token, "Bearer owner-fixture"}, "X-Msgvault-Spoke-Instance": {uid}},
		{"Authorization": {"Bearer " + token}, "X-Msgvault-Spoke-Instance": {uid}, "Cookie": {sessionCookieName + "=fixture"}},
		{"X-Api-Key": {"owner-fixture"}, "X-Msgvault-Spoke-Instance": {uid}},
		{"Authorization": {"Bearer " + token}, "X-Api-Key": {"owner-fixture"}, "X-Msgvault-Spoke-Instance": {uid}},
	}
	var first string
	for _, h := range invalid {
		w := do(http.MethodGet, "/api/v1/federation/handshake", h)
		assertions.Equal(http.StatusUnauthorized, w.Code)
		if first == "" {
			first = w.Body.String()
		}
		assertions.Equal(first, w.Body.String())
	}
	requirements.NoError(st.RevokeFederationEnrollmentContext(t.Context(), enrollment.EnrollmentUID))
	revoked := do(http.MethodGet, "/api/v1/federation/handshake", http.Header{"Authorization": {"Bearer " + token}, "X-Msgvault-Spoke-Instance": {uid}})
	assertions.Equal(http.StatusUnauthorized, revoked.Code)
	assertions.Equal(first, revoked.Body.String())
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
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "owner-fixture"
	cfg.Federation.Role = "hub"
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
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
	requirements.Equal(http.StatusCreated, w.Code, w.Body.String())
	var created federationCreateResponse
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &created))
	requirements.NotEmpty(created.Token)
	list := do(http.MethodGet, "/api/v1/federation/enrollments", "")
	requirements.Equal(http.StatusOK, list.Code)
	assertions.Contains(list.Body.String(), created.Enrollment.EnrollmentUID)
	assertions.NotContains(list.Body.String(), created.Token)
	assertions.Equal(http.StatusNotFound, do(http.MethodDelete, "/api/v1/federation/enrollments/missing", "").Code)
	revoked := do(http.MethodDelete, "/api/v1/federation/enrollments/"+created.Enrollment.EnrollmentUID, "")
	assertions.Equal(http.StatusNoContent, revoked.Code)
	list = do(http.MethodGet, "/api/v1/federation/enrollments", "")
	requirements.Equal(http.StatusOK, list.Code)
	var listed federationListResponse
	requirements.NoError(json.Unmarshal(list.Body.Bytes(), &listed))
	requirements.Len(listed.Enrollments, 1)
	assertions.Equal(created.Enrollment.EnrollmentUID, listed.Enrollments[0].EnrollmentUID)
	assertions.NotNil(listed.Enrollments[0].RevokedAt)
	for _, tc := range []struct{ body, message string }{
		{`{"spoke_instance":"bad","member":"alice"}`, "spoke_instance must be 64 lowercase hexadecimal digits"},
	} {
		w := do(http.MethodPost, "/api/v1/federation/enrollments", tc.body)
		assertions.Equal(http.StatusBadRequest, w.Code)
		assertions.Contains(w.Body.String(), tc.message)
	}
	_, err := st.DB().ExecContext(t.Context(), "DROP TABLE federation_enrollments")
	requirements.NoError(err)
	w = do(http.MethodPost, "/api/v1/federation/enrollments", `{"spoke_instance":"`+strings.Repeat("a", 64)+`","member":"alice"}`)
	assertions.Equal(http.StatusInternalServerError, w.Code)
	assertions.Contains(w.Body.String(), "Could not create enrollment")
}

func TestFederationOperationGate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	gate := NewSerialOperationGate()
	release, ok := gate.BeginWorkContext(t.Context())
	requirements.True(ok)
	t.Cleanup(release)

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "owner-fixture"
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, OperationGate: gate, Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	for _, role := range []string{"off", "spoke"} {
		cfg.Federation.Role = role
		for _, tc := range []struct{ method, path string }{{http.MethodPost, "/api/v1/federation/enrollments"}, {http.MethodDelete, "/api/v1/federation/enrollments/fixture"}} {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			r := httptest.NewRequestWithContext(ctx, tc.method, tc.path, nil)
			r.Header.Set("X-Api-Key", "owner-fixture")
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			requirements.NoError(ctx.Err(), "%s %s waited on the gate", tc.method, tc.path)
			assertions.Equal(http.StatusNotFound, w.Code)
			cancel()
			assertions.False(gate.HasRequestWaiters())
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
	assertions.Eventually(gate.HasRequestWaiters, 5*time.Second, 10*time.Millisecond)
	cancel()
	<-done
	assertions.False(gate.HasRequestWaiters())
}

func TestFederationCredentialOnlyHandshake(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "owner-fixture"
	cfg.Federation.Role = "hub"
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	uid := strings.Repeat("a", 64)
	_, token, err := st.CreateFederationEnrollmentContext(t.Context(), uid, "alice", "")
	requirements.NoError(err)
	document := OpenAPIDocument()
	matcher := regexp.MustCompile(`\{[^}]+\}`)
	requests := 0
	seen := false
	for path, item := range document.Paths {
		operations := map[string]bool{"GET": item.Get != nil, "POST": item.Post != nil, "PUT": item.Put != nil, "PATCH": item.Patch != nil, "DELETE": item.Delete != nil}
		for method, present := range operations {
			if !present {
				continue
			}
			if path == "/api/v1/federation/handshake" && method == http.MethodGet {
				seen = true
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
			assertions.Equal(http.StatusForbidden, w.Code, "%s %s: %s", method, path, w.Body.String())
		}
	}
	assertions.True(seen)
	assertions.Greater(requests, 10)
	for _, method := range []string{http.MethodPost, http.MethodTrace, http.MethodHead} {
		r := httptest.NewRequestWithContext(t.Context(), method, "/api/v1/federation/handshake", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Msgvault-Spoke-Instance", uid)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		assertions.Equal(http.StatusForbidden, w.Code, method)
	}
	for _, path := range []string{"/health"} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Msgvault-Spoke-Instance", uid)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		assertions.Equal(http.StatusForbidden, w.Code, path)
	}
}

// TestFederationRoleMatrix exercises authentication and role checks over HTTP.
func TestFederationRoleMatrix(t *testing.T) {
	operations := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/federation/handshake"},
		{http.MethodGet, "/api/v1/federation/identity"},
		{http.MethodGet, "/api/v1/federation/enrollments"},
		{http.MethodPost, "/api/v1/federation/enrollments"},
		{http.MethodDelete, "/api/v1/federation/enrollments/fixture"},
	}
	var unauthorizedBody string
	for _, role := range []string{"off", "spoke"} {
		t.Run(role, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			cfg.Server.APIKey = "owner-fixture"
			cfg.Federation.Role = role
			server := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, Logger: testLogger()})
			t.Cleanup(func() { requirements.NoError(server.Shutdown(context.WithoutCancel(t.Context()))) })
			httpServer := httptest.NewServer(server.Router())
			t.Cleanup(httpServer.Close)
			request := func(method, path string, owner bool) (int, string) {
				t.Helper()
				req, err := http.NewRequestWithContext(t.Context(), method, httpServer.URL+path, nil)
				requirements.NoError(err)
				if owner {
					req.Header.Set("X-Api-Key", "owner-fixture")
				}
				resp, err := httpServer.Client().Do(req)
				requirements.NoError(err)
				defer func() { assertions.NoError(resp.Body.Close()) }()
				body, err := io.ReadAll(resp.Body)
				requirements.NoError(err)
				return resp.StatusCode, string(body)
			}
			for _, op := range operations {
				status, body := request(op.method, op.path, false)
				assertions.Equal(http.StatusUnauthorized, status, "%s %s", op.method, op.path)
				if unauthorizedBody == "" {
					unauthorizedBody = body
				}
				assertions.Equal(unauthorizedBody, body, "%s %s", op.method, op.path)
				status, body = request(op.method, op.path, true)
				if role == "spoke" && op.path == "/api/v1/federation/identity" {
					assertions.Equal(http.StatusOK, status)
					var identity map[string]any
					requirements.NoError(json.Unmarshal([]byte(body), &identity))
					assertions.NotEmpty(identity["archive_uid"])
					assertions.NotContains(identity, "role")
					continue
				}
				assertions.Equal(http.StatusNotFound, status, "%s %s", op.method, op.path)
				const unknown = "/api/v1/no-such-route"
				unknownStatus, unknownBody := request(op.method, unknown, true)
				requirements.Equal(http.StatusNotFound, unknownStatus)
				assertions.Equal(strings.ReplaceAll(unknownBody, unknown, op.path), body)
			}
		})
	}
}

func TestFederationHandshakeOpenAPISecurity(t *testing.T) {
	for name, doc := range map[string]*huma.OpenAPI{"server": OpenAPIDocument(), "client": openAPIClientDocument()} {
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			for _, route := range []string{"/api/v1/federation/handshake", "/api/v1/federation/identity", "/api/v1/federation/enrollments", "/api/v1/federation/enrollments/{uid}"} {
				requirements.NotNil(doc.Paths[route], route)
			}
			path := doc.Paths["/api/v1/federation/handshake"]
			requirements.NotNil(path)
			requirements.NotNil(path.Get)
			assertions.Equal([]map[string][]string{{federationTokenSecurityScheme: {}, federationSpokeSecurityScheme: {}}}, path.Get.Security)
			token := doc.Components.SecuritySchemes[federationTokenSecurityScheme]
			requirements.NotNil(token)
			assertions.Equal("http", token.Type)
			assertions.Equal("bearer", token.Scheme)
			spoke := doc.Components.SecuritySchemes[federationSpokeSecurityScheme]
			requirements.NotNil(spoke)
			assertions.Equal("apiKey", spoke.Type)
			assertions.Equal("header", spoke.In)
			assertions.Equal("X-Msgvault-Spoke-Instance", spoke.Name)
		})
	}
}
