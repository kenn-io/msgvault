package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/verification"
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
	handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/cli/rebuild-fts", nil))

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
	handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/cli/rebuild-fts", nil))

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
				// Cleanup runs after t.Context() is canceled.
				t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
				handler := srv.Handler(func(http.ResponseWriter, *http.Request, Operation) bool { return true })
				r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://archive.example/api/v1/cli/stats", nil)
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
			// Cleanup runs after t.Context() is canceled.
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
			for _, allowed := range []bool{true, false} {
				handler := srv.Handler(func(w http.ResponseWriter, _ *http.Request, _ Operation) bool {
					if !allowed {
						w.WriteHeader(http.StatusForbidden)
					}
					return allowed
				})
				// Even an invalid daemon credential must not override caller policy.
				r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://archive.example"+tc.path, nil)
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
	// Cleanup runs after t.Context() is canceled.
	t.Cleanup(func() { require.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	handler := srv.Handler(func(http.ResponseWriter, *http.Request, Operation) bool { return true })
	begin := httptest.NewRecorder()
	handler.ServeHTTP(begin, httptest.NewRequestWithContext(t.Context(), http.MethodPost, backupFreezeBeginPath, nil))
	require.Equal(http.StatusOK, begin.Code, begin.Body.String())
	var frozen backupFreezeBeginResponse
	require.NoError(json.Unmarshal(begin.Body.Bytes(), &frozen))
	require.NotEmpty(frozen.Token)
	body, err := json.Marshal(backupFreezeEndRequest(frozen))
	require.NoError(err)
	end := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, backupFreezeEndPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(end, request)
	require.Equal(http.StatusOK, end.Code, end.Body.String())
	shutdown := httptest.NewRecorder()
	handler.ServeHTTP(shutdown, httptest.NewRequestWithContext(t.Context(), http.MethodPost, DaemonShutdownPath, nil))
	require.Equal(http.StatusAccepted, shutdown.Code, shutdown.Body.String())
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		require.FailNow("caller-authorized shutdown did not reach the supplied callback")
	}
}

func TestLibraryHandlerVerificationPolicy(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, _, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "reader@example.test", "verification", "Your verification code is 482913", "Your verification code is 482913")
	require.NoError(t, err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })

	for _, tc := range []struct {
		name   string
		path   string
		marked bool
		scoped bool
	}{
		{name: "marked health", path: "/api/v1/health", marked: true},
		{name: "marked stats", path: "/api/v1/stats", marked: true},
		{name: "protected messages", path: "/api/v1/messages", marked: true},
		{name: "protected source scoped messages", path: "/api/v1/messages", marked: true, scoped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			handler := srv.Handler(nil)
			ctx := t.Context()
			if tc.scoped {
				// Leave one slot available to catch nested snapshots.
				srv.readSnapshotSlots <- struct{}{}
				t.Cleanup(func() { <-srv.readSnapshotSlots })
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
				t.Cleanup(cancel)
				ctx = sourcescope.WithSourceIDs(ctx, []int64{source.ID})
			}
			r := httptest.NewRequestWithContext(ctx, http.MethodGet, tc.path, nil)
			if tc.marked {
				r.Header.Set(apiprotocol.VerificationHeader, apiprotocol.VerificationWithhold)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			require.Equal(http.StatusOK, w.Code, w.Body.String())
			assert.Empty(w.Header().Get(apiprotocol.VerificationHeader))
			if tc.path == "/api/v1/messages" {
				assert.Contains(w.Body.String(), verification.Masked)
				assert.NotContains(w.Body.String(), "482913")
				if tc.scoped {
					assert.Len(srv.readSnapshotSlots, 1)
				} else {
					assert.Empty(srv.readSnapshotSlots)
				}
			}
		})
	}
}

func TestLibraryHandlerRejectsInvalidVerificationHeader(t *testing.T) {
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: testutil.NewTestStore(t), Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	for _, handler := range []http.Handler{srv.Handler(nil), srv.Router()} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/messages", nil)
		req.Header.Set(apiprotocol.VerificationHeader, "withheld")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), `"bad_request"`)
	}
}

func TestLibraryHandlerVerificationRefusesUnclassifiedReads(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, id, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "reader@example.test", "code", "Your verification code is 482913", "Your verification code is 482913")
	require.NoError(t, err)
	view, err := st.CreateSavedView(t.Context(), store.SavedViewInput{Name: "all", CanonicalState: []byte(`{}`), SchemaVersion: 1})
	require.NoError(t, err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, SavedViewStore: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, fmt.Sprintf("/api/v1/saved-views/%d/run", view.ID)},
		{http.MethodGet, fmt.Sprintf("/api/v1/conversations/%d", id)},
		{http.MethodGet, "/api/v1/loops"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			for _, handler := range []http.Handler{srv.Handler(nil), srv.Router()} {
				request := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, strings.NewReader(`{}`))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(apiprotocol.VerificationHeader, apiprotocol.VerificationWithhold)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, request)
				assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
				assert.Contains(t, w.Body.String(), `"verification_masked"`)
				assert.Contains(t, w.Body.String(), "disable verification withholding")
			}
		})
	}
}
