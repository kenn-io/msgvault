package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

func TestCLIActionOwnersReportAfterDaemonSuccess(t *testing.T) {
	setup := require.New(t)
	terminal := cliActionTerminal
	cliActionTerminal = func(uintptr) bool { return true }
	t.Cleanup(func() { cliActionTerminal = terminal })
	path := filepath.Join(t.TempDir(), "muesli.db")
	db, err := sql.Open("sqlite3", path)
	setup.NoError(err)
	_, err = db.Exec(`CREATE TABLE meetings (id INTEGER PRIMARY KEY,title TEXT,start_time TEXT,created_at TEXT,raw_transcript TEXT)`)
	setup.NoError(err)
	setup.NoError(db.Close())
	disabled := false
	for action, run := range map[string]func(*cobra.Command) error{
		"search":       func(cmd *cobra.Command) error { return runHTTPSearch(cmd, "needle") },
		"show-message": func(cmd *cobra.Command) error { return showHTTPMessage(cmd, "1") },
		"documents": func(cmd *cobra.Command) error {
			return runSearchDocuments(cmd, store.DocumentSearchRequest{Query: "needle", SearchMode: " Hybrid "}, true, defaultDocumentsCommandDeps())
		},
		"visual": func(cmd *cobra.Command) error { return multimodalSearchCmd.RunE(cmd, []string{"needle"}) },
		"vector": func(cmd *cobra.Command) error { return runHybridSearch(cmd, "needle", cmd.Name(), false) },
		"hybrid": func(cmd *cobra.Command) error { return runHybridSearch(cmd, "needle", cmd.Name(), false) },
		"sync":   func(cmd *cobra.Command) error { return runSyncHTTP(cmd, daemonclient.CLISyncRequest{}) },
		"muesli-hook": func(cmd *cobra.Command) error {
			return muesliHookCmd.RunE(cmd, nil)
		},
		"sync-muesli": func(cmd *cobra.Command) error {
			invocationFromCommand(cmd).cfg.Muesli = []config.MuesliSource{{Identifier: "archive", AccountEmail: "user@example.com", DBPath: path, Contacts: &disabled}}
			return runMuesliClientSync(cmd, nil, 0)
		},
		"sync-slack": func(cmd *cobra.Command) error {
			return runDaemonCLICommandHTTPWithEnv(cmd, []string{"sync-slack"}, nil, false, false)
		},
	} {
		for _, fails := range []bool{false, true} {
			if (action == "muesli-hook" || action == "sync-muesli") && fails {
				continue
			}
			t.Run(fmt.Sprintf("%s/failure=%v", action, fails), func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				var events []map[string]any
				mux := http.NewServeMux()
				registerStatsProbeHandler(mux)
				mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
				mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/api/v1/telemetry/events" {
						var event map[string]any
						assert.NoError(json.UnmarshalRead(r.Body, &event))
						events = append(events, event)
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if action == "muesli-hook" {
						var request daemonclient.CLIRunRequest
						assert.NoError(json.UnmarshalRead(r.Body, &request))
						assert.Equal([]string{"sync-muesli", "archive", "--meeting-id", "42"}, request.Args)
					}
					if fails {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					switch action {
					case "show-message":
						_, _ = w.Write([]byte(`{"id":1,"message_type":"email"}`))
					case "sync", "sync-full", "sync-slack", "muesli-hook":
						w.Header().Set("Content-Type", "application/x-ndjson")
						_, _ = w.Write([]byte("{\"type\":\"complete\"}\n"))
					default:
						_, _ = w.Write([]byte(`{"results":[]}`))
					}
				}))
				server := httptest.NewServer(mux)
				t.Cleanup(server.Close)
				resetSearchFlags()
				t.Cleanup(resetSearchFlags)
				cmd := &cobra.Command{Use: action}
				cmd.SetContext(configureRemoteSyncTest(t, server.URL))
				if strings.HasPrefix(action, "sync") {
					cmd.SetOut(&bytes.Buffer{})
				} else {
					cmd.SetOut(os.Stdout)
				}
				var stderr bytes.Buffer
				cmd.SetErr(&stderr)
				if action == "muesli-hook" {
					state := invocationFromCommand(cmd)
					state.cfg.Remote.URL = ""
					state.cfg.Muesli = []config.MuesliSource{{Identifier: "archive"}}
					writeStatsHTTPDaemonRuntime(t, state.cfg.Data.DataDir, server)
					cmd.SetIn(strings.NewReader(`{"schemaVersion":1,"event":"meeting.completed","kind":"meeting","id":42,"completedAt":"2026-09-01T16:00:00Z"}`))
				}
				err := run(cmd)
				if fails {
					require.Error(err)
				} else {
					require.NoError(err, "telemetry failures leave the action successful")
				}
				kind := map[string]string{"search": "text", "show-message": "", "documents": "hybrid", "visual": "semantic", "vector": "semantic", "hybrid": "hybrid"}[action]
				if action == "muesli-hook" || fails {
					assert.Empty(events)
					return
				}
				properties, event := map[string]any{"surface": "cli"}, "message_opened"
				if strings.HasPrefix(action, "sync") {
					event = "sync_finished"
				}
				if kind != "" {
					properties["search_type"], event = kind, "search_run"
				}
				assert.Equal([]map[string]any{{"event": event, "properties": properties}}, events)
				if action == "search" {
					searchOffset = 1
					require.NoError(run(cmd))
					assert.Len(events, 1, "continuation pages do not count")
				}
			})
		}
	}
}

func TestCLIActionDeliveryDeadline(t *testing.T) {
	terminal := cliActionTerminal
	cliActionTerminal = func(uintptr) bool { return true }
	t.Cleanup(func() { cliActionTerminal = terminal })
	synctest.Test(t, func(t *testing.T) {
		client, err := daemonclient.New(daemonclient.Config{URL: "https://daemon.example.test", HTTPClient: &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
			assert.Equal(t, "/api/v1/telemetry/events", r.URL.Path)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}})
		require.NoError(t, err)
		cmd := &cobra.Command{Use: "search"}
		cmd.SetContext(t.Context())
		cmd.SetOut(os.Stdout)
		start := time.Now()
		reportCLIAction(cmd, client, "search_run", "search_type", "text")
		assert.Equal(t, 250*time.Millisecond, time.Since(start))
	})
}

func TestCLIActionSenderSuppressesNonInteractiveCalls(t *testing.T) {
	assert, cmd := assert.New(t), &cobra.Command{Use: "search"}
	assert.True(reportsCLIAction(cmd, true))
	assert.False(reportsCLIAction(cmd, false))
	cmd.SetContext(withInvocation(t.Context(), &invocation{options: invocationOptions{agentURLChanged: true}}))
	assert.False(reportsCLIAction(cmd, true))
	cmd.SetContext(t.Context())
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	assert.False(reportsCLIAction(cmd, true))
}
