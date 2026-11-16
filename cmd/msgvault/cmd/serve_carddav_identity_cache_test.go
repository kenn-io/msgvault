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
	return carddav.SyncResult{}, errors.New("later book failed")
}

func TestCardDAVSchedulerRequestsIdentityRefresh(t *testing.T) {
	req := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.AutoBuildCache = true
	requests := make(chan string, 2)
	refresher := newBackgroundCacheRefresher(t.Context(), func(_ context.Context, id string) error {
		requests <- id
		return nil
	}, nil)
	old := daemonCacheRefresher
	daemonCacheRefresher = refresher
	t.Cleanup(func() {
		daemonCacheRefresher = old
		req.NoError(refresher.Shutdown(context.WithoutCancel(t.Context())))
	})
	sched := scheduler.New(nil)
	t.Cleanup(func() { sched.Stop() })
	service := &failingCardDAVService{}
	req.NoError(reconcileCardDAVSchedulerJob(sched, config.CardDAVConfig{Enabled: true, Schedule: "0 */6 * * *"}, service, slog.New(slog.NewTextHandler(io.Discard, nil)), config.DefaultCardDAVConnection, testInvocationWithConfig(cfg)))
	req.ErrorContains(sched.TriggerJob(api.CardDAVJobName), "later book failed")
	select {
	case id := <-requests:
		req.Equal("carddav", id)
	case <-time.After(10 * time.Second):
		req.FailNow("cache refresh was not requested")
	}
}
