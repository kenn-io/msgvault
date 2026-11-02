package cmd

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
)

// Shared fixtures also support focused production-file tests on busy hosts.
type daemonCLIRunTestRequest struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
	Cwd  string            `json:"cwd"`
}

func testConfigValue() *config.Config { return config.NewDefaultConfig() }

func testDiscardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testInvocationWithConfig(cfg *config.Config) *invocation {
	state := newInvocation()
	state.cfg = cfg
	state.logger = testDiscardLogger()
	return state
}

func testLoggerValue() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testInvocationContext(ctx context.Context, cfg *config.Config, options invocationOptions) context.Context {
	state := newInvocation()
	state.cfg = cfg
	state.options = options
	return withInvocation(ctx, state)
}

func newDaemonCLIRunnerTestServer(
	t *testing.T,
	checkRequest func(daemonCLIRunTestRequest),
	events ...string,
) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	requests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeDaemonTestHealth(t, w)
	})
	mux.HandleFunc("/api/v1/cli/run", daemonCLIRunTestHandler(t, requests, checkRequest, events...))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, requests
}

func writeDaemonTestHealth(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
		"status": "ok", "api_schema_version": api.APISchemaVersion,
	}))
}

func daemonCLIRunTestHandler(
	t *testing.T,
	requests *atomic.Int32,
	checkRequest func(daemonCLIRunTestRequest),
	events ...string,
) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method, "method")
		requests.Add(1)

		var req daemonCLIRunTestRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req), "decode request") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if checkRequest != nil {
			checkRequest(req)
		}

		w.Header().Set("Content-Type", "application/x-ndjson")
		if len(events) == 0 {
			_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
			return
		}
		for _, event := range events {
			_, _ = w.Write([]byte(event + "\n"))
		}
	}
}

func configureRemoteDaemonForTest(t *testing.T, url string, configs ...*config.Config) context.Context {
	t.Helper()
	cfg := &config.Config{}
	if len(configs) > 0 && configs[0] != nil {
		cfg = configs[0]
	}
	if cfg.HomeDir == "" {
		cfg.HomeDir = t.TempDir()
	}
	cfg.Remote.URL = url
	cfg.Remote.AllowInsecure = true
	return testInvocationContext(t.Context(), cfg, invocationOptions{})
}
