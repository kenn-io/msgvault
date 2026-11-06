package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestListSendersUsesTotalUniqueForMoreResultsHint(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	savedLimit, savedAfter, savedBefore, savedJSON := aggLimit, aggAfter, aggBefore, aggJSON
	t.Cleanup(func() { aggLimit, aggAfter, aggBefore, aggJSON = savedLimit, savedAfter, savedBefore, savedJSON })
	aggLimit, aggAfter, aggBefore, aggJSON = 50, "", "", true
	requests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
	mux.HandleFunc("/api/v1/aggregates", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal("senders", r.URL.Query().Get("view_type"))
		assert.Equal("50", r.URL.Query().Get("limit"), "request only the displayed rows")
		rows := make([]map[string]any, 50)
		for i := range rows {
			rows[i] = map[string]any{"key": fmt.Sprintf("sender-%d@example.com", i), "count": 1, "total_unique": 51}
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.NewEncoder(w).Encode(map[string]any{"view_type": "senders", "rows": rows}))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	dataDir := t.TempDir()
	writeStatsHTTPDaemonRuntime(t, dataDir, server)
	cfg := &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}
	cmd := &cobra.Command{Use: "list-senders", RunE: listSendersCmd.RunE}
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{useLocal: true}))
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	done := captureStdout(t)
	err := cmd.Execute()
	out := done()
	require.NoError(err)
	assert.Equal(int32(1), requests.Load())
	var rows []map[string]any
	require.NoError(json.Unmarshal([]byte(out), &rows))
	assert.Len(rows, 50)
	assert.Equal("Hint: more senders exist; raise -n\n", stderr.String())
}

func TestListSendersUsesLocalDaemonHTTPAndPreservesOutput(t *testing.T) {
	cfg := testConfigValue()
	useLocal := false

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
	testCtx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	_ = testCtx
	useLocal = true
	invocationFromContext(testCtx).options.useLocal = true
	aggLimit = 50
	aggAfter = ""
	aggBefore = ""
	aggJSON = false

	done := captureStdout(t)
	cmd := &cobra.Command{Use: "list-senders", RunE: listSendersCmd.RunE}
	cmd.SetContext(testCtx)

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
