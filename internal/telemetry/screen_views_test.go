package telemetry

import (
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
