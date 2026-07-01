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

func TestListSendersUsesLocalDaemonHTTPAndPreservesOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	server, aggregateRequests := aggregateHTTPDaemon(t, "senders")
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedUseLocal := useLocal
	savedLimit := aggLimit
	savedAfter := aggAfter
	savedBefore := aggBefore
	savedJSON := aggJSON
	defer func() {
		cfg = savedCfg
		useLocal = savedUseLocal
		aggLimit = savedLimit
		aggAfter = savedAfter
		aggBefore = savedBefore
		aggJSON = savedJSON
	}()

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote:  config.RemoteConfig{URL: "http://configured-daemonclient.invalid"},
	}
	useLocal = true
	aggLimit = 50
	aggAfter = ""
	aggBefore = ""
	aggJSON = false

	done := captureStdout(t)
	cmd := &cobra.Command{Use: "list-senders", RunE: listSendersCmd.RunE}

	err := cmd.Execute()
	out := done()
	require.NoError(err, "list-senders")

	assert.Equal(1, int(aggregateRequests.Load()), "aggregate endpoint calls")
	assert.Contains(out, "SENDER", "table header")
	assert.Contains(out, "alice@example.com", "sender row")
	assert.Contains(out, "2.0K", "size formatting")
}

func aggregateHTTPDaemon(t *testing.T, viewType string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	requests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/aggregates", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("view_type") != viewType {
			http.Error(w, "wrong view type", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"view_type": "senders",
			"rows": [{
				"key": "alice@example.com",
				"count": 3,
				"total_size": 2048,
				"attachment_size": 1024,
				"attachment_count": 1,
				"total_unique": 1
			}]
		}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, requests
}
