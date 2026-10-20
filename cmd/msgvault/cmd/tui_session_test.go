package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry/posthog"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/telemetry"
)

func TestTUISessionUsesSelectedDaemonAfterCancellation(t *testing.T) {
	// Serving tests permanently disable telemetry; run reporting assertions in a fresh process.
	const subprocessEnv = "MSGVAULT_TUI_SESSION_REPORT_TEST"
	if os.Getenv(subprocessEnv) != "1" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestTUISessionUsesSelectedDaemonAfterCancellation$", "-test.count=1") //nolint:gosec // os.Args[0] is the test binary; args are fixed test flags.
		cmd.Env = append(os.Environ(), subprocessEnv+"=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return
	}
	for _, tc := range []struct {
		name, env, value   string
		timeout, configOff bool
	}{
		{name: "delivery"},
		{name: "timeout", timeout: true},
		{name: "do not track", env: "DO_NOT_TRACK", value: "1"},
		{name: "CI present and empty", env: "CI"},
		{name: "product opt-out", env: telemetry.EnabledEnv, value: "0"},
		{name: "global opt-out", env: posthog.GenericEnabledEnv, value: "0"},
		{name: "config opt-out", configOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearTelemetryVetoEnvironment(t)
			t.Setenv(telemetry.EnabledEnv, "1")
			t.Setenv(posthog.GenericEnabledEnv, "")
			if tc.env != "" {
				t.Setenv(tc.env, tc.value)
			}
			if tc.configOff {
				t.Setenv(telemetry.EnabledEnv, "")
			}
			synctest.Test(t, func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				requests := 0
				release := make(chan struct{})
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					assert.Equal("/api/v1/telemetry/events", r.URL.Path)
					assert.Equal("selected-daemon-key", r.Header.Get("X-Api-Key"))
					var body map[string]any
					assert.NoError(json.UnmarshalRead(r.Body, &body))
					assert.Equal(map[string]any{"event": "session_ended", "properties": map[string]any{"surface": "tui", "duration_bucket": "1_to_5m"}}, body)
					if tc.timeout {
						<-release
					} else {
						time.Sleep(5 * time.Millisecond)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"status":"queued"}`))
				}))
				transport := http.DefaultTransport
				http.DefaultTransport = server.Client().Transport
				t.Cleanup(func() { http.DefaultTransport = transport })
				var exitStart time.Time
				root := &cobra.Command{Use: "tui", RunE: func(cmd *cobra.Command, _ []string) error {
					ctx, cancel := context.WithCancel(cmd.Context())
					enabled := !tc.configOff
					cfg := &config.Config{Telemetry: config.TelemetryConfig{Enabled: &enabled}}
					cfg.Remote.APIKey = "selected-daemon-key"
					cfg.Remote.AllowInsecure = true
					cancel()
					require.ErrorIs(ctx.Err(), context.Canceled)
					reportTUISession(ctx, cfg, HTTPStoreInfo{Kind: HTTPStoreConfiguredRemote, URL: server.URL}, 2*time.Minute)
					exitStart = time.Now()
					return nil
				}}
				root.SetArgs([]string{})
				require.NoError(executeRootContext(context.Background(), root))
				synctest.Wait()
				if tc.env != "" || tc.configOff {
					invocationFromCommand(root).sends.Wait(time.Second)
					assert.Zero(requests)
					close(release)
					return
				}
				want := 5 * time.Millisecond
				if tc.timeout {
					want = 50 * time.Millisecond
				}
				assert.Equal(want, time.Since(exitStart))
				assert.Equal(1, requests)
				close(release)
				invocationFromCommand(root).sends.Wait(time.Second)
			})
		})
	}
}
