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

func TestAggregateListsUseDaemonHTTPAndPreserveOutput(t *testing.T) {
	savedLimit, savedAfter, savedBefore, savedJSON := aggLimit, aggAfter, aggBefore, aggJSON
	t.Cleanup(func() { aggLimit, aggAfter, aggBefore, aggJSON = savedLimit, savedAfter, savedBefore, savedJSON })
	for _, view := range []struct {
		command           *cobra.Command
		name, header, key string
	}{
		{listSendersCmd, "senders", "SENDER", "alice@example.com"}, {listDomainsCmd, "domains", "DOMAIN", "example.com"}, {listLabelsCmd, "labels", "LABEL", "INBOX"},
	} {
		for _, total := range []int{50, 51} {
			for _, asJSON := range []bool{false, true} {
				if asJSON && view.name != "senders" {
					continue
				}
				t.Run(fmt.Sprintf("%s/%d/json=%t", view.name, total, asJSON), func(t *testing.T) {
					assert, require := assert.New(t), require.New(t)
					dataDir := t.TempDir()
					server, requests := aggregateHTTPDaemon(t, view.name, total)
					writeStatsHTTPDaemonRuntime(t, dataDir, server)
					cfg := &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}, Remote: config.RemoteConfig{URL: "http://configured-daemonclient.invalid"}}
					cmd := &cobra.Command{Use: view.command.Use, RunE: view.command.RunE}
					cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{useLocal: true}))
					aggLimit, aggAfter, aggBefore, aggJSON = 50, "", "", asJSON
					var stderr bytes.Buffer
					cmd.SetErr(&stderr)
					done := captureStdout(t)
					err := cmd.Execute()
					out := done()
					require.NoError(err)
					assert.Equal(int32(1), requests.Load())
					if asJSON {
						var rows []map[string]any
						require.NoError(json.Unmarshal([]byte(out), &rows))
						assert.Len(rows, 50)
					} else {
						assert.Contains(out, view.header)
						assert.Contains(out, view.key)
						assert.Contains(out, "2.0K")
						assert.Contains(out, "Showing 50 results")
					}
					if total == 51 {
						assert.Equal("Hint: more "+view.name+" exist; raise -n\n", stderr.String())
					} else {
						assert.Empty(stderr.String())
					}
				})
			}
		}
	}
}

func aggregateHTTPDaemon(t *testing.T, viewType string, totalUnique int) (*httptest.Server, *atomic.Int32) {
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
		assert.Equal(t, "50", r.URL.Query().Get("limit"))
		rows := make([]map[string]any, 50)
		for i := range rows {
			key := fmt.Sprintf("sender-%d@example.com", i)
			if i == 0 {
				key = "alice@example.com"
			}
			switch viewType {
			case "domains":
				key = fmt.Sprintf("domain-%d.example.com", i)
				if i == 0 {
					key = "example.com"
				}
			case "labels":
				key = fmt.Sprintf("label-%d", i)
				if i == 0 {
					key = "INBOX"
				}
			}
			rows[i] = map[string]any{"key": key, "count": 3, "total_size": 2048, "attachment_size": 1024, "attachment_count": 1, "total_unique": totalUnique}
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"view_type": viewType, "rows": rows}))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, requests
}
