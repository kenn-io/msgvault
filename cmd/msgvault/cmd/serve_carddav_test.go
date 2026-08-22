package cmd

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

type scheduledCardDAVFixture struct{ syncs int }

func (f *scheduledCardDAVFixture) Sync(context.Context, carddav.SyncOptions) (carddav.SyncResult, error) {
	f.syncs++
	return carddav.SyncResult{}, nil
}
func (f *scheduledCardDAVFixture) ListBooks(context.Context) ([]store.CardDAVAddressBook, error) {
	return nil, nil
}
func (f *scheduledCardDAVFixture) SetBookRoles(context.Context, int64, carddav.BookRoles) error {
	return nil
}
func (f *scheduledCardDAVFixture) Publication(context.Context, int64) (*store.CardDAVPublication, error) {
	return &store.CardDAVPublication{}, nil
}
func (f *scheduledCardDAVFixture) PublishPerson(context.Context, int64) error   { return nil }
func (f *scheduledCardDAVFixture) UnpublishPerson(context.Context, int64) error { return nil }
func (f *scheduledCardDAVFixture) ListConflicts(context.Context) ([]store.CardDAVConflict, error) {
	return nil, nil
}
func (f *scheduledCardDAVFixture) GetConflict(context.Context, int64) (*store.CardDAVConflict, error) {
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
