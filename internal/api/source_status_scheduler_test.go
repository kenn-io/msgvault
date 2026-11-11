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
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/sources/status", nil)
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

func TestSourceStatusReportsAliasSchedule(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "provider@example.test")
	requirements.NoError(err)
	alias := "archive@example.test"
	_, err = st.UpdateSourceSettingsContext(t.Context(), source.ID, store.SourceSettingsUpdate{Alias: &alias})
	requirements.NoError(err)
	sched := newMockScheduler()
	sched.statuses = []AccountStatus{{Email: alias, Schedule: "0 * * * *", NextRun: time.Now().Add(time.Minute)}}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, sched, testLogger())
	status := sourceStatusFor(t, srv)
	checks.True(status.Scheduled)
	checks.True(status.CanSync)
	checks.NotNil(status.NextSyncAt)
}

func TestTriggerSyncUsesArchiveAliasSchedule(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "provider@example.test")
	requirements.NoError(err)
	alias := "archive@example.test"
	_, err = st.UpdateSourceSettingsContext(t.Context(), source.ID, store.SourceSettingsUpdate{Alias: &alias})
	requirements.NoError(err)
	sched := newMockScheduler()
	sched.scheduled[alias] = true
	sched.statuses = []AccountStatus{{Email: alias, Schedule: "0 * * * *"}}
	var triggered string
	sched.triggerFn = func(email string, _ bool) error { triggered = email; return nil }
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, sched, testLogger())
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/sync/provider@example.test?source_type=gmail", nil)
	req.SetPathValue("account", source.Identifier)
	response := httptest.NewRecorder()
	srv.handleTriggerSync(response, req)
	checks.Equal(http.StatusAccepted, response.Code, response.Body.String())
	checks.Equal(alias, triggered)
}

func TestSourceStatusAndTriggerUseCaseInsensitiveAccount(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "reader@example.test")
	requirements.NoError(err)
	alias := "archive@example.test"
	_, err = st.UpdateSourceSettingsContext(t.Context(), source.ID, store.SourceSettingsUpdate{Alias: &alias})
	requirements.NoError(err)
	account := "READER@example.test"
	sched := newMockScheduler()
	sched.scheduled[account], sched.scheduled[alias] = true, true
	sched.statuses = []AccountStatus{{Email: alias, Schedule: "0 2 * * *"}, {Email: account, Schedule: "0 * * * *", NextRun: time.Now().Add(time.Minute)}}
	var triggered string
	sched.triggerFn = func(email string, _ bool) error { triggered = email; return nil }
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, sched, testLogger())
	status := sourceStatusFor(t, srv)
	checks.True(status.Scheduled)
	checks.True(status.CanSync)
	checks.NotNil(status.NextSyncAt)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/sync/reader@example.test?source_type=gmail", nil)
	req.SetPathValue("account", source.Identifier)
	response := httptest.NewRecorder()
	srv.handleTriggerSync(response, req)
	checks.Equal(http.StatusAccepted, response.Code, response.Body.String())
	checks.Equal(account, triggered)
}

func TestSourceStatusAndTriggerIgnoreNonMailAliasCollision(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "provider@example.test")
	requirements.NoError(err)
	alias := "archive@example.test"
	_, err = st.UpdateSourceSettingsContext(t.Context(), source.ID, store.SourceSettingsUpdate{Alias: &alias})
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("slack", "workspace")
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET display_name = ? WHERE id = ?`), alias, other.ID)
	requirements.NoError(err)
	sched := newMockScheduler()
	sched.scheduled[alias] = true
	sched.statuses = []AccountStatus{{Email: alias, Schedule: "0 * * * *", NextRun: time.Now().Add(time.Minute)}}
	var triggered string
	sched.triggerFn = func(email string, _ bool) error { triggered = email; return nil }
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, sched, testLogger())
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/sources/status", nil)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, req)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var result SourceStatusResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	found := false
	for _, status := range result.Sources {
		if status.ID == source.ID {
			found = true
			checks.True(status.Scheduled)
			checks.True(status.CanSync)
			checks.NotNil(status.NextSyncAt)
		}
	}
	requirements.True(found)
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/sync/provider@example.test?source_type=gmail", nil)
	req.SetPathValue("account", source.Identifier)
	response = httptest.NewRecorder()
	srv.handleTriggerSync(response, req)
	checks.Equal(http.StatusAccepted, response.Code, response.Body.String())
	checks.Equal(alias, triggered)
}
