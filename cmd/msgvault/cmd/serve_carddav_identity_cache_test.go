package cmd

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type committingCardDAVService struct {
	scheduledCardDAVFixture

	store       *store.Store
	participant int64
	committed   bool
}

func (f *committingCardDAVService) Sync(ctx context.Context, _ carddav.SyncOptions) (carddav.SyncResult, error) {
	if !f.committed {
		_, _, err := f.store.CreatePersonFromParticipantContext(ctx, f.participant)
		if err != nil {
			return carddav.SyncResult{}, err
		}
		f.committed = true
	}
	return carddav.SyncResult{Books: 1}, errors.New("later book failed")
}

func TestCardDAVIdentityRefreshUpdatesCache(t *testing.T) {
	req := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = true
	cfg.Analytics.MinRebuildInterval = 0
	dbPath := filepath.Join(cfg.Data.DataDir, "msgvault.db")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	id, err := st.EnsureParticipant("other@example.test", "Other", "example.test")
	req.NoError(err)
	_, err = buildCache(t.Context(), dbPath, cfg.AnalyticsDir(), true)
	req.NoError(err)
	initial, err := query.ReadCacheSyncState(cfg.AnalyticsDir())
	req.NoError(err)
	oldBuild := runScheduledBuildCacheSubprocess
	attempts := 0
	runScheduledBuildCacheSubprocess = func(ctx context.Context) error {
		attempts++
		if attempts == 1 {
			return errors.New("synthetic publication failure")
		}
		_, err := buildCache(ctx, dbPath, cfg.AnalyticsDir(), true)
		return err
	}
	t.Cleanup(func() { runScheduledBuildCacheSubprocess = oldBuild })
	completed := make(chan error, 2)
	refresher := newBackgroundCacheRefresher(testInvocationContext(t.Context(), cfg, invocationOptions{}), nil, nil)
	run := refresher.run
	refresher.run = func(ctx context.Context, id string) error {
		err := run(ctx, id)
		completed <- err
		return err
	}
	old := daemonCacheRefresher
	daemonCacheRefresher = refresher
	t.Cleanup(func() {
		daemonCacheRefresher = old
		req.NoError(refresher.Shutdown(context.WithoutCancel(t.Context())))
	})
	logger := slog.New(slog.DiscardHandler)
	sched := scheduler.New(nil)
	t.Cleanup(func() { sched.Stop() })
	service := &committingCardDAVService{store: st, participant: id}
	state := testInvocationWithConfig(cfg)
	req.NoError(reconcileCardDAVSchedulerJob(sched, config.CardDAVConfig{Enabled: true, Schedule: "0 */6 * * *"}, service, logger, config.DefaultCardDAVConnection, state))
	req.ErrorContains(sched.TriggerJob(api.CardDAVJobName), "later book failed")
	select {
	case err := <-completed:
		req.ErrorContains(err, "synthetic publication failure")
	case <-time.After(30 * time.Second):
		req.FailNow("cache refresh did not finish")
	}
	failed, err := query.ReadCacheSyncState(cfg.AnalyticsDir())
	req.NoError(err)
	req.Equal(initial.IdentityRevision, failed.IdentityRevision)
	req.ErrorContains(sched.TriggerJob(api.CardDAVJobName), "later book failed")
	select {
	case err := <-completed:
		req.NoError(err)
	case <-time.After(30 * time.Second):
		req.FailNow("cache retry did not finish")
	}
	published, err := query.ReadCacheSyncState(cfg.AnalyticsDir())
	req.NoError(err)
	revision, err := st.IdentityRevisionContext(t.Context())
	req.NoError(err)
	req.Greater(revision, initial.IdentityRevision)
	req.Equal(revision, published.IdentityRevision)
}

func TestCardDAVIdentityRefreshFailureRequestsRecovery(t *testing.T) {
	req := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = true
	requests := make(chan string, 2)
	refresher := newBackgroundCacheRefresher(t.Context(), func(_ context.Context, id string) error {
		requests <- id
		return nil
	}, nil)
	oldRefresher := daemonCacheRefresher
	daemonCacheRefresher = refresher
	t.Cleanup(func() {
		daemonCacheRefresher = oldRefresher
		req.NoError(refresher.Shutdown(context.WithoutCancel(t.Context())))
	})
	oldBuild := runDerivedCacheSubprocess
	t.Cleanup(func() { runDerivedCacheSubprocess = oldBuild })
	adapter := &storeAPIAdapter{config: cfg, analyticsDir: cfg.AnalyticsDir()}
	runDerivedCacheSubprocess = func(context.Context, string) error { return errors.New("synthetic build failure") }
	_, err := adapter.RefreshIdentityDatasets(t.Context())
	req.ErrorContains(err, "synthetic build failure")
	select {
	case id := <-requests:
		req.Equal("identity", id)
	case <-time.After(10 * time.Second):
		req.FailNow("cache recovery was not requested")
	}

	runDerivedCacheSubprocess = func(context.Context, string) error { return nil }
	_, err = adapter.RefreshIdentityDatasets(t.Context())
	req.ErrorContains(err, "read refreshed cache revision")
	select {
	case id := <-requests:
		req.Equal("identity", id)
	case <-time.After(10 * time.Second):
		req.FailNow("cache recovery was not requested")
	}
}
