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
	require := require.New(t)
	assert := assert.New(t)
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
	assert.False(status.Configured)
	assert.False(status.Available)
	assert.False(status.CredentialConfigured)
	assert.False(status.Enabled)
	assert.False(status.Scheduled)
	assert.Nil(status.Account)
	assert.Empty(status.RepairReason)
	assert.Nil(status.Active)
	assert.Nil(status.Latest)
	assert.Nil(status.LatestSuccessful)
	assert.NotContains(resp.Body.String(), "password")
}

func TestCardDAVStatusPreservesIncompleteSavedEnablementAndRuntimeAvailability(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	assert.False(status.Configured)
	assert.True(status.Enabled)
	assert.True(status.Available)
	assert.Equal("0 3 * * *", status.Schedule)
	assert.Nil(status.Account)
	assert.Empty(status.RepairReason)
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
			assert := assert.New(t)
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
			assert.Equal(tt.want, status.RepairReason)
			assert.NotContains(resp.Body.String(), "synthetic-secret")
			assert.NotContains(resp.Body.String(), "permission denied")
		})
	}
}

func TestCardDAVStatusRedactsSavedAccountURLSecrets(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	assert.Equal("https://contacts.example/dav", status.Account.BaseURL)
	for _, private := range []string{"synthetic-password", "synthetic-query", "private-fragment", "access_token"} {
		assert.NotContains(resp.Body.String(), private)
	}
}

func TestNewCardDAVControllerKeepsUnreadableCredentialAvailableForRepairStatus(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg, st, _ := savedCardDAVFixture(t)
	credentialPath := filepath.Join(cfg.TokensDir(), "carddav.json")
	require.NoError(os.Remove(credentialPath))
	require.NoError(os.Mkdir(credentialPath, 0o700))

	var logs bytes.Buffer
	controller, err := NewCardDAVController(cfg, st, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(err)
	assert.Nil(controller.Current())
	assert.Contains(logs.String(), "level=WARN")
	assert.Contains(logs.String(), "CardDAV credential is unreadable")
	status, err := controller.Status(t.Context())
	require.NoError(err)
	assert.Equal("credential_unavailable", status.RepairReason)
	assert.False(status.CredentialConfigured)
}

func TestCardDAVStatusSeparatesRuntimeEnablementAndMatchingSchedule(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	assert.True(status.Configured)
	assert.True(status.Available)
	assert.True(status.CredentialConfigured)
	assert.False(status.Enabled)
	assert.True(status.Scheduled)
	assert.Equal(cfg.CardDAV.Schedule, status.Schedule)
	require.NotNil(status.NextScheduledAt)
	assert.Equal(next, *status.NextScheduledAt)
	assert.Empty(status.RepairReason)

	controller.service = nil
	resp = getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, nil), "/api/v1/carddav/status")
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())
	require.NoError(json.NewDecoder(resp.Body).Decode(&status))
	assert.False(status.Available)
	assert.True(status.CredentialConfigured)
	assert.Equal("runtime_unavailable", status.RepairReason)
}

func TestCardDAVStatusIgnoresUnavailableAndUnrelatedSchedulers(t *testing.T) {
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
			assert := assert.New(t)
			resp := getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, sched), "/api/v1/carddav/status")
			require.Equal(http.StatusOK, resp.Code, resp.Body.String())
			var status CardDAVStatusResponse
			require.NoError(json.NewDecoder(resp.Body).Decode(&status))
			assert.False(status.Scheduled)
			assert.Nil(status.NextScheduledAt)
		})
	}
}

func TestCardDAVStatusProjectsLatestFailureAndSuccessfulRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewTestStore(t)
	success, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual, Full: true})
	require.NoError(err)
	_, err = st.FinishCardDAVSyncRunContext(t.Context(), success.ID, store.CardDAVSyncRunFinish{
		State: store.CardDAVSyncRunSucceeded, Books: 2, Created: 3, Updated: 4, Removed: 5,
	})
	require.NoError(err)
	failed, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerScheduled})
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
	assert.Equal(failed.ID, status.Latest.ID)
	assert.Equal("failed", status.Latest.State)
	assert.Equal("upstream_failed", status.Latest.ErrorCode)
	require.NotNil(status.LatestSuccessful)
	assert.Equal(success.ID, status.LatestSuccessful.ID)
	assert.Equal(int64(2), status.LatestSuccessful.Books)
	assert.Equal(int64(3), status.LatestSuccessful.Created)
	assert.NotContains(resp.Body.String(), "connection_generation")
}

func TestCardDAVStatusAndRunsCollapseUnknownStoredFailureProjection(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewTestStore(t)
	run, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual})
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
		assert.Contains(resp.Body.String(), `"error_code":"sync_failed"`)
		assert.Contains(resp.Body.String(), `"error_message":"CardDAV sync failed."`)
		assert.NotContains(resp.Body.String(), "future_provider_failure")
		assert.NotContains(resp.Body.String(), "tenant-internal-marker")
	}
}

func TestCardDAVStatusProjectsActiveRunExactly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewTestStore(t)
	active, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{
		Trigger: store.CardDAVSyncTriggerScheduled, Full: true,
	})
	require.NoError(err)
	controller := &CardDAVController{cfg: cfg, store: st}

	resp := getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, nil), "/api/v1/carddav/status")
	require.Equal(http.StatusOK, resp.Code, resp.Body.String())
	var status CardDAVStatusResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&status))
	require.NotNil(status.Active)
	assert.Equal(active.ID, status.Active.ID)
	assert.Equal("scheduled", status.Active.Trigger)
	assert.True(status.Active.Full)
	assert.Equal("running", status.Active.State)
	assert.Nil(status.Active.FinishedAt)
	require.NotNil(status.Latest)
	assert.Equal(active.ID, status.Latest.ID)
	assert.Nil(status.LatestSuccessful)
}

func TestCardDAVRunHistoryPagesNewestFirstWithoutRuntimeService(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st := testutil.NewTestStore(t)
	var ids []int64
	for i := range 3 {
		run, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual, Full: i == 0})
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
	assert.Equal([]int64{ids[2], ids[1]}, []int64{firstPage.Runs[0].ID, firstPage.Runs[1].ID})
	require.NotNil(firstPage.NextBeforeID)
	assert.Equal(ids[1], *firstPage.NextBeforeID)

	second := getCardDAVRead(t, srv, "/api/v1/carddav/runs?limit=2&before_id="+strconv.FormatInt(*firstPage.NextBeforeID, 10))
	require.Equal(http.StatusOK, second.Code, second.Body.String())
	var secondPage CardDAVRunsResponse
	require.NoError(json.NewDecoder(second.Body).Decode(&secondPage))
	require.Len(secondPage.Runs, 1)
	assert.Equal(ids[0], secondPage.Runs[0].ID)
	assert.Nil(secondPage.NextBeforeID)
	assert.NotContains(first.Body.String(), "connection_generation")
}

func TestCardDAVRunHistoryRejectsInvalidPagination(t *testing.T) {
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
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	for _, path := range []string{"/api/v1/carddav/status", "/api/v1/carddav/runs"} {
		missing := getCardDAVRead(t, cardDAVReadServer(t, cfg, nil, nil), path)
		assert.Equal(http.StatusServiceUnavailable, missing.Code, path+": "+missing.Body.String())
		missingStore := getCardDAVRead(t, cardDAVReadServer(t, cfg, &CardDAVController{cfg: cfg}, nil), path)
		assert.Equal(http.StatusServiceUnavailable, missingStore.Code, path+": "+missingStore.Body.String())
	}

	st := testutil.NewTestStore(t)
	_, err := st.DB().Exec(`DROP TABLE carddav_sync_runs`)
	require.NoError(t, err)
	controller := &CardDAVController{cfg: cfg, store: st}
	for _, path := range []string{"/api/v1/carddav/status", "/api/v1/carddav/runs"} {
		failed := getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, nil), path)
		assert.Equal(http.StatusInternalServerError, failed.Code, path+": "+failed.Body.String())
		assert.NotContains(failed.Body.String(), "no such table")
		assert.NotContains(failed.Body.String(), "carddav_sync_runs")
	}
}
