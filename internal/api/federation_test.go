package api

import (
	"context"
	"encoding/json"
	"errors"
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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func federationHubFixture(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Federation.Role = "hub"
	cfg.Server.APIKey = "owner-fixture"
	require.NoError(t, st.ConfigureFederationHubContext(t.Context()))
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	return srv, st
}

type failingFederationCreate struct{ FederationStore }

func (f failingFederationCreate) CreateFederationEnrollmentContext(context.Context, string, string, string) (*store.FederationEnrollment, string, error) {
	return nil, "", errors.New("private fixture error")
}

type observedFederationStore struct {
	FederationStore

	lookups int
	bounded bool
}

func (f *observedFederationStore) AuthenticateFederationEnrollmentContext(ctx context.Context, token, spokeUID string) (*store.FederationEnrollment, error) {
	f.lookups++
	_, f.bounded = ctx.Deadline()
	return f.FederationStore.AuthenticateFederationEnrollmentContext(ctx, token, spokeUID)
}

func TestFederationAuthenticationAfterRateLimitAndDeadline(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, peer string
		limited                  bool
	}{
		{"long CLI route", http.MethodGet, "/api/v1/cli/build-cache", "192.0.2.1:1234", false},
		{"import special case", http.MethodPost, messageImportEndpointPath, "192.0.2.1:1234", false},
		{"loopback limited", http.MethodGet, "/api/v1/federation/handshake", "127.0.0.1:1234", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			srv, st := federationHubFixture(t)
			uid := strings.Repeat("a", 64)
			_, token, err := st.CreateFederationEnrollmentContext(t.Context(), uid, "alice", "")
			requirements.NoError(err)
			observed := &observedFederationStore{FederationStore: st}
			srv.federationStore = observed
			if tc.limited {
				srv.rateLimiter.rate = 0
				srv.rateLimiter.burst = 1
				requirements.True(srv.rateLimiter.Allow(clientIP(&http.Request{RemoteAddr: tc.peer})))
			}
			r := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)
			r.RemoteAddr = tc.peer
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-Msgvault-Spoke-Instance", uid)
			r.Header.Set("X-Msgvault-Client", "cli")
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			if tc.limited {
				assertions.Equal(http.StatusTooManyRequests, w.Code)
				assertions.Zero(observed.lookups)
			} else {
				assertions.Equal(1, observed.lookups)
				assertions.True(observed.bounded)
			}
		})
	}
}

func TestFederationHandshakeCredential(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st := federationHubFixture(t)
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
	assertions.Equal(federationHandshakeResponse{ProtocolVersions: []int{1}, HubInstance: hubUID, EnrollmentState: "active"}, body)
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
	w = request("/api/v1/federation/handshake", uid)
	assertions.Equal(http.StatusUnauthorized, w.Code)
	assertions.Equal(first, w.Body.String())
}

func TestFederationHandshakeLibraryAuthorization(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Federation.Role = "hub"
	// Library hosts can construct a keyless server without daemon startup validation.
	requirements.NoError(st.ConfigureFederationHubContext(t.Context()))
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	spokeUID := strings.Repeat("a", 64)
	_, token, err := st.CreateFederationEnrollmentContext(t.Context(), spokeUID, "alice", "")
	requirements.NoError(err)
	observed := &observedFederationStore{FederationStore: st}
	srv.federationStore = observed
	admit := true
	handler := srv.Handler(func(_ http.ResponseWriter, _ *http.Request, op Operation) bool {
		requirements.Equal(Operation{ID: "getFederationHandshake", Method: http.MethodGet, Path: "/api/v1/federation/handshake"}, op)
		return admit
	})
	request := func(withCredentials bool) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/federation/handshake", nil)
		if withCredentials {
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-Msgvault-Spoke-Instance", spokeUID)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	admit = false
	request(true)
	requirements.Zero(observed.lookups)
	admit = true
	requirements.Equal(http.StatusUnauthorized, request(false).Code)
	w := request(true)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	requirements.Equal(1, observed.lookups)
	requirements.True(observed.bounded)
}

func TestFederationKeylessSpokeIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Federation.Role = "spoke"
	cfg.Server.BindAddr = "0.0.0.0"
	cfg.Server.AllowInsecure = true
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	uid, err := st.ArchiveUIDContext(t.Context())
	requirements.NoError(err)
	for _, tc := range []struct {
		peer   string
		status int
	}{{"127.0.0.1:1234", http.StatusOK}, {"192.0.2.1:1234", http.StatusUnauthorized}} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/federation/identity", nil)
		r.RemoteAddr = tc.peer
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		assertions.Equal(tc.status, w.Code, w.Body.String())
		if tc.status == http.StatusOK {
			var response federationIdentityResponse
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
			assertions.Equal(uid, response.ArchiveUID)
		} else {
			assertions.Contains(w.Body.String(), "Invalid or missing API key")
		}
	}
}

func TestFederationOwnerLifecycle(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st := federationHubFixture(t)
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
	w = do(http.MethodPost, "/api/v1/federation/enrollments", `{"spoke_instance":"bad","member":"alice"}`)
	assertions.Equal(http.StatusBadRequest, w.Code)
	assertions.Contains(w.Body.String(), "spoke_instance must be 64 lowercase hexadecimal digits")
	srv.federationStore = failingFederationCreate{FederationStore: st}
	w = do(http.MethodPost, "/api/v1/federation/enrollments", `{"spoke_instance":"`+strings.Repeat("a", 64)+`","member":"alice"}`)
	assertions.Equal(http.StatusInternalServerError, w.Code)
	assertions.Contains(w.Body.String(), "Could not create enrollment")
	assertions.NotContains(w.Body.String(), "private fixture error")
}

func TestFederationHubOperationGate(t *testing.T) {
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
	cfg.Federation.Role = "hub"
	requirements.NoError(st.ConfigureFederationHubContext(t.Context()))
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, OperationGate: gate, Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
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
	srv, st := federationHubFixture(t)
	uid := strings.Repeat("a", 64)
	_, token, err := st.CreateFederationEnrollmentContext(t.Context(), uid, "alice", "")
	require.NoError(t, err)
	document := OpenAPIDocument()
	matcher := regexp.MustCompile(`\{[^}]+\}`)
	requests := 0
	for path, item := range document.Paths {
		operations := map[string]bool{"GET": item.Get != nil, "POST": item.Post != nil, "PUT": item.Put != nil, "PATCH": item.Patch != nil, "DELETE": item.Delete != nil}
		for method, present := range operations {
			if !present {
				continue
			}
			if path == "/api/v1/federation/handshake" && method == http.MethodGet {
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
			assertions.Contains(w.Body.String(), "Operation is not available to federation credentials", "%s %s", method, path)
		}
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/federation/handshake"},
		{http.MethodTrace, "/api/v1/federation/handshake"},
		{http.MethodHead, "/api/v1/federation/handshake"},
		{http.MethodGet, "/health"},
	} {
		r := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-Msgvault-Spoke-Instance", uid)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		assertions.Equal(http.StatusForbidden, w.Code, "%s %s", tc.method, tc.path)
		assertions.Contains(w.Body.String(), "Operation is not available to federation credentials", "%s %s", tc.method, tc.path)
	}
}

// TestFederationRoleMatrix exercises authentication and role checks over HTTP.
func TestFederationRoleMatrix(t *testing.T) {
	t.Run("hub requires armed store", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		cfg := config.NewDefaultConfig()
		cfg.Data.DataDir = t.TempDir()
		cfg.Federation.Role = "hub"
		opts := ServerOptions{Config: cfg, Store: st, Logger: testLogger()}
		require.PanicsWithValue(t, "api: federation hub role needs its archive Store armed with Store.ConfigureFederationHubContext first", func() {
			NewServerWithOptions(opts)
		})
		require.NoError(t, st.ConfigureFederationHubContext(t.Context()))
		srv := NewServerWithOptions(opts)
		t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	})
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
			gate := NewSerialOperationGate()
			release, ok := gate.BeginWorkContext(t.Context())
			requirements.True(ok)
			t.Cleanup(release)
			server := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, FederationStore: st, OperationGate: gate, Logger: testLogger()})
			t.Cleanup(func() { requirements.NoError(server.Shutdown(context.WithoutCancel(t.Context()))) })
			for _, tc := range []struct{ method, path string }{{http.MethodPost, "/api/v1/federation/enrollments"}, {http.MethodDelete, "/api/v1/federation/enrollments/fixture"}} {
				for _, handler := range []http.Handler{server.Router(), server.Handler(func(http.ResponseWriter, *http.Request, Operation) bool { return true })} {
					ctx, cancel := context.WithTimeout(t.Context(), time.Second)
					r := httptest.NewRequestWithContext(ctx, tc.method, tc.path, nil)
					r.Header.Set("X-Api-Key", "owner-fixture")
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)
					requirements.NoError(ctx.Err(), "%s %s waited on the gate", tc.method, tc.path)
					cancel()
					assertions.Equal(http.StatusNotFound, w.Code)
					assertions.False(gate.HasRequestWaiters())
				}
			}
			release()
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
