package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry/posthog"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/telemetry"
)

// tuiTelemetryDaemon records TUI telemetry events and answers with the status
// code and receipt the test chooses. An empty receipt sends no body.
type tuiTelemetryDaemon struct {
	mu      sync.Mutex
	events  []string
	code    int
	receipt string
	server  *httptest.Server
}

func newTUITelemetryDaemon(t *testing.T, receipt string) *tuiTelemetryDaemon {
	t.Helper()
	d := &tuiTelemetryDaemon{code: http.StatusAccepted, receipt: receipt}
	d.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Event      string            `json:"event"`
			Properties map[string]string `json:"properties"`
		}
		assert.NoError(t, json.UnmarshalRead(r.Body, &body))
		d.mu.Lock()
		defer d.mu.Unlock()
		d.events = append(d.events, body.Event+":"+body.Properties["screen"]+body.Properties["search_type"])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(d.code)
		if d.receipt != "" {
			_, _ = w.Write([]byte(`{"status":"` + d.receipt + `"}`))
		}
	}))
	t.Cleanup(d.server.Close)
	return d
}

func (d *tuiTelemetryDaemon) answer(code int, receipt string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.code, d.receipt = code, receipt
}

func (d *tuiTelemetryDaemon) sent() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.events...)
}

func (d *tuiTelemetryDaemon) client(t *testing.T) *daemonclient.Client {
	t.Helper()
	client, err := daemonclient.New(daemonclient.Config{URL: d.server.URL, AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func prepareTUITelemetryTest(t *testing.T, testName, subprocessEnv string) bool {
	t.Helper()
	// Serving tests permanently disable telemetry; run reporting assertions in a fresh process.
	if os.Getenv(subprocessEnv) != "1" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^"+testName+"$", "-test.count=1") //nolint:gosec // os.Args[0] is the test binary; args select a known test.
		cmd.Env = append(os.Environ(), subprocessEnv+"=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return false
	}
	clearTelemetryVetoEnvironment(t)
	t.Setenv(telemetry.EnabledEnv, "")
	t.Setenv(posthog.GenericEnabledEnv, "")
	return true
}

func TestTUIScreenAndActionReportersUseSelectedDaemon(t *testing.T) {
	if !prepareTUITelemetryTest(t, "TestTUIScreenAndActionReportersUseSelectedDaemon", "MSGVAULT_TUI_SCREEN_REPORT_TEST") {
		return
	}
	for _, tc := range []struct {
		name      string
		configOff bool
		envOff    bool
	}{
		{name: "enabled"},
		{name: "config opt-out", configOff: true},
		{name: "environment opt-out", envOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enabled := !tc.configOff
			cfg := &config.Config{Telemetry: config.TelemetryConfig{Enabled: &enabled}}
			if tc.envOff {
				t.Setenv("DO_NOT_TRACK", "1")
			}
			assert, require := assert.New(t), require.New(t)
			var requests []map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				assert.NoError(json.UnmarshalRead(r.Body, &body))
				assert.Equal("selected-daemon-key", r.Header.Get("X-Api-Key"))
				requests = append(requests, body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"queued"}`))
			}))
			defer server.Close()
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "selected-daemon-key", AllowInsecure: true})
			require.NoError(err)
			defer func() { require.NoError(client.Close()) }()
			gate := newTUITelemetryGate(t.TempDir(), client.BaseURL())
			require.NoError(tuiScreenReporter(gate, client, cfg)(t.Context(), "settings"))
			require.NoError(tuiActionReporter(gate, client, cfg)(t.Context(), "search_run", "search_type", "hybrid"))
			if tc.configOff || tc.envOff {
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

// reportAll sends a screen, a message open, and a search as one TUI process.
func reportAll(t *testing.T, gate *tuiTelemetryGate, client *daemonclient.Client) {
	t.Helper()
	_ = tuiScreenReporter(gate, client, &config.Config{})(t.Context(), "settings")
	_ = tuiActionReporter(gate, client, &config.Config{})(t.Context(), "message_opened", "", "")
	_ = tuiActionReporter(gate, client, &config.Config{})(t.Context(), "search_run", "search_type", "text")
}

func TestTUITelemetryGateKeepsReceiptsAcrossRestarts(t *testing.T) {
	if !prepareTUITelemetryTest(t, "TestTUITelemetryGateKeepsReceiptsAcrossRestarts", "MSGVAULT_TUI_GATE_RESTART_TEST") {
		return
	}
	for _, tc := range []struct {
		receipt string
		want    []string
	}{
		{receipt: "queued", want: []string{"screen_viewed:settings", "message_opened:", "search_run:text", "screen_viewed:directory"}},
		{receipt: "disabled", want: []string{"screen_viewed:settings"}},
	} {
		t.Run(tc.receipt, func(t *testing.T) {
			dataDir := t.TempDir()
			daemon := newTUITelemetryDaemon(t, tc.receipt)
			client := daemon.client(t)
			gate := newTUITelemetryGate(dataDir, client.BaseURL())
			reportAll(t, gate, client)
			restarted := newTUITelemetryGate(dataDir, client.BaseURL())
			if tc.receipt == "disabled" {
				for _, current := range []*tuiTelemetryGate{gate, restarted} {
					require.NoError(t, current.report(t.Context(), client, "session_ended", "duration_bucket", "1_to_5m"))
				}
			}
			reportAll(t, restarted, client)
			_ = tuiScreenReporter(restarted, client, &config.Config{})(t.Context(), "directory")
			assert.Equal(t, tc.want, daemon.sent())
		})
	}
}

func TestTUITelemetryGateSharesFailureBackoff(t *testing.T) {
	if !prepareTUITelemetryTest(t, "TestTUITelemetryGateSharesFailureBackoff", "MSGVAULT_TUI_GATE_BACKOFF_TEST") {
		return
	}
	assert, require := assert.New(t), require.New(t)
	dataDir := t.TempDir()
	daemon := newTUITelemetryDaemon(t, "")
	daemon.answer(http.StatusServiceUnavailable, "")
	client := daemon.client(t)
	gate := newTUITelemetryGate(dataDir, client.BaseURL())
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	gate.now = func() time.Time { return clock }
	for attempt := 1; attempt <= tuiTelemetryFailures; attempt++ {
		reportAll(t, gate, client)
		require.Len(daemon.sent(), attempt, "one failure waits out the shared window before any key checks again")
		if attempt < tuiTelemetryFailures {
			clock = gate.retryAt.Add(-time.Millisecond)
			reportAll(t, gate, client)
			require.Len(daemon.sent(), attempt)
			clock = gate.retryAt
		}
	}
	clock = clock.Add(time.Hour)
	reportAll(t, gate, client)
	assert.Len(daemon.sent(), tuiTelemetryFailures, "six failures stop the process")
	daemon.answer(http.StatusAccepted, "queued")
	restarted := newTUITelemetryGate(dataDir, client.BaseURL())
	restarted.now = gate.now
	reportAll(t, restarted, client)
	assert.Equal([]string{"screen_viewed:settings", "message_opened:", "search_run:text"}, daemon.sent()[tuiTelemetryFailures:], "failed keys stay unclaimed for a new process")
}

func TestTUITelemetryGateSendsShownErrorsEachTime(t *testing.T) {
	if !prepareTUITelemetryTest(t, "TestTUITelemetryGateSendsShownErrorsEachTime", "MSGVAULT_TUI_GATE_ERRORS_TEST") {
		return
	}
	daemon := newTUITelemetryDaemon(t, "queued")
	client := daemon.client(t)
	report := tuiActionReporter(newTUITelemetryGate(t.TempDir(), client.BaseURL()), client, &config.Config{})
	for range 2 {
		require.NoError(t, report(t.Context(), "error_shown", "kind", "search"))
		require.NoError(t, report(t.Context(), "search_empty", "search_type", "text"))
	}
	assert.Equal(t, []string{"error_shown:", "search_empty:text", "error_shown:"}, daemon.sent())
}

func TestTUITelemetryGateAcceptedReportClearsFailures(t *testing.T) {
	if !prepareTUITelemetryTest(t, "TestTUITelemetryGateAcceptedReportClearsFailures", "MSGVAULT_TUI_GATE_RECOVERY_TEST") {
		return
	}
	assert, require := assert.New(t), require.New(t)
	daemon := newTUITelemetryDaemon(t, "")
	client := daemon.client(t)
	gate := newTUITelemetryGate(t.TempDir(), client.BaseURL())
	clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	gate.now = func() time.Time { return clock }
	report := tuiScreenReporter(gate, client, &config.Config{})
	failRun := func(screen string) {
		daemon.answer(http.StatusServiceUnavailable, "")
		for range tuiTelemetryFailures - 1 {
			require.Error(report(t.Context(), screen))
			clock = gate.retryAt
		}
	}
	failRun("settings")
	daemon.answer(http.StatusAccepted, "queued")
	require.NoError(report(t.Context(), "settings"))
	failRun("directory")
	daemon.answer(http.StatusAccepted, "queued")
	require.NoError(report(t.Context(), "directory"))
	assert.Len(daemon.sent(), 2*tuiTelemetryFailures, "only failures in a row stop the process")
}

func TestTUITelemetryGateStaysStoppedAcrossMidnight(t *testing.T) {
	if !prepareTUITelemetryTest(t, "TestTUITelemetryGateStaysStoppedAcrossMidnight", "MSGVAULT_TUI_GATE_MIDNIGHT_STOP_TEST") {
		return
	}
	for _, code := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			dataDir := t.TempDir()
			daemon := newTUITelemetryDaemon(t, "")
			daemon.answer(code, "")
			client := daemon.client(t)
			gate := newTUITelemetryGate(dataDir, client.BaseURL())
			clock := time.Date(2026, 10, 20, 23, 0, 0, 0, time.UTC)
			gate.now = func() time.Time { return clock }
			attempts := 1
			if code == http.StatusServiceUnavailable {
				attempts = 6
			}
			for range attempts {
				reportAll(t, gate, client)
				if code == http.StatusServiceUnavailable {
					clock = gate.retryAt
				}
			}
			require.Len(daemon.sent(), attempts)
			clock = time.Date(2026, 10, 21, 0, 1, 0, 0, time.UTC)
			daemon.answer(http.StatusAccepted, "queued")
			_ = tuiScreenReporter(gate, client, &config.Config{})(t.Context(), "directory")
			require.NoError(gate.report(t.Context(), client, "session_ended", "duration_bucket", "1_to_5m"))
			assert.Len(daemon.sent(), attempts, "the stopped process stays quiet across midnight")
			restarted := newTUITelemetryGate(dataDir, client.BaseURL())
			restarted.now = gate.now
			_ = tuiScreenReporter(restarted, client, &config.Config{})(t.Context(), "settings")
			assert.Equal([]string{"screen_viewed:settings"}, daemon.sent()[attempts:], "a fresh process can report the unclaimed key")
		})
	}
}

func TestTUITelemetryGateRollsOverAtUTCMidnight(t *testing.T) {
	if !prepareTUITelemetryTest(t, "TestTUITelemetryGateRollsOverAtUTCMidnight", "MSGVAULT_TUI_GATE_MIDNIGHT_TEST") {
		return
	}
	daemon := newTUITelemetryDaemon(t, "disabled")
	client := daemon.client(t)
	dataDir := t.TempDir()
	gate := newTUITelemetryGate(dataDir, client.BaseURL())
	clock := time.Date(2026, 10, 20, 23, 59, 0, 0, time.UTC)
	gate.now = func() time.Time { return clock }
	reportAll(t, gate, client)
	clock = clock.Add(2 * time.Minute)
	daemon.answer(http.StatusAccepted, "queued")
	reportAll(t, gate, client)
	restarted := newTUITelemetryGate(dataDir, client.BaseURL())
	restarted.now = gate.now
	reportAll(t, restarted, client)
	assert.Equal(t, []string{"screen_viewed:settings", "screen_viewed:settings", "message_opened:", "search_run:text"}, daemon.sent())
}

func TestTUITelemetryGateLateCompletionLeavesNextDayFileAlone(t *testing.T) {
	if !prepareTUITelemetryTest(t, "TestTUITelemetryGateLateCompletionLeavesNextDayFileAlone", "MSGVAULT_TUI_GATE_LATE_TEST") {
		return
	}
	for _, saved := range []string{"", `{"day":"2026-10-21","disabled":true}`} {
		t.Run(saved, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					<-release
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"status":"queued"}`))
				}))
				transport := http.DefaultTransport
				http.DefaultTransport = server.Client().Transport
				t.Cleanup(func() { http.DefaultTransport = transport })
				client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, client.Close()) })
				gate := newTUITelemetryGate(t.TempDir(), server.URL)
				clock := time.Date(2026, 10, 20, 23, 59, 0, 0, time.UTC)
				gate.now = func() time.Time { return clock }
				finished := make(chan error, 1)
				go func() { finished <- tuiScreenReporter(gate, client, &config.Config{})(t.Context(), "settings") }()
				synctest.Wait()
				clock = clock.Add(2 * time.Minute)
				if saved != "" {
					require.NoError(t, os.WriteFile(gate.path, []byte(saved), 0o600))
				}
				close(release)
				require.NoError(t, <-finished)
				if saved == "" {
					assert.NoFileExists(t, gate.path)
				} else {
					data, err := os.ReadFile(gate.path)
					require.NoError(t, err)
					assert.Equal(t, saved, string(data))
				}
			})
		})
	}
}

func TestTUITelemetryGateScopesReceiptsToEndpoint(t *testing.T) {
	if !prepareTUITelemetryTest(t, "TestTUITelemetryGateScopesReceiptsToEndpoint", "MSGVAULT_TUI_GATE_ENDPOINT_TEST") {
		return
	}
	dataDir := t.TempDir()
	disabled, enabled := newTUITelemetryDaemon(t, "disabled"), newTUITelemetryDaemon(t, "queued")
	first, second := disabled.client(t), enabled.client(t)
	reportAll(t, newTUITelemetryGate(dataDir, first.BaseURL()), first)
	reportAll(t, newTUITelemetryGate(dataDir, second.BaseURL()), second)
	assert.Equal(t, []string{"screen_viewed:settings"}, disabled.sent())
	assert.Equal(t, []string{"screen_viewed:settings", "message_opened:", "search_run:text"}, enabled.sent())
}

func TestTUITelemetryGateRetriesUnconfirmedReceipts(t *testing.T) {
	if !prepareTUITelemetryTest(t, "TestTUITelemetryGateRetriesUnconfirmedReceipts", "MSGVAULT_TUI_GATE_UNCONFIRMED_TEST") {
		return
	}
	for _, receipt := range []string{"", "unknown"} {
		t.Run(fmt.Sprintf("receipt=%q", receipt), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			daemon := newTUITelemetryDaemon(t, receipt)
			client := daemon.client(t)
			dataDir := t.TempDir()
			gate := newTUITelemetryGate(dataDir, client.BaseURL())
			now := time.Now()
			gate.now = func() time.Time { return now }
			require.Error(tuiScreenReporter(gate, client, &config.Config{})(t.Context(), "settings"))
			require.NoFileExists(gate.path)
			daemon.answer(http.StatusAccepted, "queued")
			now = gate.retryAt
			require.NoError(tuiScreenReporter(gate, client, &config.Config{})(t.Context(), "settings"))
			restarted := newTUITelemetryGate(dataDir, client.BaseURL())
			restarted.now = gate.now
			require.NoError(tuiScreenReporter(restarted, client, &config.Config{})(t.Context(), "settings"))
			assert.Equal([]string{"screen_viewed:settings", "screen_viewed:settings"}, daemon.sent())
		})
	}
}

func TestTUITelemetryGateRecoversMalformedState(t *testing.T) {
	if !prepareTUITelemetryTest(t, "TestTUITelemetryGateRecoversMalformedState", "MSGVAULT_TUI_GATE_MALFORMED_TEST") {
		return
	}
	require := require.New(t)
	dataDir := t.TempDir()
	daemon := newTUITelemetryDaemon(t, "queued")
	client := daemon.client(t)
	gate := newTUITelemetryGate(dataDir, client.BaseURL())
	gate.now = func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }
	require.NoError(os.WriteFile(gate.path, []byte(`{"day":"2026-10-20","sent":["screen_viewed:settings"]}`), 0o600))
	_ = tuiScreenReporter(gate, client, &config.Config{})(t.Context(), "settings")
	restarted := newTUITelemetryGate(dataDir, client.BaseURL())
	restarted.now = gate.now
	_ = tuiScreenReporter(restarted, client, &config.Config{})(t.Context(), "settings")
	assert.Equal(t, []string{"screen_viewed:settings"}, daemon.sent())
}

func TestTUITelemetryGateSerializesReports(t *testing.T) {
	if !prepareTUITelemetryTest(t, "TestTUITelemetryGateSerializesReports", "MSGVAULT_TUI_GATE_SERIAL_TEST") {
		return
	}
	for _, receipt := range []string{"disabled", "queued"} {
		t.Run(receipt, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var screens []string
				release := make(chan struct{})
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						Properties map[string]string `json:"properties"`
					}
					assert.NoError(t, json.UnmarshalRead(r.Body, &body))
					screens = append(screens, body.Properties["screen"])
					<-release
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"status":"` + receipt + `"}`))
				}))
				transport := http.DefaultTransport
				http.DefaultTransport = server.Client().Transport
				t.Cleanup(func() { http.DefaultTransport = transport })
				client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, client.Close()) })
				gate := newTUITelemetryGate("", server.URL)
				report := tuiScreenReporter(gate, client, &config.Config{})
				finished := make(chan error, 3)
				go func() { finished <- report(t.Context(), "directory") }()
				synctest.Wait()
				go func() { finished <- report(t.Context(), "email") }()
				go func() { finished <- report(t.Context(), "directory") }()
				synctest.Wait()
				assert.Equal(t, []string{"directory"}, screens)

				ctx, cancel := context.WithCancel(t.Context())
				waiter := make(chan error, 1)
				go func() { waiter <- report(ctx, "settings") }()
				synctest.Wait()
				cancel()
				require.ErrorIs(t, <-waiter, context.Canceled)
				// Check before accepted reports finish, since an accepted report clears failures.
				assert.Zero(t, gate.failures, "a canceled waiter spends no failure budget")
				assert.Equal(t, []string{"directory"}, screens)

				close(release)
				for range 3 {
					require.NoError(t, <-finished)
				}
				want := []string{"directory"}
				if receipt == "queued" {
					want = append(want, "email")
				}
				assert.Equal(t, want, screens)
			})
		})
	}
}
