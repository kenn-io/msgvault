package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPlaudSchedulerJobName(t *testing.T) {
	name, ok := SchedulerJobNameForSource("plaud", "work")
	assert.True(t, ok)
	assert.Equal(t, "plaud:work", name)
}

func TestPlaudDaemonCLIAllowlist(t *testing.T) {
	assert := assert.New(t)
	assert.True(cliRunCommandAllowed([]string{"add-plaud", "work"}))
	assert.True(cliRunCommandAllowed([]string{"sync-plaud", "work", "--limit", "5"}))
	assert.True(cliRunCommandAllowed([]string{"sync-plaud", "--probe"}))
	for _, args := range [][]string{{"add-twilio"}, {"add-twilio", "account"}, {"sync-twilio"}, {"sync-twilio", "account", "--full"}} {
		assert.True(cliRunCommandAllowed(args), "Twilio command must be runnable via daemon: %v", args)
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
			secondRun := runs.Add(1) == 2
			startOnce.Do(func() { close(started) })
			select {
			case <-ctx.Done():
				cancelled.Add(1)
			case <-release:
			}
			if secondRun {
				close(second)
			}
			return nil
		},
	}))
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, sched, testLogger())

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

func TestAppleImportSchedulingSelectsOnlyImportedSource(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	local, err := st.GetOrCreateSource("apple_messages", "local")
	require.NoError(err)
	legacy, err := st.GetOrCreateSource("apple_messages", "+15551234567")
	require.NoError(err)
	selected, err := st.GetOrCreateSource("apple_messages", "+15557654321")
	require.NoError(err)
	sched := scheduler.New(func(context.Context, string) error { return nil })
	var runs atomic.Int32
	require.NoError(sched.AddJob(scheduler.Job{Name: IMessageJobName, Schedule: "0 0 1 1 *", Run: func(context.Context) error { runs.Add(1); return nil }}))
	defer func() { <-sched.Stop().Done() }()
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, sched, testLogger())
	for _, source := range []*store.Source{local, legacy} {
		status, err := srv.sourceStatus(context.Background(), st, source)
		require.NoError(err)
		assert.False(status.Scheduled)
		assert.False(status.CanSync)
		assert.Equal("source_not_schedulable", status.SyncUnavailableReason)
		resp := servePOSTTestRequest(srv, "/api/v1/sync/"+source.Identifier+"?source_type=apple_messages")
		assert.Equal(http.StatusBadRequest, resp.Code, resp.Body.String())
	}
	assert.Zero(runs.Load(), "an unselected source must not start the singleton import")
	status, err := srv.sourceStatus(context.Background(), st, selected)
	require.NoError(err)
	assert.True(status.Scheduled)
	assert.True(status.CanSync)
	resp := servePOSTTestRequest(srv, "/api/v1/sync/"+selected.Identifier+"?source_type=apple_messages")
	assert.Equal(http.StatusAccepted, resp.Code, resp.Body.String())
}

func TestTriggerFirstIMessageImportFromEmptyArchive(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	sched := scheduler.New(func(context.Context, string) error { return nil })
	started := make(chan struct{})
	require.NoError(sched.AddJob(scheduler.Job{Name: IMessageJobName, Schedule: "0 0 1 1 *", Run: func(context.Context) error { close(started); return nil }}))
	defer func() { <-sched.Stop().Done() }()
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, sched, testLogger())
	resp := servePOSTTestRequest(srv, "/api/v1/sync/local?source_type=apple_messages")
	require.Equal(http.StatusAccepted, resp.Code, resp.Body.String())
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow("scheduled first import did not start")
	}
	sources, err := st.ListSources("apple_messages")
	require.NoError(err)
	assert.Empty(t, sources, "admission does not create an archive source")
	resp = servePOSTTestRequest(srv, "/api/v1/sync/other?source_type=apple_messages")
	assert.Equal(t, http.StatusBadRequest, resp.Code, resp.Body.String())
}
