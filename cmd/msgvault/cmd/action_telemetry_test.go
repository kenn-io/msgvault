package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry/posthog"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/telemetry"
)

func clearTelemetryVetoEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"CI", "DO_NOT_TRACK"} {
		t.Setenv(name, os.Getenv(name))
		require.NoError(t, os.Unsetenv(name), "clear %s", name)
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
