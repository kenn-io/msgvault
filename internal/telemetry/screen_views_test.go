package telemetry

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
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
	if os.Getenv("MSGVAULT_SCREEN_VIEWS_TEST") == "restart" {
		day = day.Add(2 * time.Minute)
		assertions.Equal(http.StatusAccepted, screenRequest(h, valid).Code)
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
		assertions.Equal(http.StatusAccepted, screenRequest(h, valid).Code)
		assertions.NoFileExists(filepath.Join(opts.DataDir, screenViewsFile))
		return
	}
	for _, body := range []string{
		`{"event":"screen_viewed","properties":{"screen":"private","surface":"web"}}`,
		`{"event":"screen_viewed","properties":{"screen":"settings","surface":"private"}}`,
		`{"event":"screen_viewed","properties":{"screen":"settings"}}`,
	} {
		assertions.Equal(http.StatusAccepted, screenRequest(h, body).Code)
		assertions.NoFileExists(filepath.Join(opts.DataDir, screenViewsFile))
	}
	var group sync.WaitGroup
	for i := range 20 {
		group.Go(func() {
			surface := "web"
			if i%2 == 1 {
				surface = "tui"
			}
			assertions.Equal(http.StatusAccepted, screenRequest(h, fmt.Sprintf(`{"event":"screen_viewed","properties":{"screen":"settings","surface":%q}}`, surface)).Code)
		})
	}
	group.Wait()
	assertions.Equal(http.StatusBadRequest, screenRequest(h, valid+`{}`).Code)
	assertions.Equal(http.StatusAccepted, screenRequest(h, `{"Event":"unknown","event":"screen_viewed","Properties":{"screen":"private","screen":"settings","surface":"web"}}`).Code)
	assertions.Equal(http.StatusRequestEntityTooLarge, screenRequest(h, strings.Repeat(" ", 64<<10)+valid).Code)
	assertions.Equal(http.StatusAccepted, screenRequest(h, `{"event":" screen_viewed ","properties":{" screen ":"directory","surface":"tui","content":"private"}}`).Code)
	conflicting := `{"event":"screen_viewed","properties":{"screen":"email"," screen ":"texts","surface":"web"}}`
	assertions.Equal(http.StatusAccepted, screenRequest(h, conflicting).Code)
	day = day.Add(2 * time.Minute)
	claimsPath := filepath.Join(opts.DataDir, screenViewsFile)
	previous, err := os.ReadFile(claimsPath)
	requirements.NoError(err)
	requirements.NoError(os.Remove(claimsPath))
	requirements.NoError(os.Mkdir(claimsPath, 0o700))
	assertions.Equal(http.StatusInternalServerError, screenRequest(h, valid).Code)
	requirements.NoError(os.Remove(claimsPath))
	requirements.NoError(os.WriteFile(claimsPath, previous, 0o600))
	assertions.Equal(http.StatusAccepted, screenRequest(h, valid).Code)
}

func TestScreenViewsCountAcrossSurfacesAndRestarts(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	stub := newWireStub(t)
	dir := t.TempDir()
	runWireHelper(t, stub.server.URL, dir, "MSGVAULT_SCREEN_VIEWS_TEST=1")
	screenMessages := func() []map[string]any {
		return slices.DeleteFunc(batchEvents(t, stub), func(event map[string]any) bool { return event["event"] != EventScreenViewed })
	}
	requirements.Len(batchEvents(t, stub), 7)
	messages := screenMessages()
	requirements.Len(messages, 4)
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
	assertions.Equal(2, strings.Count(strings.Join(screens, ","), "settings"))
	runWireHelper(t, stub.server.URL, dir, "MSGVAULT_SCREEN_VIEWS_TEST=restart")
	assertions.Len(screenMessages(), 4)
	install, err := posthog.LoadOrCreateInstall(dir)
	requirements.NoError(err)
	requirements.NoError(os.Remove(filepath.Join(dir, posthog.InstallFileName)))
	runWireHelper(t, stub.server.URL, dir, "MSGVAULT_SCREEN_VIEWS_TEST=restart")
	messages = screenMessages()
	requirements.Len(messages, 5)
	assertions.NotEqual(install.ID, messages[4]["distinct_id"])
}

func exerciseActions(t *testing.T, reporter *posthog.Reporter, opts Options) {
	t.Helper()
	assert, require := assert.New(t), require.New(t)
	h, ok := CaptureHandler(reporter, opts.DataDir).(*screenCaptureHandler)
	require.True(ok)
	valid := `{"event":"search_run","properties":{"surface":"cli","search_type":"text"}}`
	if !reporter.Enabled() {
		assert.Equal(http.StatusAccepted, screenRequest(h, valid).Code)
		assert.NoFileExists(filepath.Join(opts.DataDir, screenViewsFile))
		return
	}
	if os.Getenv("MSGVAULT_ACTIONS_TEST") == "restart" {
		h.now = func() time.Time { return time.Date(2026, 1, 2, 23, 59, 0, 0, time.UTC) }
		assert.Equal(http.StatusAccepted, screenRequest(h, valid).Code)
		h.now = func() time.Time { return time.Date(2026, 1, 3, 0, 1, 0, 0, time.UTC) }
		assert.Equal(http.StatusAccepted, screenRequest(h, valid).Code)
		return
	}
	browser, _ := reporter.SanitizeProperties(EventSyncFinished, map[string]any{propertySurface: "web"})
	assert.Equal("web", browser[propertySurface])
	day := time.Date(2026, 1, 2, 23, 59, 0, 0, time.UTC)
	h.now = func() time.Time { return day }
	for _, event := range []string{EventSearchRun, EventMessageOpened, EventSyncFinished} {
		for _, props := range []string{`{}`, `{"surface":"other"}`, `{"surface":42}`} {
			assert.Equal(http.StatusAccepted, screenRequest(h, fmt.Sprintf(`{"event":%q,"properties":%s}`, event, props)).Code)
		}
	}
	for _, value := range []string{`null`, `"other"`, `42`} {
		assert.Equal(http.StatusAccepted, screenRequest(h, fmt.Sprintf(`{"event":"search_run","properties":{"surface":"web","search_type":%s}}`, value)).Code)
	}
	assert.Equal(http.StatusAccepted, screenRequest(h, `{"event":"search_run","properties":{"surface":"web"}}`).Code)
	for _, surface := range []string{"web", "tui", "cli"} {
		for _, kind := range []string{"text", "semantic", "hybrid"} {
			body := fmt.Sprintf(`{"event":"search_run","properties":{"surface":%q,"search_type":%q,"query":"private"}}`, surface, kind)
			for range 2 {
				assert.Equal(http.StatusAccepted, screenRequest(h, body).Code)
			}
		}
	}
	capture := h.captureEvent
	h.captureEvent = func(string, map[string]any) error { return errors.New("queue full") }
	path := filepath.Join(opts.DataDir, screenViewsFile)
	previous, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(http.StatusInternalServerError, screenRequest(h, `{"event":"message_opened","properties":{"surface":"web"}}`).Code)
	restored, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(previous, restored, "failed capture rolls the daily claim back")
	h.captureEvent = capture
	for _, surface := range []string{"web", "tui", "cli"} {
		for _, event := range []string{EventMessageOpened, EventSyncFinished} {
			for range 2 {
				assert.Equal(http.StatusAccepted, screenRequest(h, fmt.Sprintf(`{"event":%q,"properties":{"surface":%q,"message_id":1}}`, event, surface)).Code)
			}
		}
	}
}
func TestCoreActionDailyClaimsAndValidation(t *testing.T) {
	assert, require, stub := assert.New(t), require.New(t), newWireStub(t)
	dir := t.TempDir()
	runWireHelper(t, stub.server.URL, dir, "MSGVAULT_ACTIONS_TEST=1", wireConfigOffEnv+"=1")
	runWireHelper(t, stub.server.URL, dir, "MSGVAULT_ACTIONS_TEST=1")
	runWireHelper(t, stub.server.URL, dir, "MSGVAULT_ACTIONS_TEST=restart")
	events := batchEvents(t, stub)
	require.Len(events, 15)
	counts := map[string]int{}
	for _, event := range events {
		props, ok := event["properties"].(map[string]any)
		require.True(ok)
		assert.NotContains(props, "query")
		assert.NotContains(props, "message_id")
		counts[fmt.Sprint(event["event"], ":", props["surface"], ":", props["search_type"])]++
	}
	assert.Equal(2, counts["search_run:cli:text"])
	assert.Equal(2, counts["sync_finished:cli:<nil>"], "successful CLI syncs count individually")
	assert.Len(counts, 13)
}
