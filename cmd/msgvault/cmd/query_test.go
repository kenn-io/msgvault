package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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
	savedQueryFresh := queryFresh
	t.Cleanup(func() {
		cfg = savedCfg
		logger = savedLogger
		useLocal = savedUseLocal
		queryFormat = savedQueryFormat
		queryFresh = savedQueryFresh
	})

	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	logger = slog.New(slog.DiscardHandler)
	useLocal = true
	queryFormat = outputFormatJSON
	queryFresh = false

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

func TestQueryCommandWaitsForAcceptedBuild(t *testing.T) {
	for _, test := range []struct {
		name    string
		fresh   bool
		outcome string
		wantErr string
	}{
		{name: "plain query", outcome: "published"},
		{name: "fresh query", fresh: true, outcome: "published"},
		{name: "failed build", fresh: true, outcome: "failed", wantErr: "synthetic build failure"},
		{name: "missing job", outcome: "missing", wantErr: "Analytics cache build not found"},
		{name: "canceled request", fresh: true, outcome: "canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			dataDir := t.TempDir()
			var queries, polls atomic.Int32
			mux := http.NewServeMux()
			mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
				Service: daemonService, Version: Version,
			}))
			mux.HandleFunc("POST /api/v1/query", func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					SQL   string `json:"sql"`
					Fresh bool   `json:"fresh"`
				}
				if !assert.NoError(json.NewDecoder(r.Body).Decode(&req)) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal("SELECT id FROM messages", req.SQL)
				w.Header().Set("Content-Type", "application/json")
				if queries.Add(1) == 1 {
					assert.Equal(test.fresh, req.Fresh)
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"status":"queued","job_id":"synthetic-job"}`))
					return
				}
				assert.False(req.Fresh, "retry must read the completed publication")
				assert.GreaterOrEqual(polls.Load(), int32(3), "query must wait for publication")
				_, _ = w.Write([]byte(`{"columns":["id"],"rows":[[9007199254740993]],"row_count":1}`))
			})
			mux.HandleFunc("GET /api/v1/cache-builds/synthetic-job", func(w http.ResponseWriter, r *http.Request) {
				poll := polls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				switch test.outcome {
				case "failed":
					_, _ = w.Write([]byte(`{"job_id":"synthetic-job","status":"failed","error":"synthetic build failure"}`))
				case "missing":
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"error":"cache_build_not_found","message":"Analytics cache build not found"}`))
				case "canceled":
					cancel()
					<-r.Context().Done()
				default:
					status := "published"
					switch poll {
					case 1:
						status = "queued"
					case 2:
						status = "running"
					}
					assert.NoError(json.NewEncoder(w).Encode(map[string]string{
						"job_id": "synthetic-job", "status": status,
					}))
				}
			})
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			writeStatsHTTPDaemonRuntime(t, dataDir, server)
			savedCfg, savedLogger, savedUseLocal := cfg, logger, useLocal
			savedFormat, savedFresh := queryFormat, queryFresh
			t.Cleanup(func() {
				cfg, logger, useLocal = savedCfg, savedLogger, savedUseLocal
				queryFormat, queryFresh = savedFormat, savedFresh
			})
			cfg = &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}
			logger = slog.New(slog.DiscardHandler)
			useLocal = true
			queryFormat, queryFresh = outputFormatJSON, test.fresh
			var stdout, stderr bytes.Buffer
			cmd := &cobra.Command{
				Use: "query", Args: queryCmd.Args, RunE: queryCmd.RunE,
				SilenceErrors: true, SilenceUsage: true,
			}
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"SELECT id FROM messages"})
			err := cmd.ExecuteContext(ctx)
			if test.outcome == "published" {
				require.NoError(err)
				assert.JSONEq(`{"columns":["id"],"rows":[[9007199254740993]],"row_count":1}`, stdout.String())
				assert.Equal(int32(2), queries.Load())
			} else {
				if test.outcome == "canceled" {
					require.ErrorIs(err, context.Canceled)
				} else {
					require.ErrorContains(err, test.wantErr)
				}
				assert.Empty(stdout.String(), "failed queries must not emit a result")
				assert.Equal(int32(1), queries.Load())
			}
			assert.Contains(stderr.String(), "synthetic-job")
		})
	}
}

func TestWriteQueryResult_PlainDecimalNumbers(t *testing.T) {
	result := &query.QueryResult{
		Columns: []string{"name", "message_count", "id", "ratio"},
		Rows: [][]any{
			{"UNREAD", float64(1662130), jsontext.Value("9007199254740993"), 2.5},
			{nil, float64(0), jsontext.Value("1722776"), float64(-1234567)},
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

func TestWriteQueryResultIncludesCacheFreshness(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	result := &query.QueryResult{
		Columns: []string{"count"}, Rows: [][]any{{int64(1)}}, RowCount: 1,
		Cache: &query.CacheFreshness{
			Generation: "synthetic-generation", PublishedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC),
			StaleReason: "1 new message", PendingAdditions: 1,
		},
	}
	var out bytes.Buffer
	require.NoError(writeQueryResult(&out, result, "json"))
	assert.Contains(out.String(), `"generation": "synthetic-generation"`)
	assert.Contains(out.String(), `"stale_reason": "1 new message"`)
	assert.Contains(out.String(), `"pending_additions": 1`)
}

func TestWriteQueryResult_FormatCaseInsensitive(t *testing.T) {
	result := &query.QueryResult{
		Columns:  []string{"n"},
		Rows:     [][]any{{jsontext.Value("1")}},
		RowCount: 1,
	}
	for _, format := range []string{"JSON", "Json", "CSV", "Table", " table ", "TABLE"} {
		t.Run(format, func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, writeQueryResult(&out, result, format),
				"writeQueryResult(%q)", format)
			assert.NotEmpty(t, out.String(), "output for %q", format)
		})
	}

	var out bytes.Buffer
	err := writeQueryResult(&out, result, "xml")
	require.Error(t, err, "unknown format")
	assert.Contains(t, err.Error(), "unknown format", "error text")
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
