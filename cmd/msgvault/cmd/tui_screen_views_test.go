package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry/posthog"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/telemetry"
)

func TestTUIScreenAndActionReportersUseSelectedDaemon(t *testing.T) {
	// Serving tests permanently disable telemetry; run reporting assertions in a fresh process.
	const subprocessEnv = "MSGVAULT_TUI_SCREEN_REPORT_TEST"
	if os.Getenv(subprocessEnv) != "1" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestTUIScreenAndActionReportersUseSelectedDaemon$", "-test.count=1") //nolint:gosec // os.Args[0] is the test binary; args are fixed test flags.
		cmd.Env = append(os.Environ(), subprocessEnv+"=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return
	}
	for _, tc := range []struct {
		name, env, value string
		configOff        bool
	}{
		{name: "enabled"},
		{name: "config opt-out", configOff: true},
		{name: "do not track", env: "DO_NOT_TRACK", value: "1"},
		{name: "CI present and empty", env: "CI"},
		{name: "product opt-out", env: telemetry.EnabledEnv, value: "0"},
		{name: "global opt-out", env: posthog.GenericEnabledEnv, value: "0"},
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
			enabled := !tc.configOff
			cfg := &config.Config{Telemetry: config.TelemetryConfig{Enabled: &enabled}}
			assert, require := assert.New(t), require.New(t)
			var requests []map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				assert.NoError(json.UnmarshalRead(r.Body, &body))
				requests = append(requests, body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"queued"}`))
			}))
			defer server.Close()
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "selected-daemon-key", AllowInsecure: true})
			require.NoError(err)
			require.NoError(tuiScreenReporter(client, cfg)(context.Background(), "settings"))
			require.NoError(tuiActionReporter(client, cfg)(context.Background(), "search_run", "search_type", "hybrid"))
			if tc.env != "" || tc.configOff {
				assert.Empty(requests)
				return
			}
			assert.Equal([]map[string]any{
				{"event": "screen_viewed", "properties": map[string]any{"screen": "settings", "surface": "tui"}},
				{"event": "search_run", "properties": map[string]any{"search_type": "hybrid", "surface": "tui"}},
			}, requests)
		})
	}
}
