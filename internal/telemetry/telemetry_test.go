package telemetry

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry/posthog"
)

const (
	wireStubEnv      = "MSGVAULT_TELEMETRY_WIRE_STUB"
	wireDirEnv       = "MSGVAULT_TELEMETRY_WIRE_DIR"
	wireConfigOffEnv = "MSGVAULT_TELEMETRY_WIRE_CONFIG_OFF"
)

// An install file the daemon can't create must leave serve running with telemetry off.
func TestNewReporterOrDisabledFallsBackWhenInstallFails(t *testing.T) {
	assert := assert.New(t)
	t.Setenv(EnabledEnv, "1")
	t.Setenv(posthog.GenericEnabledEnv, "")
	notDir := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o600))
	var logs bytes.Buffer

	reporter := NewReporterOrDisabled(Options{DataDir: notDir, ConfigEnabled: true}, slog.New(slog.NewTextHandler(&logs, nil)))
	assert.False(reporter.Enabled())
	assert.Contains(logs.String(), "telemetry disabled")
	assert.NotContains(logs.String(), "telemetry is on")
}

// TestEnabledReporterWireHelper runs only inside the helper process the wire tests start.
func TestEnabledReporterWireHelper(t *testing.T) {
	stub := os.Getenv(wireStubEnv)
	if stub == "" {
		return
	}
	opts := Options{DataDir: os.Getenv(wireDirEnv), Version: "test-version", Commit: "test-commit", ConfigEnabled: os.Getenv(wireConfigOffEnv) == ""}
	require := require.New(t)
	reporter := newReporterOrDisabled(opts, stub, slog.New(slog.NewTextHandler(os.Stdout, nil)))
	require.True(reporter.EventAllowed(EventAppOpened), "an opted-out reporter keeps the allowlist")
	if os.Getenv("MSGVAULT_SESSION_TEST") != "" {
		h := CaptureHandler(reporter, opts.DataDir)
		for _, body := range []string{
			`{"event":"session_ended","properties":{"surface":"web","duration_bucket":"1_to_5m"}}`,
			`{"event":"session_ended","properties":{"surface":"tui","duration_bucket":"5_to_30m"}}`,
			`{"event":"session_ended","properties":{"surface":"private","duration_bucket":"private"}}`,
		} {
			require.Equal(http.StatusAccepted, screenRequest(h, body).Code)
		}
		require.NoError(reporter.Close())
		return
	}
	if os.Getenv("MSGVAULT_SCREEN_VIEWS_TEST") != "" {
		exerciseScreenViews(t, reporter, opts)
		require.NoError(reporter.Close())
		return
	}
	require.NoError(reporter.Capture(EventAppOpened, map[string]any{"query": "q", "account": "a", propertySurface: "web"}))
	require.NoError(reporter.Capture(posthog.EventDaemonActive, nil))
	require.NoError(reporter.Close())
}

type recordedRequest struct {
	path string
	body []byte
}

type wireStub struct {
	mu       sync.Mutex
	requests []recordedRequest
	server   *httptest.Server
}

func newWireStub(t *testing.T) *wireStub {
	t.Helper()
	stub := &wireStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		stub.mu.Lock()
		stub.requests = append(stub.requests, recordedRequest{path: r.URL.Path, body: body})
		stub.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":1}`))
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *wireStub) recorded() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// runWireHelper runs the helper with only the given telemetry variables set and returns its output.
func runWireHelper(t *testing.T, stubURL, dir string, env ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestEnabledReporterWireHelper$", "-test.count=1") //nolint:gosec // os.Args[0] is the test binary; args are fixed test flags.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, EnabledEnv+"=") && !strings.HasPrefix(kv, posthog.GenericEnabledEnv+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, wireStubEnv+"="+stubURL, wireDirEnv+"="+dir)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

func batchEvents(t *testing.T, stub *wireStub) []map[string]any {
	t.Helper()
	var messages []map[string]any
	for _, req := range stub.recorded() {
		if req.path != "/batch/" {
			continue
		}
		var batch struct {
			Messages []map[string]any `json:"batch"`
		}
		require.NoError(t, json.Unmarshal(req.body, &batch), string(req.body))
		messages = append(messages, batch.Messages...)
	}
	return messages
}

func TestEnabledReporterSendsOnlyAllowlistedFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	stub := newWireStub(t)
	dir := t.TempDir()
	install, err := posthog.LoadOrCreateInstall(dir)
	require.NoError(err)

	out := runWireHelper(t, stub.server.URL, dir)
	assert.Contains(out, "telemetry is on", "serve tells the user how to turn telemetry off")
	assert.Contains(out, EnabledEnv+"=0")

	messages := batchEvents(t, stub)
	require.Len(messages, 2)
	kitKeys := []string{"$process_person_profile", "$geoip_disable", "application", "source", "version", "commit", "goos", "goarch", "install_age_hours"}
	sdkKeys := []string{"$lib", "$lib_version", "$os", "$go_version"}
	optionalSDKKeys := []string{"$os_version", "$os_distro"}
	events := make([]string, 0, len(messages))
	for _, message := range messages {
		events = append(events, fmt.Sprint(message["event"]))
		assert.Equal(install.ID, message["distinct_id"])
		props, ok := message["properties"].(map[string]any)
		require.True(ok, "properties object")
		for _, key := range append(slices.Clone(kitKeys), sdkKeys...) {
			assert.Contains(props, key)
		}
		eventKeys := []string{}
		if message["event"] == EventAppOpened {
			eventKeys = []string{propertySurface}
			assert.Equal("web", props[propertySurface])
		}
		for key := range props {
			allowed := slices.Contains(kitKeys, key) || slices.Contains(sdkKeys, key) ||
				slices.Contains(optionalSDKKeys, key) || slices.Contains(eventKeys, key)
			assert.True(allowed, "unexpected property %q on %v", key, message["event"])
		}
		assert.Equal("msgvault", props["application"])
		assert.Equal("daemon", props["source"])
		assert.Equal("test-version", props["version"])
		assert.Equal("test-commit", props["commit"])
	}
	assert.ElementsMatch([]string{EventAppOpened, posthog.EventDaemonActive}, events)
}

// A daemon started by launchd or systemd may have the env var set where the config says off; the env var wins.
func TestEnvOverridesConfigOff(t *testing.T) {
	stub := newWireStub(t)
	runWireHelper(t, stub.server.URL, t.TempDir(), wireConfigOffEnv+"=1", EnabledEnv+"=1")
	assert.Len(t, batchEvents(t, stub), 2)
}

func TestOptedOutReporterSendsNothing(t *testing.T) {
	cases := []struct {
		name string
		env  []string
	}{
		{"config off", []string{wireConfigOffEnv + "=1"}},
		{"prefixed variable", []string{EnabledEnv + "=0", "MSGVAULT_SCREEN_VIEWS_TEST=1"}},
		{"generic variable", []string{EnabledEnv + "=1", posthog.GenericEnabledEnv + "=0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newWireStub(t)
			dir := t.TempDir()
			out := runWireHelper(t, stub.server.URL, dir, tc.env...)
			assert.Empty(t, stub.recorded(), "an opted-out daemon must send nothing")
			assert.NoFileExists(t, filepath.Join(dir, posthog.InstallFileName))
			assert.NotContains(t, out, "telemetry is on")
		})
	}
}
