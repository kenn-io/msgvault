package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
)

func TestCardDAVSchedulerJobNameIsStable(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "carddav", CardDAVJobName)
}

func TestPlaudSchedulerJobName(t *testing.T) {
	name, ok := SchedulerJobNameForSource("plaud", "work")
	assert.True(t, ok)
	assert.Equal(t, "plaud:work", name)
}

func TestPlaudDaemonCLIAllowlist(t *testing.T) {
	assert.True(t, cliRunCommandAllowed([]string{"add-plaud", "work"}))
	assert.True(t, cliRunCommandAllowed([]string{"sync-plaud", "work", "--limit", "5"}))
	assert.True(t, cliRunCommandAllowed([]string{"sync-plaud", "--probe"}))
}

func TestAppleImportSchedulerJobNames(t *testing.T) {
	assert := assert.New(t)
	name, ok := SchedulerJobNameForSource("whatsapp", "+15551234567")
	assert.True(ok)
	assert.Equal(WhatsAppAppleJobName("+15551234567"), name)

	// The iMessage store identifier varies by install; the job is a singleton.
	for _, identifier := range []string{"local", "+15551234567"} {
		name, ok = SchedulerJobNameForSource("apple_messages", identifier)
		assert.True(ok)
		assert.Equal(IMessageJobName, name)
	}
}

// TestTriggerAppleImportRunsDetachedFromRequest proves that cancelling the
// triggering HTTP request does not cancel the daemon-owned run, and that
// repeated triggers while it runs start no second run.
func TestTriggerAppleImportRunsDetachedFromRequest(t *testing.T) {
	require := require.New(t)
	sched := scheduler.New(func(context.Context, string) error { return nil })
	started := make(chan struct{})
	release := make(chan struct{})
	second := make(chan struct{})
	var runs, cancelled atomic.Int32
	var startOnce sync.Once
	require.NoError(sched.AddJob(scheduler.Job{
		Name:     IMessageJobName,
		Schedule: "0 0 1 1 *",
		Run: func(ctx context.Context) error {
			if runs.Add(1) == 2 {
				close(second)
			}
			startOnce.Do(func() { close(started) })
			select {
			case <-ctx.Done():
				cancelled.Add(1)
			case <-release:
			}
			return nil
		},
	}))
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, nil, sched, testLogger())

	reqCtx, cancelReq := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/local?source_type=apple_messages", nil).WithContext(reqCtx)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(http.StatusAccepted, resp.Code, resp.Body.String())
	<-started
	cancelReq()

	for range 5 {
		resp = servePOSTTestRequest(srv, "/api/v1/sync/local?source_type=apple_messages")
		require.Equal(http.StatusAccepted, resp.Code, resp.Body.String())
	}
	require.Equal(int32(1), runs.Load(), "triggers during a run must not start a second concurrent run")
	close(release)
	<-second
	<-sched.Stop().Done()
	require.Zero(cancelled.Load(), "run must outlive the triggering request")
	require.Equal(int32(2), runs.Load(), "the five triggers coalesce into exactly one follow-up run")
}
