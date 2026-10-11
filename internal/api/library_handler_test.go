package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func newLibraryHandlerTestServer(t *testing.T) *Server {
	t.Helper()
	gate := NewSerialOperationGate()
	srv := NewServerWithOptions(ServerOptions{
		Config:        &config.Config{},
		Store:         &stubSourceStore{},
		Logger:        testLogger(),
		Scheduler:     newMockScheduler(),
		OperationGate: gate,
	})
	holderDone, ok := gate.BeginRequestWorkContext(t.Context(), "host maintenance")
	require.True(t, ok)
	t.Cleanup(holderDone)
	orig := operationGateWaitLimit
	operationGateWaitLimit = time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = orig })
	return srv
}

func TestLibraryHandlerGatesAuthorizedMutations(t *testing.T) { //nolint:paralleltest // swaps the package-level operationGateWaitLimit
	srv := newLibraryHandlerTestServer(t)
	handler := srv.Handler(func(http.ResponseWriter, *http.Request, Operation) bool { return true })

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/cli/rebuild-fts", nil))

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "operation_in_progress")
	assert.NotEmpty(t, w.Header().Get("X-Request-ID"))
}

func TestLibraryHandlerAuthorizesBeforeGate(t *testing.T) { //nolint:paralleltest // swaps the package-level operationGateWaitLimit
	srv := newLibraryHandlerTestServer(t)
	var seen Operation
	handler := srv.Handler(func(w http.ResponseWriter, _ *http.Request, op Operation) bool {
		seen = op
		w.WriteHeader(http.StatusForbidden)
		return false
	})

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/cli/rebuild-fts", nil))

	assert.Equal(t, http.StatusForbidden, w.Code, "rejected requests must not wait on or observe the gate")
	assert.NotContains(t, w.Body.String(), "host maintenance")
	assert.Equal(t, Operation{ID: "rebuildCLIFTS", Method: http.MethodPost, Path: "/api/v1/cli/rebuild-fts"}, seen)
}

func TestLibraryHandlerClientClassDoesNotLiftTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cli    bool
		header string
		value  string
	}{
		{name: "CLI without daemon credentials", cli: true},
		{name: "CLI with daemon key", cli: true, header: "X-Api-Key", value: "synthetic-key"},
		{name: "CLI with invalid daemon credentials", cli: true, header: "X-Msgvault-Agent-Token", value: "invalid"},
		{name: "ordinary request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				called := false
				st := &mockStore{getStatsContextFunc: func(ctx context.Context) error {
					called = true
					select {
					case <-time.After(2 * time.Second):
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}}
				srv := NewServerWithOptions(ServerOptions{
					Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-key"}},
					Store:  st, Logger: testLogger(), RequestTimeout: time.Second,
				})
				t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
				handler := srv.Handler(func(http.ResponseWriter, *http.Request, Operation) bool { return true })
				r := httptest.NewRequest(http.MethodGet, "https://archive.example/api/v1/cli/stats", nil)
				r.RemoteAddr = "192.0.2.10:1234"
				if tc.cli {
					r.Header.Set("X-Msgvault-Client", "cli")
				}
				if tc.header != "" {
					r.Header.Set(tc.header, tc.value)
				}
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				assert.True(t, called, "request must reach the operation")
				assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			})
		})
	}
}

func TestLibraryHandlerOwnsAuthentication(t *testing.T) {
	for _, tc := range []struct {
		path   string
		status int
		body   string
	}{
		{"/api/v1/agent-tokens", http.StatusOK, `"tokens":[]`},
		{"/api/v1/settings/people-inference/codex/login/missing", http.StatusNotFound, `"error":"codex_login_not_found"`},
		{"/api/session", http.StatusOK, `"auth_mode":"caller"`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			assertions := assert.New(t)
			srv, _ := newAgentTokenTestServer(t)
			srv.peopleCodexLogins = newPeopleCodexLogins(completedCodexLoginClient{}, time.Now)
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
			for _, allowed := range []bool{true, false} {
				handler := srv.Handler(func(w http.ResponseWriter, _ *http.Request, _ Operation) bool {
					if !allowed {
						w.WriteHeader(http.StatusForbidden)
					}
					return allowed
				})
				// Even an invalid daemon credential must not override caller policy.
				r := httptest.NewRequest(http.MethodGet, "https://archive.example"+tc.path, nil)
				r.Header.Set("X-Msgvault-Agent-Token", "invalid-daemon-token")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if allowed {
					assertions.Equal(tc.status, w.Code, w.Body.String())
					assertions.Contains(w.Body.String(), tc.body)
				} else {
					assertions.Equal(http.StatusForbidden, w.Code)
					assertions.Empty(w.Body.String())
				}
			}
		})
	}
}

func TestLibraryHandlerOwnsDaemonControls(t *testing.T) {
	require := require.New(t)
	srv, _ := newAgentTokenTestServer(t)
	srv.operationGate = NewSerialOperationGate()
	stopped := make(chan struct{})
	srv.shutdownFunc = func() { close(stopped) }
	t.Cleanup(func() { require.NoError(srv.Shutdown(context.Background())) })
	handler := srv.Handler(func(http.ResponseWriter, *http.Request, Operation) bool { return true })
	begin := httptest.NewRecorder()
	handler.ServeHTTP(begin, httptest.NewRequest(http.MethodPost, backupFreezeBeginPath, nil))
	require.Equal(http.StatusOK, begin.Code, begin.Body.String())
	var frozen backupFreezeBeginResponse
	require.NoError(json.Unmarshal(begin.Body.Bytes(), &frozen))
	require.NotEmpty(frozen.Token)
	body, err := json.Marshal(backupFreezeEndRequest(frozen))
	require.NoError(err)
	end := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, backupFreezeEndPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(end, request)
	require.Equal(http.StatusOK, end.Code, end.Body.String())
	shutdown := httptest.NewRecorder()
	handler.ServeHTTP(shutdown, httptest.NewRequest(http.MethodPost, DaemonShutdownPath, nil))
	require.Equal(http.StatusAccepted, shutdown.Code, shutdown.Body.String())
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		require.FailNow("caller-authorized shutdown did not reach the supplied callback")
	}
}
