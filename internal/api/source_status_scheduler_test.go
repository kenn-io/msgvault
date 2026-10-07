package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func sourceStatusFor(t *testing.T, srv *Server) SourceStatus {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sources/status", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response SourceStatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Len(t, response.Sources, 1)
	return response.Sources[0]
}

func TestHandleSourceStatusReportsGenericJobQueueState(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	started := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	sched := newMockScheduler()
	sched.jobStatuses = []JobStatus{{
		Name: BeeperJobName, Schedule: "*/5 * * * *", Running: true,
		Pending: true, StartedAt: started,
	}}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, sched, testLogger())

	status := sourceStatusFor(t, srv)
	assert.False(status.SchedulerQueued)
	assert.True(status.SchedulerPending, "a tick during the run is visible")
	require.NotNil(status.SchedulerStartedAt)
	assert.Equal(started.Format(time.RFC3339), *status.SchedulerStartedAt)
}

func TestHandleSourceStatusReportsQueuedAccountSync(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("gmail", "queued@example.com")
	require.NoError(err)
	sched := newMockScheduler()
	sched.scheduled["queued@example.com"] = true
	sched.statuses = []AccountStatus{{
		Email: "queued@example.com", Running: true, Queued: true, Schedule: "*/5 * * * *",
	}}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, sched, testLogger())

	status := sourceStatusFor(t, srv)
	assert.True(status.SchedulerQueued, "a sync waiting for another job is visible")
	assert.False(status.SchedulerPending)
	assert.Nil(status.SchedulerStartedAt)
}

func TestHandleSourceStatusReportsRunOutcome(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("whatsapp", "+15555550100")
	require.NoError(err)
	readStarted := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	mtime := readStarted.Add(-3 * time.Hour)
	alive := false

	syncID, err := st.StartSync(src.ID, "whatsapp_apple_import")
	require.NoError(err)
	require.NoError(st.CompleteSync(syncID, ""))
	require.NoError(st.SetSyncMeasurement(t.Context(), syncID, store.SyncMeasurement{
		ReadStartedAt: &readStarted, SourceMtime: &mtime, WriterAlive: &alive,
	}.ClassifyOutcome()))
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, newMockScheduler(), testLogger())

	run := sourceStatusFor(t, srv).LatestSync
	require.NotNil(run)
	assert.Equal("completed", run.Status)
	assert.Equal("unmeasured", run.Outcome, "a completed run with a stopped writer is not a zero")
	assert.Equal("writer_not_running", run.Reason)
	assert.Equal(readStarted.Format(time.RFC3339), *run.ReadStartedAt)
	assert.Equal(mtime.Format(time.RFC3339), *run.SourceMtime)
	require.NotNil(run.WriterAlive)
	assert.False(*run.WriterAlive)
}

func TestHandleSourceStatusOutcomeDefaultsToRunStatus(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "plain@example.com")
	require.NoError(err)
	syncID, err := st.StartSync(src.ID, "")
	require.NoError(err)
	require.NoError(st.FailSync(syncID, "boom"))
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, newMockScheduler(), testLogger())

	run := sourceStatusFor(t, srv).LatestSync
	require.NotNil(run)
	assert.Equal(t, "failed", run.Outcome)
}
