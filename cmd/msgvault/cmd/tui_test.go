package cmd

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
)

func TestOpenTUIEngineUsesConfiguredRemoteHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var requests atomic.Int32
	srv := httptest.NewServer(tuiAccountsHandler(&requests, "remote@example.com"))
	t.Cleanup(srv.Close)
	withTUIConfig(t, lifecycleTestConfig(t.TempDir()))
	cfg.Remote.URL = srv.URL
	cfg.Remote.AllowInsecure = true

	backend, err := openTUIBackend(context.Background())
	require.NoError(
		err, "openTUIBackend")

	t.Cleanup(backend.cleanup)

	accounts, err := backend.engine.ListAccounts(context.Background())
	require.NoError(
		err, "ListAccounts")

	require.Len(accounts, 1, "accounts")
	assert.Equal(HTTPStoreConfiguredRemote, backend.info.Kind)
	assert.Equal(srv.URL, backend.info.URL)
	_, ok := backend.engine.(query.TextEngine)
	assert.True(ok, "TUI backend should expose daemon-backed text queries")
	assert.Equal("remote@example.com", accounts[0].Identifier)
	assert.Equal("gmail", accounts[0].SourceType)
	assert.Equal(int32(1), requests.Load())
}

func TestOpenTUIEngineLocalFlagUsesLocalDaemonHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	localCfg := lifecycleTestConfig(dataDir)
	localCfg.Remote.URL = "http://configured-daemonclient.example:8080"
	localCfg.Remote.AllowInsecure = true
	localCfg.Server.APIKey = "local-daemon-secret"
	withTUIConfig(t, localCfg)
	forceLocalTUI = true

	var requests atomic.Int32
	srv := httptest.NewServer(tuiAccountsHandler(&requests, "local@example.com"))
	t.Cleanup(srv.Close)
	host, portText, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(
		err, "split listener address")

	port, err := strconv.Atoi(portText)
	require.NoError(
		err, "parse listener port")

	_, err = daemonRuntimeStore(dataDir).Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(host, portText),
		Service: daemonService,
		Version: Version,
		Metadata: map[string]string{
			runtimeHost:            host,
			runtimePort:            strconv.Itoa(port),
			runtimeAPIVersion:      strconv.Itoa(daemonAPIVersion),
			runtimeAuthFingerprint: daemonAPIKeyFingerprint(localCfg.Server.APIKey),
		},
	})
	require.NoError(
		err, "write runtime")

	backend, err := openTUIBackend(context.Background())
	require.NoError(
		err, "openTUIBackend")

	t.Cleanup(backend.cleanup)

	accounts, err := backend.engine.ListAccounts(context.Background())
	require.NoError(
		err, "ListAccounts")

	require.Len(accounts, 1, "accounts")
	assert.Equal(HTTPStoreLocalDaemon, backend.info.Kind)
	assert.Equal(srv.URL, backend.info.URL)
	_, ok := backend.engine.(query.TextEngine)
	assert.True(ok, "TUI backend should expose daemon-backed text queries")
	assert.Equal("local@example.com", accounts[0].Identifier)
	assert.Equal("gmail", accounts[0].SourceType)
	assert.Equal(int32(1), requests.Load())
}

func withTUIConfig(t *testing.T, c *config.Config) {
	t.Helper()
	oldCfg := cfg
	oldUseLocal := useLocal
	oldForceLocalTUI := forceLocalTUI
	cfg = c
	useLocal = false
	forceLocalTUI = false
	t.Cleanup(func() {
		cfg = oldCfg
		useLocal = oldUseLocal
		forceLocalTUI = oldForceLocalTUI
	})
}

func tuiAccountsHandler(requests *atomic.Int32, email string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_messages":42}`))
	})
	mux.HandleFunc("/api/v1/cli/accounts", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accounts": []map[string]any{{
				"id":            1,
				"email":         email,
				"type":          "gmail",
				"display_name":  "Test Account",
				"message_count": 42,
			}},
		})
	})
	return mux
}

// TestAnalyticsCacheNotice verifies the pre-launch warning keys off the
// analytics mode the daemon itself reports on /health: only the live-SQL
// fallback mode warns, while deliberate live SQL (engine = "sql",
// PostgreSQL), cache-backed DuckDB, daemons predating the field, and
// health-endpoint failures all stay silent.
func TestAnalyticsCacheNotice(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		statusCode int
		wantNotice bool
	}{
		{name: "sql fallback warns", mode: api.AnalyticsModeSQLFallback, statusCode: http.StatusOK, wantNotice: true},
		{name: "duckdb is silent", mode: api.AnalyticsModeDuckDB, statusCode: http.StatusOK},
		{name: "deliberate sql is silent", mode: api.AnalyticsModeSQL, statusCode: http.StatusOK},
		{name: "postgres is silent", mode: api.AnalyticsModePostgres, statusCode: http.StatusOK},
		{name: "older daemon without field is silent", statusCode: http.StatusOK},
		{name: "health error is silent", statusCode: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
				if tt.statusCode != http.StatusOK {
					http.Error(w, "health unavailable", tt.statusCode)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				body := map[string]any{"status": "ok"}
				if tt.mode != "" {
					body["analytics_engine"] = tt.mode
				}
				_ = json.NewEncoder(w).Encode(body)
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			client, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true})
			require.NoError(t, err, "daemonclient.New")

			notice := analyticsCacheNotice(context.Background(), client)
			if tt.wantNotice {
				assert.Contains(t, notice, "msgvault build-cache")
			} else {
				assert.Empty(t, notice)
			}
		})
	}
}
