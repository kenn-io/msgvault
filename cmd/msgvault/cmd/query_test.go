package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
)

func TestQueryCommand_UsesLocalDaemonHTTPAndPreservesJSONOutput(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	dataDir := t.TempDir()
	server, queryRequests := queryHTTPDaemon(t)
	writeStatsHTTPDaemonRuntime(t, dataDir, server)

	savedCfg := cfg
	savedLogger := logger
	savedUseLocal := useLocal
	savedQueryFormat := queryFormat
	t.Cleanup(func() {
		cfg = savedCfg
		logger = savedLogger
		useLocal = savedUseLocal
		queryFormat = savedQueryFormat
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	logger = slog.New(slog.DiscardHandler)
	useLocal = true
	queryFormat = outputFormatJSON

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{
		Use:  "query [sql]",
		Args: queryCmd.Args,
		RunE: queryCmd.RunE,
	}
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"SELECT subject FROM messages"})

	err := cmd.Execute()
	require.NoError(err, "query command")

	assert.Equal(1, int(queryRequests.Load()), "query endpoint calls")
	assert.Empty(stderr.String(), "stderr")
	assert.JSONEq(`{
		"columns": ["subject"],
		"rows": [["Hello"]],
		"row_count": 1
	}`, stdout.String(), "stdout JSON")
}

func queryHTTPDaemon(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	assert := assertpkg.New(t)

	queryRequests := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			SQL string `json:"sql"`
		}
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(err, "read request body") {
			return
		}
		if !assert.NoError(json.Unmarshal(body, &req), "decode query request") {
			return
		}
		assert.Equal("SELECT subject FROM messages", req.SQL, "sql")

		queryRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"columns": ["subject"],
			"rows": [["Hello"]],
			"row_count": 1
		}`))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, queryRequests
}
