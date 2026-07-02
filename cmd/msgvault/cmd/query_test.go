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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
)

func TestQueryCommand_UsesLocalDaemonHTTPAndPreservesJSONOutput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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

func TestWriteQueryResult_PlainDecimalNumbers(t *testing.T) {
	result := &query.QueryResult{
		Columns: []string{"name", "message_count", "id", "ratio"},
		Rows: [][]any{
			{"UNREAD", float64(1662130), json.Number("9007199254740993"), 2.5},
			{nil, float64(0), json.Number("1722776"), float64(-1234567)},
		},
		RowCount: 2,
	}

	tests := []struct {
		format string
		want   []string
	}{
		{
			format: "table",
			want: []string{
				"1662130", "9007199254740993", "2.5",
				"1722776", "-1234567",
			},
		},
		{
			format: "csv",
			want: []string{
				"UNREAD,1662130,9007199254740993,2.5",
				",0,1722776,-1234567",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			var out bytes.Buffer
			require.NoError(writeQueryResult(&out, result, tt.format), "write %s", tt.format)
			got := out.String()
			for _, want := range tt.want {
				assert.Contains(got, want, "%s output", tt.format)
			}
			assert.NotContains(got, "e+06", "%s output must not use scientific notation", tt.format)
			assert.NotContains(got, "e+15", "%s output must not use scientific notation", tt.format)
		})
	}
}

func queryHTTPDaemon(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	assert := assert.New(t)

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
