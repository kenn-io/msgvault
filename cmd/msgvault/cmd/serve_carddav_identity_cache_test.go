package cmd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
)

type failingCardDAVService struct {
	scheduledCardDAVFixture
}

func (f *failingCardDAVService) Sync(context.Context, carddav.SyncOptions) (carddav.SyncResult, error) {
	return f.result, errors.New("later book failed")
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
	requests := captureDaemonCacheRefreshes(t)
	sched := scheduler.New(nil)
	t.Cleanup(func() { sched.Stop() })
	service := &failingCardDAVService{scheduledCardDAVFixture{result: carddav.SyncResult{Books: 1}}}
	req.NoError(reconcileCardDAVSchedulerJob(sched, config.CardDAVConfig{Enabled: true, Schedule: "0 */6 * * *"}, service, slog.New(slog.NewTextHandler(io.Discard, nil)), config.DefaultCardDAVConnection, testInvocationWithConfig(cfg)))
	req.ErrorContains(sched.TriggerJob(api.CardDAVJobName), "later book failed")
	select {
	case id := <-requests:
		req.Equal("carddav", id)
	case <-time.After(10 * time.Second):
		req.FailNow("cache refresh was not requested")
	}
}
