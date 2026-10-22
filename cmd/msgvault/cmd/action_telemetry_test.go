package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/telemetry/posthog"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telemetry"
)

func clearTelemetryVetoEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"CI", "DO_NOT_TRACK"} {
		t.Setenv(name, os.Getenv(name))
		require.NoError(t, os.Unsetenv(name), "clear %s", name)
	}
}

func TestCLIActionOwnersReportSuccessAndSuppressHooksAndFailures(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDECODE", "")
	t.Setenv(telemetry.EnabledEnv, "1")
	t.Setenv(posthog.GenericEnabledEnv, "")
	clearTelemetryVetoEnvironment(t)
	terminal := cliActionTerminal
	cliActionTerminal = func(uintptr) bool { return true }
	t.Cleanup(func() { cliActionTerminal = terminal })
	path := newMuesliMeetingsFixture(t, false)
	disabled := false
	for action, run := range map[string]func(*cobra.Command) error{
		"search": func(cmd *cobra.Command) error { return runHTTPSearch(cmd, "needle") },
		"media": func(cmd *cobra.Command) error {
			search, _, err := rootCmd.Find([]string{"media", "search"})
			require.NoError(t, err)
			return search.RunE(cmd, []string{"needle"})
		},
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
		"sync-omi": func(cmd *cobra.Command) error {
			return runDaemonCLICommandHTTPWithEnv(cmd, []string{"sync-omi"}, nil, false, false)
		},
		"sync-slack": func(cmd *cobra.Command) error {
			return runDaemonCLICommandHTTPWithEnv(cmd, []string{"sync-slack"}, nil, false, false)
		},
	} {
		for _, outcome := range []struct {
			name                                             string
			indexState                                       string
			fails, paused, outputFails, json, body, nonempty bool
		}{
			{name: "success"},
			{name: "nonempty", nonempty: true},
			{name: "failure", fails: true},
			{name: "building", indexState: "building"},
			{name: "paused", paused: true},
			{name: "output failure", outputFails: true},
			{name: "JSON output failure", outputFails: true, json: true},
			{name: "body output failure", outputFails: true, body: true},
		} {
			fails, outputFails := outcome.fails, outcome.outputFails
			if outcome.nonempty && action != "search" && action != "media" && action != "documents" && action != "visual" && action != "vector" && action != "hybrid" {
				continue
			}
			if outcome.indexState != "" && action != "search" {
				continue
			}
			if outcome.paused && action != "sync-omi" || outputFails && action != "show-message" {
				continue
			}
			if (action == "muesli-hook" || action == "sync-muesli") && fails {
				continue
			}
			t.Run(fmt.Sprintf("%s/%s", action, outcome.name), func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				var eventsMu sync.Mutex
				var events []map[string]any
				mux := http.NewServeMux()
				registerStatsProbeHandler(mux)
				mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
				mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/api/v1/telemetry/events" {
						var event map[string]any
						assert.NoError(json.UnmarshalRead(r.Body, &event))
						eventsMu.Lock()
						events = append(events, event)
						eventsMu.Unlock()
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if action == "muesli-hook" {
						var request daemonclient.CLIRunRequest
						assert.NoError(json.UnmarshalRead(r.Body, &request))
					}
					if fails {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					results := "[]"
					if outcome.nonempty {
						results = `[{"id":1,"subject":"Synthetic result"}]`
					}
					switch action {
					case "search":
						_, _ = fmt.Fprintf(w, `{"results":%s,"index_state":%q}`, results, outcome.indexState)
					case "show-message":
						_, _ = w.Write([]byte(`{"id":1,"message_type":"email"}`))
					case "documents":
						_, _ = fmt.Fprintf(w, `{"results":%s,"effective_mode":"hybrid"}`, results)
					case "sync-omi":
						w.Header().Set("Content-Type", "application/x-ndjson")
						_, _ = fmt.Fprintln(w, `{"type":"stdout","data":"Omi sync output"}`)
						var waitErr error
						if outcome.paused {
							waitErr = helperProcessCommand(r.Context(), "paused").Run()
						}
						assert.NoError(classifyDaemonCLIWaitErr(waitErr, []string{"sync-omi"}, func(stream, data string) error {
							return json.MarshalWrite(w, map[string]string{"type": stream, "data": data})
						}))
						_, _ = fmt.Fprintln(w, "\n"+`{"type":"complete"}`)
					case "sync", "sync-slack", "muesli-hook":
						w.Header().Set("Content-Type", "application/x-ndjson")
						_, _ = w.Write([]byte("{\"type\":\"complete\"}\n"))
					default:
						_, _ = fmt.Fprintf(w, `{"results":%s}`, results)
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
				if outputFails {
					cmd.SetOut(failingSearchWriter{})
					oldJSON, oldBody := showMessageJSON, showMessageBodyOnly
					showMessageJSON, showMessageBodyOnly = outcome.json, outcome.body
					t.Cleanup(func() { showMessageJSON, showMessageBodyOnly = oldJSON, oldBody })
				}
				err := run(cmd)
				invocationFromCommand(cmd).sends.Wait(time.Second)
				if fails || outputFails {
					require.Error(err)
					if outputFails {
						require.ErrorIs(err, io.ErrClosedPipe)
					}
				} else {
					require.NoError(err, "telemetry failures leave the action successful")
				}
				kind := map[string]string{"search": "text", "media": "text", "show-message": "", "documents": "hybrid", "visual": "semantic", "vector": "semantic", "hybrid": "hybrid"}[action]
				if action == "sync-omi" && !fails {
					output, ok := cmd.OutOrStdout().(*bytes.Buffer)
					require.True(ok)
					assert.Equal("Omi sync output", output.String())
				}
				if action == "muesli-hook" || fails || outputFails || outcome.paused {
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
				want := []map[string]any{{"event": event, "properties": properties}}
				if kind != "" && outcome.indexState == "" && !outcome.nonempty {
					want = append([]map[string]any{{"event": "search_empty", "properties": properties}}, want...)
				}
				assert.ElementsMatch(want, events)
				if action == "search" {
					searchOffset = 1
					require.NoError(run(cmd))
					invocationFromCommand(cmd).sends.Wait(time.Second)
					assert.Len(events, len(want), "continuation pages do not count")
				}
			})
		}
	}
}

func TestCLIActionDeliveryDeadline(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDECODE", "")
	t.Setenv(telemetry.EnabledEnv, "1")
	t.Setenv(posthog.GenericEnabledEnv, "")
	clearTelemetryVetoEnvironment(t)
	terminal := cliActionTerminal
	cliActionTerminal = func(uintptr) bool { return true }
	t.Cleanup(func() { cliActionTerminal = terminal })
	for _, tc := range []struct {
		name        string
		delay, want time.Duration
	}{
		{name: "stalled", delay: time.Second, want: 50 * time.Millisecond},
		{name: "healthy", delay: 5 * time.Millisecond, want: 5 * time.Millisecond},
		{name: "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				release := make(chan struct{})
				var requests atomic.Int32
				mux := http.NewServeMux()
				registerStatsProbeHandler(mux)
				mux.HandleFunc("/api/v1/cli/search", func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(`{"results":[{"id":1,"subject":"Synthetic result"}]}`))
				})
				mux.HandleFunc("/api/v1/telemetry/events", func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					if tc.name == "stalled" {
						<-release
					} else {
						time.Sleep(tc.delay)
					}
					w.WriteHeader(http.StatusAccepted)
				})
				server := httptest.NewTestServer(t, mux)
				transport := http.DefaultTransport
				http.DefaultTransport = server.Client().Transport
				t.Cleanup(func() { http.DefaultTransport = transport })
				resetSearchFlags()
				t.Cleanup(resetSearchFlags)
				var exitStart time.Time
				root := &cobra.Command{Use: "search", RunE: func(cmd *cobra.Command, _ []string) error {
					inv := invocationFromCommand(cmd)
					inv.cfg = &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}}
					err := runHTTPSearch(cmd, "needle")
					if tc.name == "completed" {
						synctest.Wait()
					}
					exitStart = time.Now()
					return err
				}}
				root.SetArgs([]string{})
				root.SetOut(io.Discard)
				root.SetErr(io.Discard)
				root.SetIn(os.Stdin)
				require.NoError(executeRootContext(context.Background(), root))
				elapsed := time.Since(exitStart)
				synctest.Wait()
				assert.Equal(tc.want, elapsed)
				assert.Equal(int32(1), requests.Load())
				close(release)
				invocationFromCommand(root).sends.Wait(time.Second)
			})
		})
	}
}

func TestCLIActionSenderSuppressesNonInteractiveCalls(t *testing.T) {
	terminal := cliActionTerminal
	t.Cleanup(func() { cliActionTerminal = terminal })
	for _, tc := range []struct {
		name, stream, env string
		agent             bool
		want              bool
	}{
		{name: "none"},
		{name: "stdin", stream: "stdin", want: true},
		{name: "stdout", stream: "stdout", want: true},
		{name: "stderr", stream: "stderr", want: true},
		{name: "Codex", stream: "stdout", env: "CODEX_THREAD_ID"},
		{name: "Claude Code", stream: "stdout", env: "CLAUDECODE"},
		{name: "daemon subprocess", stream: "stdout", env: daemonCLISubprocessEnv},
		{name: "delegated agent", stream: "stdout", agent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_THREAD_ID", "")
			t.Setenv("CLAUDECODE", "")
			t.Setenv(daemonCLISubprocessEnv, "")
			if tc.env != "" {
				t.Setenv(tc.env, strconv.Itoa(os.Getppid()))
			}
			stream := tc.stream
			cmd := &cobra.Command{Use: "sync"}
			cmd.SetContext(withInvocation(t.Context(), &invocation{options: invocationOptions{agentURLChanged: tc.agent}}))
			cmd.SetIn(strings.NewReader(""))
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			var terminalFD uintptr
			switch stream {
			case "stdin":
				cmd.SetIn(os.Stdin)
				terminalFD = os.Stdin.Fd()
			case "stdout":
				cmd.SetOut(os.Stdout)
				terminalFD = os.Stdout.Fd()
			case "stderr":
				cmd.SetErr(os.Stderr)
				terminalFD = os.Stderr.Fd()
			}
			cliActionTerminal = func(fd uintptr) bool { return stream != "" && fd == terminalFD }
			assert.Equal(t, tc.want, humanCLIAction(cmd))
		})
	}
}

func TestCLIActionLocallyDisabledSendsNothing(t *testing.T) {
	clearTelemetryVetoEnvironment(t)
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDECODE", "")
	t.Setenv(telemetry.EnabledEnv, "")
	t.Setenv(posthog.GenericEnabledEnv, "")
	terminal := cliActionTerminal
	cliActionTerminal = func(uintptr) bool { return true }
	t.Cleanup(func() { cliActionTerminal = terminal })
	client, err := daemonclient.New(daemonclient.Config{URL: "https://daemon.example.test", HTTPClient: &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		assert.Fail(t, "disabled telemetry sent a request", r.URL.String())
		return nil, errors.New("unexpected request")
	})}})
	require.NoError(t, err)
	cmd := &cobra.Command{Use: "search"}
	cfg := config.NewDefaultConfig()
	enabled := false
	cfg.Telemetry.Enabled = &enabled
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	reportCLIAction(cmd, client, "search_run", "search_type", "text")
	invocationFromCommand(cmd).sends.Wait(time.Second)
}
