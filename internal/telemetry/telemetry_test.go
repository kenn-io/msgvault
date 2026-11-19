package telemetry

import (
	"encoding/json/v2"
	"fmt"
	"io"
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
	wireStubEnv = "MSGVAULT_TELEMETRY_WIRE_STUB"
	wireDirEnv  = "MSGVAULT_TELEMETRY_WIRE_DIR"
	// testAPIKey stands in for the key release builds set at link time.
	testAPIKey = "phc_test"
)

// enableTelemetryEnv opts this test in and clears ambient opt-outs. An opted-out
// in-process reporter disables telemetry for the rest of the test binary.
func enableTelemetryEnv(t *testing.T) {
	t.Helper()
	t.Setenv(posthog.PrefixedEnabledEnv("MSGVAULT"), "")
	t.Setenv(posthog.GenericEnabledEnv, "1")
	for _, name := range []string{"CI", "DO_NOT_TRACK"} {
		t.Setenv(name, os.Getenv(name))
		require.NoError(t, os.Unsetenv(name))
	}
}

// An install file the daemon can't create must leave serve running with telemetry off.
func TestNewReporterOrDisabledFallsBackWhenInstallFails(t *testing.T) {
	assert := assert.New(t)
	enableTelemetryEnv(t)
	notDir := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o600))

	reporter := NewReporterOrDisabled(Options{DataDir: notDir})
	assert.False(reporter.Enabled())
}

// TestEnabledReporterWireHelper runs only inside the helper process the wire tests start.
func TestEnabledReporterWireHelper(t *testing.T) {
	stub := os.Getenv(wireStubEnv)
	if stub == "" {
		return
	}
	if os.Getenv("MSGVAULT_TEST_CONSENT") == "yes" {
		require.NoError(t, posthog.RecordConsent(true))
	}
	opts := Options{DataDir: os.Getenv(wireDirEnv), Version: "v1.2.3", Commit: "test-commit"}
	require := require.New(t)
	if os.Getenv("MSGVAULT_SCREEN_VIEWS_TEST") == "rollover" {
		exerciseDailyViewRollover(t, opts)
		return
	}
	reporter := newReporterOrDisabled(opts, testAPIKey, stub)
	require.True(reporter.EventAllowed(EventAppOpened), "an opted-out reporter keeps the allowlist")
	if os.Getenv("MSGVAULT_OUTCOMES_TEST") != "" {
		exerciseOutcomes(t, CaptureHandler(reporter, opts.DataDir), reporter)
		require.NoError(reporter.Close())
		return
	}
	if os.Getenv("MSGVAULT_SESSION_TEST") != "" {
		h := CaptureHandler(reporter, opts.DataDir)
		for _, body := range []string{
			`{"event":"session_ended","properties":{"surface":"web","duration_bucket":"1_to_5m"}}`,
			`{"event":"session_ended","properties":{"surface":"tui","duration_bucket":"5_to_30m"}}`,
			`{"event":"session_ended","properties":{"surface":"private","duration_bucket":"private"}}`,
		} {
			require.Equal(http.StatusAccepted, screenRequest(t.Context(), h, body).Code)
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
	stub.server = httptest.NewServer(http.HandlerFunc(stub.serveHTTP))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *wireStub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, recordedRequest{path: r.URL.Path, body: body})
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":1}`))
}

func (s *wireStub) recorded() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// runWireHelper runs the helper with only the given telemetry variables set.
func runWireHelper(t *testing.T, stubURL, dir string, env ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestEnabledReporterWireHelper$", "-test.count=1") //nolint:gosec // os.Args[0] is the test binary; args are fixed test flags.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, posthog.PrefixedEnabledEnv("MSGVAULT")+"=") && !strings.HasPrefix(kv, posthog.GenericEnabledEnv+"=") &&
			!strings.HasPrefix(kv, "CI=") && !strings.HasPrefix(kv, "DO_NOT_TRACK=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	home := t.TempDir()
	cmd.Env = append(cmd.Env, "HOME="+home, "USERPROFILE="+home, wireStubEnv+"="+stubURL, wireDirEnv+"="+dir)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
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

	runWireHelper(t, stub.server.URL, dir, "MSGVAULT_TEST_CONSENT=yes")

	messages := batchEvents(t, stub)
	require.Len(messages, 2)
	kitKeys := []string{"$process_person_profile", "$geoip_disable", "application", "source", "version", "commit", "distribution", "goos", "goarch", "install_age_hours", "telemetry_consent"}
	sdkKeys := []string{"$lib", "$lib_version"}
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
				slices.Contains(eventKeys, key)
			assert.True(allowed, "unexpected property %q on %v", key, message["event"])
		}
		assert.Equal(true, props["telemetry_consent"])
		assert.Equal("msgvault", props["application"])
		assert.Equal("daemon", props["source"])
		assert.Equal("v1.2.3", props["version"])
		assert.Equal("test-commit", props["commit"])
		assert.Empty(props["distribution"], "a build without the link-time flag reports no distribution")
	}
	assert.ElementsMatch([]string{EventAppOpened, posthog.EventDaemonActive}, events)
}

func TestOptedOutReporterSendsNothing(t *testing.T) {
	cases := []struct {
		name string
		env  []string
	}{
		{"prefixed variable", []string{posthog.PrefixedEnabledEnv("MSGVAULT") + "=0", posthog.GenericEnabledEnv + "=1", "MSGVAULT_SCREEN_VIEWS_TEST=1"}},
		{"generic variable", []string{"MSGVAULT_TEST_CONSENT=yes", posthog.GenericEnabledEnv + "=0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newWireStub(t)
			dir := t.TempDir()
			runWireHelper(t, stub.server.URL, dir, tc.env...)
			assert.Empty(t, stub.recorded(), "an opted-out daemon must send nothing")
			assert.NoFileExists(t, filepath.Join(dir, posthog.InstallFileName))
		})
	}
}

func TestTelemetryUnansweredConsentSendsNothing(t *testing.T) {
	stub := newWireStub(t)
	dir := t.TempDir()
	runWireHelper(t, stub.server.URL, dir)
	assert.Empty(t, stub.recorded())
	assert.NoFileExists(t, filepath.Join(dir, posthog.InstallFileName))
}
