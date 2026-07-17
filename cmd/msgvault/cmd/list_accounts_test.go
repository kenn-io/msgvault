package cmd

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
)

func TestListAccountsUsesLocalDaemonHTTPAndPreservesOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server, accountRequests := accountsHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedJSON := listAccountsJSON
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		listAccountsJSON = savedJSON
	}()

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	useLocal = true
	listAccountsJSON = false

	done := captureStdout(t)
	cmd := &cobra.Command{Use: "list-accounts", RunE: listAccountsCmd.RunE}

	err := cmd.Execute()
	out := done()
	require.NoError(err, "list-accounts")

	assert.Equal(1, int(accountRequests.Load()), "accounts endpoint calls")
	assert.Contains(out, "ID  ACCOUNT", "table header")
	assert.Contains(out, "alice@example.com", "account row")
	assert.Contains(out, "Alice", "display name")
	assert.Contains(out, "1,234", "message count formatting")
	assert.Contains(out, "2024-01-02 03:04", "last sync formatting")
}

func accountsHTTPDaemon(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	requests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/cli/accounts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"accounts": [{
				"id": 7,
				"email": "alice@example.com",
				"type": "gmail",
				"display_name": "Alice",
				"message_count": 1234,
				"last_sync": "2024-01-02T03:04:05Z"
			}]
		}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, requests
}
