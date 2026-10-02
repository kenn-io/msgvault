package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCardDAVStatusUnconfiguredRemainsReadable(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	controller := &CardDAVController{cfg: cfg, store: testutil.NewTestStore(t)}
	srv := NewServerWithOptions(ServerOptions{
		Config: cfg, Store: &mockStore{}, CardDAV: controller, Logger: testLogger(),
	})

	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/v1/carddav/status", nil))
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var status CardDAVStatusResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&status))
	assertions.False(status.Configured)
	assertions.False(status.Available)
	assertions.False(status.CredentialConfigured)
	assertions.False(status.Enabled)
	assertions.False(status.Scheduled)
	assertions.Nil(status.Account)
	assertions.Empty(status.RepairReason)
	assertions.Nil(status.Active)
	assertions.Nil(status.Latest)
	assertions.Nil(status.LatestSuccessful)
	assertions.NotContains(resp.Body.String(), "password")
}

func TestCardDAVStatusPreservesIncompleteSavedEnablementAndRuntimeAvailability(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.CardDAV = config.CardDAVConfig{BaseURL: "https://contacts.example/dav", Enabled: true, Schedule: "0 3 * * *"}
	controller := &CardDAVController{
		cfg: cfg, store: testutil.NewTestStore(t), service: cardDAVListFixture{},
	}

	resp := getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, nil), "/api/v1/carddav/status")
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var status CardDAVStatusResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&status))
	assertions.False(status.Configured)
	assertions.True(status.Enabled)
	assertions.True(status.Available)
	assertions.Equal("0 3 * * *", status.Schedule)
	assertions.Nil(status.Account)
	assertions.Empty(status.RepairReason)
}

func cardDAVReadServer(t *testing.T, cfg *config.Config, controller *CardDAVController, sched SyncScheduler) *Server {
	t.Helper()
	return NewServerWithOptions(ServerOptions{
		Config: cfg, Store: &mockStore{}, CardDAV: controller, Scheduler: sched, Logger: testLogger(),
	})
}

func getCardDAVRead(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
	return resp
}

func TestCardDAVStatusReportsStableCredentialRepairReasons(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		seedAccount bool
		load        func(string) (carddav.Credential, error)
		want        string
	}{
		{name: "account missing", load: func(string) (carddav.Credential, error) {
			return carddav.Credential{}, errors.New("must not inspect credential before account")
		}, want: "account_missing"},
		{name: "credential missing", seedAccount: true, load: func(string) (carddav.Credential, error) {
			return carddav.Credential{}, os.ErrNotExist
		}, want: "credential_missing"},
		{name: "credential mismatch", seedAccount: true, load: func(string) (carddav.Credential, error) {
			return carddav.Credential{BaseURL: "https://other.example/dav", Username: "alice", ConnectionGeneration: 1}, nil
		}, want: "credential_mismatch"},
		{name: "credential unavailable", seedAccount: true, load: func(string) (carddav.Credential, error) {
			return carddav.Credential{}, errors.New("permission denied Authorization: synthetic-secret")
		}, want: "credential_unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assertions := assert.New(t)
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			cfg.CardDAV = config.CardDAVConfig{BaseURL: "https://contacts.example/dav", Username: "alice"}
			st := testutil.NewTestStore(t)
			if tt.seedAccount {
				_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
					BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username,
					PrincipalURL: "https://contacts.example/principal/", HomeURL: "https://contacts.example/books/",
				})
				require.NoError(err)
			}
			controller := &CardDAVController{cfg: cfg, store: st, loadCredential: tt.load}
			resp := getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, nil), "/api/v1/carddav/status")
			require.Equal(http.StatusOK, resp.Code, resp.Body.String())
			var status CardDAVStatusResponse
			require.NoError(json.NewDecoder(resp.Body).Decode(&status))
			assertions.Equal(tt.want, status.RepairReason)
			assertions.NotContains(resp.Body.String(), "synthetic-secret")
			assertions.NotContains(resp.Body.String(), "permission denied")
		})
	}
}

func TestCardDAVStatusRedactsSavedAccountURLSecrets(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.CardDAV = config.CardDAVConfig{
		BaseURL:  "https://alice:synthetic-password@contacts.example/dav?access_token=synthetic-query#private-fragment",
		Username: "alice",
	}
	controller := &CardDAVController{cfg: cfg, store: testutil.NewTestStore(t)}

	resp := getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, nil), "/api/v1/carddav/status")
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var status CardDAVStatusResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&status))
	require.NotNil(status.Account)
	assertions.Equal("https://contacts.example/dav", status.Account.BaseURL)
	for _, private := range []string{"synthetic-password", "synthetic-query", "private-fragment", "access_token"} {
		assertions.NotContains(resp.Body.String(), private)
	}
}

func TestNewCardDAVControllerKeepsUnreadableCredentialAvailableForRepairStatus(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assertions := assert.New(t)
	cfg, st, _ := savedCardDAVFixture(t)
	credentialPath := filepath.Join(cfg.TokensDir(), "carddav.json")
	require.NoError(os.Remove(credentialPath))
	require.NoError(os.Mkdir(credentialPath, 0o700))

	var logs bytes.Buffer
	controller, err := NewCardDAVController(cfg, st, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(err)
	assertions.Nil(controller.Current())
	assertions.Contains(logs.String(), "level=WARN")
	assertions.Contains(logs.String(), "CardDAV credential is unreadable")
	status, err := controller.Status(t.Context(), "")
	require.NoError(err)
	assertions.Equal("credential_unavailable", status.RepairReason)
	assertions.False(status.CredentialConfigured)
}

func TestCardDAVStatusSeparatesRuntimeEnablementAndMatchingSchedule(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assertions := assert.New(t)
	cfg, st, service := savedCardDAVFixture(t)
	cfg.CardDAV.Enabled = false
	next := time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC)
	controller := &CardDAVController{
		cfg: cfg, store: st, service: service, loadCredential: carddav.LoadCredential,
	}
	sched := newMockScheduler()
	sched.jobStatuses = []JobStatus{
		{Name: "unrelated", Schedule: cfg.CardDAV.Schedule, NextRun: next.Add(-time.Hour)},
		{Name: CardDAVJobName, Schedule: "stale-runtime-copy", NextRun: next},
	}

	resp := getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, sched), "/api/v1/carddav/status")
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var status CardDAVStatusResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&status))
	assertions.True(status.Configured)
	assertions.True(status.Available)
	assertions.True(status.CredentialConfigured)
	assertions.False(status.Enabled)
	assertions.True(status.Scheduled)
	assertions.Equal(cfg.CardDAV.Schedule, status.Schedule)
	require.NotNil(status.NextScheduledAt)
	assertions.Equal(next, *status.NextScheduledAt)
	assertions.Empty(status.RepairReason)

	controller.service = nil
	resp = getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, nil), "/api/v1/carddav/status")
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())
	require.NoError(json.NewDecoder(resp.Body).Decode(&status))
	assertions.False(status.Available)
	assertions.True(status.CredentialConfigured)
	assertions.Equal("runtime_unavailable", status.RepairReason)
}

func TestCardDAVStatusIgnoresUnavailableAndUnrelatedSchedulers(t *testing.T) {
	t.Parallel()
	cfg, st, service := savedCardDAVFixture(t)
	controller := &CardDAVController{cfg: cfg, store: st, service: service, loadCredential: carddav.LoadCredential}
	next := time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC)
	stopped := newMockScheduler()
	stopped.running = false
	stopped.jobStatuses = []JobStatus{{Name: CardDAVJobName, NextRun: next}}
	unrelated := newMockScheduler()
	unrelated.jobStatuses = []JobStatus{{Name: "unrelated", NextRun: next}}
	for name, sched := range map[string]SyncScheduler{"nil": nil, "stopped": stopped, "unrelated": unrelated} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assertions := assert.New(t)
			resp := getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, sched), "/api/v1/carddav/status")
			require.Equal(http.StatusOK, resp.Code, resp.Body.String())
			var status CardDAVStatusResponse
			require.NoError(json.NewDecoder(resp.Body).Decode(&status))
			assertions.False(status.Scheduled)
			assertions.Nil(status.NextScheduledAt)
		})
	}
}

func TestCardDAVStatusProjectsLatestFailureAndSuccessfulRun(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewTestStore(t)
	success, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{AccountID: store.DefaultCardDAVAccountID, Trigger: store.CardDAVSyncTriggerManual, Full: true})
	require.NoError(err)
	_, err = st.FinishCardDAVSyncRunContext(t.Context(), success.ID, store.CardDAVSyncRunFinish{
		State: store.CardDAVSyncRunSucceeded, Books: 2, Created: 3, Updated: 4, Removed: 5,
	})
	require.NoError(err)
	failed, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{AccountID: store.DefaultCardDAVAccountID, Trigger: store.CardDAVSyncTriggerScheduled})
	require.NoError(err)
	_, err = st.FinishCardDAVSyncRunContext(t.Context(), failed.ID, store.CardDAVSyncRunFinish{
		State: store.CardDAVSyncRunFailed, Books: 1, ErrorCode: "upstream_failed", ErrorMessage: "CardDAV server request failed.",
	})
	require.NoError(err)
	controller := &CardDAVController{cfg: cfg, store: st}

	resp := getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, nil), "/api/v1/carddav/status")
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var status CardDAVStatusResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&status))
	require.NotNil(status.Latest)
	assertions.Equal(failed.ID, status.Latest.ID)
	assertions.Equal("failed", status.Latest.State)
	assertions.Equal("upstream_failed", status.Latest.ErrorCode)
	require.NotNil(status.LatestSuccessful)
	assertions.Equal(success.ID, status.LatestSuccessful.ID)
	assertions.Equal(int64(2), status.LatestSuccessful.Books)
	assertions.Equal(int64(3), status.LatestSuccessful.Created)
	assertions.NotContains(resp.Body.String(), "connection_generation")
}

func TestCardDAVStatusAndRunsCollapseUnknownStoredFailureProjection(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewTestStore(t)
	run, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{AccountID: store.DefaultCardDAVAccountID, Trigger: store.CardDAVSyncTriggerManual})
	require.NoError(err)
	_, err = st.FinishCardDAVSyncRunContext(t.Context(), run.ID, store.CardDAVSyncRunFinish{
		State: store.CardDAVSyncRunFailed, ErrorCode: "future_provider_failure",
		ErrorMessage: "tenant-internal-marker must not cross the API",
	})
	require.NoError(err)
	controller := &CardDAVController{cfg: cfg, store: st}
	srv := cardDAVReadServer(t, cfg, controller, nil)

	for _, path := range []string{"/api/v1/carddav/status", "/api/v1/carddav/runs"} {
		resp := getCardDAVRead(t, srv, path)
		require.Equal(http.StatusOK, resp.Code, resp.Body.String())
		assertions.Contains(resp.Body.String(), `"error_code":"sync_failed"`)
		assertions.Contains(resp.Body.String(), `"error_message":"CardDAV sync failed."`)
		assertions.NotContains(resp.Body.String(), "future_provider_failure")
		assertions.NotContains(resp.Body.String(), "tenant-internal-marker")
	}
}

func TestCardDAVStatusProjectsActiveRunExactly(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewTestStore(t)
	active, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{
		AccountID: store.DefaultCardDAVAccountID,
		Trigger:   store.CardDAVSyncTriggerScheduled, Full: true,
	})
	require.NoError(err)
	controller := &CardDAVController{cfg: cfg, store: st}

	resp := getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, nil), "/api/v1/carddav/status")
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var status CardDAVStatusResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&status))
	require.NotNil(status.Active)
	assertions.Equal(active.ID, status.Active.ID)
	assertions.Equal("scheduled", status.Active.Trigger)
	assertions.True(status.Active.Full)
	assertions.Equal("running", status.Active.State)
	assertions.Nil(status.Active.FinishedAt)
	require.NotNil(status.Latest)
	assertions.Equal(active.ID, status.Latest.ID)
	assertions.Nil(status.LatestSuccessful)
}

func TestCardDAVRunHistoryPagesNewestFirstWithoutRuntimeService(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewTestStore(t)
	var ids []int64
	for i := range 3 {
		run, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{AccountID: store.DefaultCardDAVAccountID, Trigger: store.CardDAVSyncTriggerManual, Full: i == 0})
		require.NoError(err)
		ids = append(ids, run.ID)
		_, err = st.FinishCardDAVSyncRunContext(t.Context(), run.ID, store.CardDAVSyncRunFinish{State: store.CardDAVSyncRunSucceeded, Books: int64(i + 1)})
		require.NoError(err)
	}
	controller := &CardDAVController{cfg: cfg, store: st}
	srv := cardDAVReadServer(t, cfg, controller, nil)

	first := getCardDAVRead(t, srv, "/api/v1/carddav/runs?limit=2")
	require.Equal(http.StatusOK, first.Code, first.Body.String())
	var firstPage CardDAVRunsResponse
	require.NoError(json.NewDecoder(first.Body).Decode(&firstPage))
	require.Len(firstPage.Runs, 2)
	assertions.Equal([]int64{ids[2], ids[1]}, []int64{firstPage.Runs[0].ID, firstPage.Runs[1].ID})
	require.NotNil(firstPage.NextBeforeID)
	assertions.Equal(ids[1], *firstPage.NextBeforeID)

	second := getCardDAVRead(t, srv, "/api/v1/carddav/runs?limit=2&before_id="+strconv.FormatInt(*firstPage.NextBeforeID, 10))
	require.Equal(http.StatusOK, second.Code, second.Body.String())
	var secondPage CardDAVRunsResponse
	require.NoError(json.NewDecoder(second.Body).Decode(&secondPage))
	require.Len(secondPage.Runs, 1)
	assertions.Equal(ids[0], secondPage.Runs[0].ID)
	assertions.Nil(secondPage.NextBeforeID)
	assertions.NotContains(first.Body.String(), "connection_generation")
}

func TestCardDAVRunHistoryRejectsInvalidPagination(t *testing.T) {
	t.Parallel()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	controller := &CardDAVController{cfg: cfg, store: testutil.NewTestStore(t)}
	srv := cardDAVReadServer(t, cfg, controller, nil)
	for _, query := range []string{"limit=0", "limit=101", "limit=bad", "before_id=0", "before_id=bad"} {
		resp := getCardDAVRead(t, srv, "/api/v1/carddav/runs?"+query)
		assert.Equal(t, http.StatusBadRequest, resp.Code, query+": "+resp.Body.String())
	}
}

func TestCardDAVStatusAndRunsMapMissingDependenciesAndStorageFailureSafely(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	for _, path := range []string{"/api/v1/carddav/status", "/api/v1/carddav/runs"} {
		missing := getCardDAVRead(t, cardDAVReadServer(t, cfg, nil, nil), path)
		assertions.Equal(http.StatusServiceUnavailable, missing.Code, path+": "+missing.Body.String())
		missingStore := getCardDAVRead(t, cardDAVReadServer(t, cfg, &CardDAVController{cfg: cfg}, nil), path)
		assertions.Equal(http.StatusServiceUnavailable, missingStore.Code, path+": "+missingStore.Body.String())
	}

	st := testutil.NewTestStore(t)
	_, err := st.DB().Exec(`DROP TABLE carddav_sync_runs`)
	require.NoError(t, err)
	controller := &CardDAVController{cfg: cfg, store: st}
	for _, path := range []string{"/api/v1/carddav/status", "/api/v1/carddav/runs"} {
		failed := getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, nil), path)
		assertions.Equal(http.StatusInternalServerError, failed.Code, path+": "+failed.Body.String())
		assertions.NotContains(failed.Body.String(), "no such table")
		assertions.NotContains(failed.Body.String(), "carddav_sync_runs")
	}
}

func TestCardDAVStatusReportsSelectedAndAggregateSchedules(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.CardDAVConnections = map[string]config.CardDAVConfig{"work": {Enabled: true}, "personal": {Enabled: true}}
	controller := &CardDAVController{cfg: cfg, store: testutil.NewTestStore(t)}
	next := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	sched := newMockScheduler()
	sched.jobStatuses = []JobStatus{{Name: "carddav:work", NextRun: next}, {Name: "carddav:personal", NextRun: next.Add(time.Hour)}, {Name: "unrelated", NextRun: next.Add(-time.Hour)}}
	server := cardDAVReadServer(t, cfg, controller, sched)
	for _, tc := range []struct {
		query     string
		scheduled bool
		next      time.Time
	}{
		{"", true, next}, {"?connection=work", true, next}, {"?connection=personal", true, next.Add(time.Hour)}, {"?connection=default", false, time.Time{}},
	} {
		response := getCardDAVRead(t, server, "/api/v1/carddav/status"+tc.query)
		require.Equal(http.StatusOK, response.Code, response.Body.String())
		var status CardDAVStatusResponse
		require.NoError(json.NewDecoder(response.Body).Decode(&status))
		assertions.Equal(tc.scheduled, status.Scheduled)
		if tc.scheduled {
			require.NotNil(status.NextScheduledAt)
			assertions.Equal(tc.next, *status.NextScheduledAt)
		} else {
			assertions.Nil(status.NextScheduledAt)
		}
	}
}
