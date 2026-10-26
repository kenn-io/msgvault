package telemetry

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry/posthog"
)

func screenRequest(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	return response
}

func exerciseScreenViews(t *testing.T, reporter *posthog.Reporter, opts Options) {
	t.Helper()
	assertions, requirements := assert.New(t), require.New(t)
	h, ok := CaptureHandler(reporter, opts.DataDir).(*screenCaptureHandler)
	requirements.True(ok)
	day := time.Date(2026, 1, 2, 23, 59, 0, 0, time.UTC)
	h.now = func() time.Time { return day }
	valid := `{"event":"screen_viewed","properties":{"screen":"settings","surface":"web"}}`
	if os.Getenv("MSGVAULT_SCREEN_VIEWS_TEST") == "upgrade" {
		install, err := posthog.LoadOrCreateInstall(opts.DataDir)
		requirements.NoError(err)
		// The previous daemon stored screen, message, and search claims together.
		mixed := fmt.Sprintf(`{"install_id":%q,"day":"2026-01-02","screens":{"settings":true,"message_opened:web":true,"search_run:web:text":true}}`, install.ID)
		requirements.NoError(os.WriteFile(filepath.Join(opts.DataDir, screenViewsFile), []byte(mixed), 0o600))
		for _, body := range []string{
			valid,
			`{"event":"message_opened","properties":{"surface":"web"}}`,
			`{"event":"search_run","properties":{"surface":"web","search_type":"text"}}`,
		} {
			assertions.JSONEq(`{"status":"queued"}`, screenRequest(h, body).Body.String())
		}
		return
	}
	if os.Getenv("MSGVAULT_SCREEN_VIEWS_TEST") == "restart" {
		day = day.Add(2 * time.Minute)
		assertions.Equal(http.StatusAccepted, screenRequest(h, valid).Code)
		assertions.Equal(http.StatusAccepted, screenRequest(h, `{"event":"search_run","properties":{"surface":"cli","search_type":"text"}}`).Code)
		assertions.Equal(http.StatusAccepted, screenRequest(h, `{"event":"search_run","properties":{"surface":"tui","search_type":"text"}}`).Code)
		return
	}
	for _, body := range []string{
		`{"Event":"app_opened","Properties":{"surface":"web"}}`,
		`{"event":"unknown","event":"app_opened","properties":{"surface":"tui","surface":"web"}}`,
		"{\"event\":\"app_opened\",\"properties\":{\"surface\":\"web\",\"private\":\"\xff\"}}",
	} {
		assertions.Equal(http.StatusAccepted, screenRequest(h, body).Code)
	}
	if !reporter.Enabled() {
		for _, body := range []string{
			valid,
			`{"event":"message_opened","properties":{"surface":"tui"}}`,
			`{"event":"search_run","properties":{"surface":"cli","search_type":"text"}}`,
		} {
			response := screenRequest(h, body)
			assertions.Equal(http.StatusAccepted, response.Code)
			assertions.JSONEq(`{"status":"disabled"}`, response.Body.String())
		}
		assertions.NoFileExists(filepath.Join(opts.DataDir, screenViewsFile))
		assertions.NoFileExists(filepath.Join(opts.DataDir, "telemetry-screen-claims.json"))
		return
	}
	for _, body := range []string{
		`{"event":"screen_viewed","properties":{"screen":"private","surface":"web"}}`,
		`{"event":"screen_viewed","properties":{"screen":"settings","surface":"private"}}`,
		`{"event":"screen_viewed","properties":{"screen":"settings"}}`,
	} {
		assertions.Equal(http.StatusAccepted, screenRequest(h, body).Code)
	}
	assertions.Equal(http.StatusBadRequest, screenRequest(h, valid+`{}`).Code)
	assertions.Equal(http.StatusAccepted, screenRequest(h, `{"Event":"unknown","event":"screen_viewed","Properties":{"screen":"private","screen":"settings","surface":"web"}}`).Code)
	assertions.Equal(http.StatusRequestEntityTooLarge, screenRequest(h, strings.Repeat(" ", 64<<10)+valid).Code)
	assertions.Equal(http.StatusAccepted, screenRequest(h, `{"event":" screen_viewed ","properties":{" screen ":"directory","surface":"tui","content":"private"}}`).Code)
	conflicting := `{"event":"screen_viewed","properties":{"screen":"email"," screen ":"texts","surface":"web"}}`
	assertions.Equal(http.StatusAccepted, screenRequest(h, conflicting).Code)
	for _, event := range []string{EventSearchRun, EventMessageOpened, EventSyncFinished} {
		assertions.Equal(http.StatusAccepted, screenRequest(h, fmt.Sprintf(`{"event":%q,"properties":{"surface":"other"}}`, event)).Code)
	}
	assertions.Equal(http.StatusAccepted, screenRequest(h, `{"event":"search_run","properties":{"surface":"web","search_type":"other"}}`).Code)
	assertions.Equal(http.StatusAccepted, screenRequest(h, `{"event":"search_run","properties":{"surface":"web"}}`).Code)
	for _, surface := range []string{"web", "tui", "cli"} {
		for _, kind := range []string{"text", "semantic", "hybrid"} {
			body := fmt.Sprintf(`{"event":"search_run","properties":{"surface":%q,"search_type":%q,"query":"private"}}`, surface, kind)
			for range 2 {
				assertions.Equal(http.StatusAccepted, screenRequest(h, body).Code)
			}
		}
	}
	capture := h.captureEvent
	h.captureEvent = func(string, map[string]any) error { return errors.New("queue full") }
	path := filepath.Join(opts.DataDir, screenViewsFile)
	previous, err := os.ReadFile(path)
	requirements.NoError(err)
	h.now = func() time.Time { return day.Add(24 * time.Hour) }
	assertions.Equal(http.StatusInternalServerError, screenRequest(h, `{"event":"search_run","properties":{"surface":"web","search_type":"text"}}`).Code)
	h.now = func() time.Time { return day }
	restored, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Equal(previous, restored, "failed capture rolls the daily claim back")
	h.captureEvent = capture
	for _, surface := range []string{"web", "tui", "cli"} {
		for range 2 {
			assertions.Equal(http.StatusAccepted, screenRequest(h, fmt.Sprintf(`{"event":"sync_finished","properties":{"surface":%q,"message_id":1}}`, surface)).Code)
		}
	}
	day = day.Add(2 * time.Minute)
	claimsPath := filepath.Join(opts.DataDir, screenViewsFile)
	previous, err = os.ReadFile(claimsPath)
	requirements.NoError(err)
	requirements.NoError(os.Remove(claimsPath))
	requirements.NoError(os.Mkdir(claimsPath, 0o700))
	assertions.Equal(http.StatusInternalServerError, screenRequest(h, `{"event":"search_run","properties":{"surface":"cli","search_type":"text"}}`).Code)
	requirements.NoError(os.Remove(claimsPath))
	requirements.NoError(os.WriteFile(claimsPath, previous, 0o600))
	assertions.Equal(http.StatusAccepted, screenRequest(h, `{"event":"search_run","properties":{"surface":"cli","search_type":"text"}}`).Code)
	assertions.Equal(http.StatusAccepted, screenRequest(h, `{"event":"search_run","properties":{"surface":"tui","search_type":"text"}}`).Code)
}

func TestMixedLedgerUpgradePreservesSearchClaims(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	stub := newWireStub(t)
	dir := t.TempDir()
	runWireHelper(t, stub.server.URL, dir, "MSGVAULT_SCREEN_VIEWS_TEST=upgrade")
	events := batchEvents(t, stub)
	require.Len(events, 2, "only screen and message claims recount on upgrade")
	assert.Equal(EventScreenViewed, events[0]["event"])
	assert.Equal(EventMessageOpened, events[1]["event"])
}

func TestViewsAndActionsCountAcrossSurfacesAndRestarts(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	stub := newWireStub(t)
	dir := t.TempDir()
	runWireHelper(t, stub.server.URL, dir, "MSGVAULT_SCREEN_VIEWS_TEST=1")
	screenMessages := func() []map[string]any {
		return slices.DeleteFunc(batchEvents(t, stub), func(event map[string]any) bool { return event["event"] != EventScreenViewed })
	}
	requirements.Len(batchEvents(t, stub), 19)
	counts := map[string]int{}
	for _, event := range batchEvents(t, stub) {
		if event["event"] == EventScreenViewed || event["event"] == EventAppOpened {
			continue
		}
		props, ok := event["properties"].(map[string]any)
		requirements.True(ok)
		assertions.NotContains(props, "query")
		assertions.NotContains(props, "message_id")
		counts[fmt.Sprint(event["event"], ":", props["surface"], ":", props["search_type"])]++
	}
	assertions.Equal(2, counts["search_run:cli:text"])
	assertions.Equal(2, counts["search_run:tui:text"])
	assertions.Equal(2, counts["sync_finished:cli:<nil>"], "successful CLI syncs count individually")
	assertions.Len(counts, 10)
	messages := screenMessages()
	requirements.Len(messages, 3)
	screens := []string{}
	for _, event := range messages {
		assertions.Equal(EventScreenViewed, event["event"])
		props, ok := event["properties"].(map[string]any)
		requirements.True(ok)
		screens = append(screens, fmt.Sprint(props["screen"]))
		assertions.NotContains(props, "content")
		assertions.Contains([]string{"web", "tui"}, props["surface"])
	}
	assertions.Contains(screens, "directory")
	assertions.Equal(1, strings.Count(strings.Join(screens, ","), "settings"))
	runWireHelper(t, stub.server.URL, dir, "MSGVAULT_SCREEN_VIEWS_TEST=restart")
	assertions.Len(screenMessages(), 3)
	assertions.Len(batchEvents(t, stub), 19)
	install, err := posthog.LoadOrCreateInstall(dir)
	requirements.NoError(err)
	requirements.NoError(os.Remove(filepath.Join(dir, posthog.InstallFileName)))
	runWireHelper(t, stub.server.URL, dir, "MSGVAULT_SCREEN_VIEWS_TEST=restart")
	messages = screenMessages()
	requirements.Len(messages, 4)
	assertions.NotEqual(install.ID, messages[3]["distinct_id"])
}

func TestDailyViewsResumeAfterUTCMidnight(t *testing.T) {
	runWireHelper(t, "http://example.invalid", t.TempDir(), "MSGVAULT_SCREEN_VIEWS_TEST=rollover")
}

func exerciseDailyViewRollover(t *testing.T, opts Options) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		stub := &wireStub{}
		stub.server = httptest.NewTestServer(t, http.HandlerFunc(stub.serveHTTP))
		transport := http.DefaultTransport
		http.DefaultTransport = stub.server.Client().Transport
		t.Cleanup(func() { http.DefaultTransport = transport })
		reporter, err := buildReporter(opts, testAPIKey, stub.server.URL, slog.New(slog.DiscardHandler))
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, reporter.Close()) })
		h := CaptureHandler(reporter, opts.DataDir)
		report := func() {
			for _, body := range []string{
				`{"event":"screen_viewed","properties":{"screen":"settings","surface":"web"}}`,
				`{"event":"message_opened","properties":{"surface":"web"}}`,
			} {
				require.Equal(t, http.StatusAccepted, screenRequest(h, body).Code)
			}
		}
		report()
		report()
		time.Sleep(time.Until(time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)))
		report()
		require.NoError(t, reporter.Close())
		counts := map[string]int{}
		for _, event := range batchEvents(t, stub) {
			counts[fmt.Sprint(event["event"])]++
		}
		assert.Equal(t, map[string]int{EventScreenViewed: 2, EventMessageOpened: 2}, counts)
	})
}
