package cmd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type scheduledCardDAVFixture struct {
	syncs   int
	options []carddav.SyncOptions
}

func TestRecoverCardDAVSyncRunsAtStartupTerminalizesOrphansAndLogsOnlyCount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{
		Trigger: store.CardDAVSyncTriggerScheduled,
	})
	require.NoError(err)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	require.NoError(recoverCardDAVSyncRunsAtStartup(t.Context(), st, logger))
	runs, err := st.ListCardDAVSyncRunsContext(t.Context(), 10, nil)
	require.NoError(err)
	require.Len(runs, 1)
	assert.Equal(store.CardDAVSyncRunFailed, runs[0].State)
	assert.Equal("daemon_restarted", runs[0].ErrorCode)
	assert.Contains(logs.String(), "count=1")
	assert.NotContains(strings.ToLower(logs.String()), "error_message")
}

func TestRecoverCardDAVSyncRunsAtStartupReturnsFailure(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := st.DB().Exec(`DROP TABLE carddav_sync_runs`)
	require.NoError(t, err)

	err = recoverCardDAVSyncRunsAtStartup(t.Context(), st, slog.New(slog.DiscardHandler))
	require.Error(t, err)
	assert.ErrorContains(t, err, "recover CardDAV sync runs")
}

func (f *scheduledCardDAVFixture) Sync(_ context.Context, options carddav.SyncOptions) (carddav.SyncResult, error) {
	f.syncs++
	f.options = append(f.options, options)
	return carddav.SyncResult{}, nil
}
func (f *scheduledCardDAVFixture) ListBooks(context.Context) ([]store.CardDAVAddressBook, error) {
	return nil, nil
}
func (f *scheduledCardDAVFixture) SetBookRoles(context.Context, int64, carddav.BookRoles) error {
	return nil
}
func (f *scheduledCardDAVFixture) PublicationView(context.Context, int64) (*carddav.PublicationView, error) {
	return &carddav.PublicationView{}, nil
}
func (f *scheduledCardDAVFixture) PublishPerson(context.Context, int64) error   { return nil }
func (f *scheduledCardDAVFixture) UnpublishPerson(context.Context, int64) error { return nil }
func (f *scheduledCardDAVFixture) ListConflictViews(context.Context) ([]carddav.ConflictListItem, error) {
	return nil, nil
}
func (f *scheduledCardDAVFixture) GetConflictView(context.Context, int64) (*carddav.ConflictDetail, error) {
	return nil, store.ErrCardDAVConflictNotFound
}
func (f *scheduledCardDAVFixture) ResolveConflict(context.Context, int64, carddav.ResolutionChoice) error {
	return nil
}

func TestRegisterCardDAVSchedulerJobRequiresEnabledSchedule(t *testing.T) {
	tests := []struct {
		name       string
		config     config.CardDAVConfig
		wantStatus []scheduler.JobStatus
	}{
		{name: "disabled with schedule", config: config.CardDAVConfig{Schedule: "0 */6 * * *"}},
		{name: "enabled without schedule", config: config.CardDAVConfig{Enabled: true}},
		{name: "enabled and scheduled", config: config.CardDAVConfig{Enabled: true, Schedule: "0 */6 * * *"}, wantStatus: []scheduler.JobStatus{{Name: api.CardDAVJobName, Schedule: "0 */6 * * *"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			sched := scheduler.New(nil)
			t.Cleanup(func() { sched.Stop() })
			service := &scheduledCardDAVFixture{}
			logger := slog.New(slog.DiscardHandler)
			require.NoError(reconcileCardDAVSchedulerJob(sched, tt.config, service, logger))
			status := sched.JobStatus()
			if len(tt.wantStatus) == 0 {
				assert.Empty(status)
				return
			}
			require.Len(status, 1)
			assert.Equal(tt.wantStatus[0].Name, status[0].Name)
			assert.Equal(tt.wantStatus[0].Schedule, status[0].Schedule)
		})
	}
}

func TestRegisterCardDAVSchedulerJobSkipsUnavailableService(t *testing.T) {
	sched := scheduler.New(nil)
	t.Cleanup(func() { sched.Stop() })

	require.NoError(t, reconcileCardDAVSchedulerJob(sched,
		config.CardDAVConfig{Enabled: true, Schedule: "0 */6 * * *"}, nil,
		slog.New(slog.DiscardHandler)))
	assert.False(t, sched.IsJobScheduled(api.CardDAVJobName))
}

func TestGoogleCardDAVSchedulerWaitsForAuthorization(t *testing.T) {
	assertions := assert.New(t)
	required := require.New(t)
	dir := t.TempDir()
	secrets := filepath.Join(dir, "client.json")
	required.NoError(os.WriteFile(secrets, []byte(`{"web":{"client_id":"synthetic-client","client_secret":"synthetic-secret","redirect_uris":["https://archive.example/"]}}`), 0600))
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.OAuth.ClientSecrets = secrets
	cfg.CardDAV = config.CardDAVConfig{Provider: "google", BaseURL: carddav.GoogleDiscoveryURL, Username: "person@example.com", Enabled: true, Schedule: "0 1 * * *"}
	required.NoError(cfg.Save())
	st := testutil.NewTestStore(t)
	_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username,
		PrincipalURL: "https://www.googleapis.com/principal/", HomeURL: "https://www.googleapis.com/contacts/",
	})
	required.NoError(err)
	required.NoError(carddav.SaveCredential(cfg.TokensDir(), carddav.Credential{
		Google: true, BaseURL: cfg.CardDAV.BaseURL, Username: cfg.CardDAV.Username, ConnectionGeneration: 1,
	}))
	logger := slog.New(slog.DiscardHandler)
	controller, err := api.NewCardDAVController(cfg, st, logger)
	required.NoError(err)
	required.NotNil(controller.Current(), "manual operations retain a runtime that can load new credentials")
	sched := scheduler.New(nil)
	t.Cleanup(func() { sched.Stop() })
	controller.SetScheduleReconciler(func(settings config.CardDAVConfig, service api.CardDAVOperations) error {
		return reconcileCardDAVSchedulerJob(sched, settings, service, logger)
	})
	required.NoError(controller.ReconcileSchedule())
	status, err := controller.Status(t.Context())
	required.NoError(err)
	assertions.Equal("google_authorization_required", status.RepairReason)
	assertions.False(sched.IsJobScheduled(api.CardDAVJobName), "startup must skip an unauthorized Google account")

	request := api.CardDAVAccountRequest{Provider: "google", Username: cfg.CardDAV.Username, Enabled: new(true), Schedule: "0 2 * * *"}
	_, err = controller.Save(t.Context(), request)
	required.NoError(err)
	assertions.False(sched.IsJobScheduled(api.CardDAVJobName), "schedule-only saves must also skip missing authorization")

	// A CLI authorization is picked up when the account is saved again.
	tokenPath := filepath.Join(cfg.TokensDir(), cfg.CardDAV.Username+".json")
	token := fmt.Sprintf(`{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","client_id":"synthetic-client","scopes":[%q]}`, oauth.ScopeCardDAV)
	required.NoError(os.WriteFile(tokenPath, []byte(token), 0600))
	request.Schedule = "0 3 * * *"
	_, err = controller.Save(t.Context(), request)
	required.NoError(err)
	assertions.True(sched.IsJobScheduled(api.CardDAVJobName))
	jobs := sched.JobStatus()
	required.Len(jobs, 1)
	assertions.Equal(request.Schedule, jobs[0].Schedule)

	required.NoError(os.Remove(tokenPath))
	request.Schedule = "0 4 * * *"
	_, err = controller.Save(t.Context(), request)
	required.NoError(err)
	assertions.False(sched.IsJobScheduled(api.CardDAVJobName), "saving after credentials are removed must unschedule the account")
}

func TestReconcileCardDAVSchedulerJobUpdatesRunsAndRemovesStableJob(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tracker := &fakeDaemonWorkTracker{allow: true}
	sched := scheduler.New(nil).WithWorkTracker(tracker)
	t.Cleanup(func() { sched.Stop() })
	service := &scheduledCardDAVFixture{}
	logger := slog.New(slog.DiscardHandler)

	require.NoError(reconcileCardDAVSchedulerJob(sched, config.CardDAVConfig{Enabled: true, Schedule: "0 1 * * *"}, service, logger))
	require.NoError(sched.TriggerJob(api.CardDAVJobName))
	assert.Equal(1, service.syncs)
	require.Len(service.options, 1)
	assert.Equal(store.CardDAVSyncTriggerScheduled, service.options[0].Trigger)
	begin, done := tracker.counts()
	assert.Equal(1, begin)
	assert.Equal(1, done)

	require.NoError(reconcileCardDAVSchedulerJob(sched, config.CardDAVConfig{Enabled: true, Schedule: "0 2 * * *"}, service, logger))
	status := sched.JobStatus()
	require.Len(status, 1)
	assert.Equal("0 2 * * *", status[0].Schedule)
	require.NoError(sched.TriggerJob(api.CardDAVJobName))
	assert.Equal(2, service.syncs)

	require.NoError(reconcileCardDAVSchedulerJob(sched, config.CardDAVConfig{Enabled: false, Schedule: "0 2 * * *"}, service, logger))
	assert.False(sched.IsJobScheduled(api.CardDAVJobName))
}
