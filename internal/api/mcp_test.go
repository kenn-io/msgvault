package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
)

func TestNativeMCPAdmissionAndCatalogs(t *testing.T) {
	credentials := []struct {
		name                   string
		headers                http.Header
		owner, session, remote bool
	}{
		{name: "bearer", headers: http.Header{"Authorization": {"Bearer owner"}}, owner: true},
		{name: "lowercase", headers: http.Header{"Authorization": {"bearer owner"}}, owner: true},
		{name: "uppercase", headers: http.Header{"Authorization": {"BEARER owner"}}, owner: true},
		{name: "missing"},
		{name: "wrong", headers: http.Header{"Authorization": {"Bearer wrong"}}},
		{name: "raw", headers: http.Header{"Authorization": {"owner"}}},
		{name: "alias", headers: http.Header{"X-Api-Key": {"owner"}}},
		{name: "duplicate", headers: http.Header{"Authorization": {"Bearer owner", "Bearer owner"}}},
		{name: "mixed", headers: http.Header{"Authorization": {"Bearer owner"}, apiprotocol.AgentTokenHeader: {"agent"}}},
		{name: "agent", headers: http.Header{apiprotocol.AgentTokenHeader: {"agent"}}},
		{name: "session", session: true},
		{name: "remote", headers: http.Header{"Authorization": {"Bearer remote"}}, remote: true},
	}
	for _, phase := range []string{"starting", "read", "writes", "keyless"} {
		for _, credential := range credentials {
			t.Run(phase+"/"+credential.name, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner", RemoteClients: []config.RemoteClientConfig{{APIKey: "remote"}}}}
				if phase == "keyless" {
					cfg.Server.APIKey = ""
				}
				server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
				defer func() { require.NoError(server.Shutdown(context.Background())) }()
				if phase != "starting" && phase != "keyless" {
					read := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
					var write http.Handler
					if phase == "writes" {
						write = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
					}
					require.NoError(server.InstallMCPHandlers(read, write, nil))
				}
				for _, path := range []string{"/mcp", "/mcp/writes"} {
					request := httptest.NewRequest(http.MethodGet, path, nil)
					if credential.headers != nil {
						request.Header = credential.headers.Clone()
					}
					if credential.session {
						token, _, err := server.sessions.create()
						require.NoError(err)
						request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
					}
					want := http.StatusUnauthorized
					if phase != "keyless" {
						if credential.remote {
							want = http.StatusForbidden
						}
						if credential.owner {
							want = http.StatusServiceUnavailable
							if phase != "starting" {
								want = http.StatusNoContent
								if path == "/mcp/writes" {
									want = http.StatusNotFound
									if phase == "writes" {
										want = http.StatusAccepted
									}
								}
							}
						}
					}
					response := httptest.NewRecorder()
					server.Router().ServeHTTP(response, request)
					assert.Equal(want, response.Code, response.Body.String())
					assert.Equal("no-store", response.Header().Get("Cache-Control"))
				}
			})
		}
	}
}

func TestNativeMCPEnvelopeDeadlineAndGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		gate := NewSerialOperationGate()
		s := NewServerWithOptions(ServerOptions{Logger: slog.New(slog.DiscardHandler), Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, RequestTimeout: time.Second, OperationGate: gate})
		t.Cleanup(func() { require.NoError(s.Shutdown(context.Background())) })
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, bounded := r.Context().Deadline()
			assert.False(bounded, "native MCP must preserve the nested CLI execution policy")
			nested := httptest.NewRequest(http.MethodPost, "/api/v1/saved-views", nil)
			gated, _, err := operationGateRequest(nested, requestAuthentication{Mode: AuthModeAPIKey})
			if !assert.NoError(err) {
				http.Error(w, "gate classification failed", http.StatusInternalServerError)
				return
			}
			assert.True(gated)
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			release, ok := gate.BeginWorkContext(ctx)
			if !assert.True(ok, "MCP envelope must not hold the gate") {
				http.Error(w, "gate unavailable", http.StatusInternalServerError)
				return
			}
			defer release()
			timer := time.NewTimer(31 * time.Minute)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				assert.Fail("native MCP execution expired before its read completed")
				http.Error(w, "execution canceled", http.StatusInternalServerError)
				return
			case <-timer.C:
			}
			w.WriteHeader(http.StatusNoContent)
		})
		require.NoError(s.InstallMCPHandlers(handler, handler, nil))
		for _, path := range []string{"/mcp", "/mcp/writes"} {
			r := httptest.NewRequest(http.MethodPost, path, nil)
			r.Header.Set("Authorization", "Bearer owner")
			w := &deadlineClearingRecorder{ResponseRecorder: httptest.NewRecorder()}
			started := time.Now()
			s.Router().ServeHTTP(w, r)
			assert.Equal(http.StatusNoContent, w.Code)
			require.Len(w.readDeadlines, 1)
			assert.Equal(started.Add(DaemonLongRequestTimeout), w.readDeadlines[0])
			require.Len(w.writeDeadlines, 1)
			assert.True(w.writeDeadlines[0].IsZero(), "native MCP must clear the write ceiling")
		}
	})
}

func TestNativeMCPShutdownDrainsBeforeCleanup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s := NewServerWithOptions(ServerOptions{Logger: slog.New(slog.DiscardHandler), Config: &config.Config{Server: config.ServerConfig{BindAddr: "127.0.0.1", APIKey: "owner"}}})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(err)
	serving := make(chan error, 1)
	go func() { serving <- s.StartOnListener(listener) }()
	require.NoError(s.WaitStarted(context.Background()))
	entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	cleanups := 0
	require.NoError(s.InstallMCPHandlers(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}), nil, func() { cleanups++; close(closed) }))
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		r, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/mcp", nil)
		if !assert.NoError(err) {
			return
		}
		r.Header.Set("Authorization", "Bearer owner")
		resp, err := http.DefaultClient.Do(r)
		if assert.NoError(err) {
			assert.Equal(http.StatusNoContent, resp.StatusCode)
			assert.NoError(resp.Body.Close())
		}
	}()
	<-entered
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	require.ErrorIs(<-serving, http.ErrServerClosed)
	select {
	case <-closed:
		require.FailNow("closed resources while request active")
	default:
	}
	close(release)
	<-requestDone
	require.NoError(<-shutdownDone)
	<-closed
	require.NoError(s.Shutdown(context.Background()))
	assert.Equal(1, cleanups)
}
