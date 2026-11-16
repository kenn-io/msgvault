package cmd

import (
	"context"
	"errors"
	"log/slog"
	"os"
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

func captureDaemonCacheRefreshes(t *testing.T) <-chan string {
	t.Helper()
	requests := make(chan string, 2)
	refresher := newBackgroundCacheRefresher(t.Context(), func(_ context.Context, id string) error {
		requests <- id
		return nil
	}, nil)
	old := daemonCacheRefresher
	daemonCacheRefresher = refresher
	t.Cleanup(func() {
		daemonCacheRefresher = old
		require.NoError(t, refresher.Shutdown(context.WithoutCancel(t.Context())))
	})
	return requests
}

func TestCardDAVSchedulerRequestsIdentityRefresh(t *testing.T) {
	req := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = true
	st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(cfg.Data.DataDir, "msgvault.db"))
	id, err := st.EnsureParticipant("other@example.test", "Other", "example.test")
	req.NoError(err)
	requests := captureDaemonCacheRefreshes(t)
	sched := scheduler.New(nil)
	t.Cleanup(func() { sched.Stop() })
	service := &committingCardDAVService{store: st, participant: id}
	req.NoError(reconcileCardDAVSchedulerJob(sched, config.CardDAVConfig{Enabled: true, Schedule: "0 */6 * * *"}, service, slog.New(slog.DiscardHandler), config.DefaultCardDAVConnection, testInvocationWithConfig(cfg)))
	for range 2 {
		req.ErrorContains(sched.TriggerJob(api.CardDAVJobName), "later book failed")
		select {
		case id := <-requests:
			req.Equal("carddav", id)
		case <-time.After(10 * time.Second):
			req.FailNow("cache refresh was not requested")
		}
	}
}

func TestCardDAVIdentityRefreshFailureRequestsRecovery(t *testing.T) {
	req := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = true
	requests := captureDaemonCacheRefreshes(t)
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

	dbPath := filepath.Join(cfg.Data.DataDir, "msgvault.db")
	testutil.NewSQLiteTestStoreAt(t, dbPath)
	runDerivedCacheSubprocess = func(ctx context.Context, _ string) error {
		_, err := buildCache(ctx, dbPath, cfg.AnalyticsDir(), true)
		if err != nil {
			return err
		}
		return os.Remove(query.CacheStatePath(cfg.AnalyticsDir()))
	}
	_, err = adapter.RefreshIdentityDatasets(t.Context())
	req.ErrorContains(err, "read refreshed cache revision")
	select {
	case id := <-requests:
		req.Equal("identity", id)
	case <-time.After(10 * time.Second):
		req.FailNow("cache recovery was not requested")
	}
}
