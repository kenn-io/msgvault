package cmd

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type committingCardDAVService struct {
	scheduledCardDAVFixture

	store       *store.Store
	participant int64
}

func (f *committingCardDAVService) Sync(ctx context.Context, _ carddav.SyncOptions) (carddav.SyncResult, error) {
	_, _, err := f.store.CreatePersonFromParticipantContext(ctx, f.participant)
	if err != nil {
		return carddav.SyncResult{}, err
	}
	return carddav.SyncResult{Books: 1}, errors.New("later book failed")
}

func TestCardDAVIdentityRefreshUpdatesCache(t *testing.T) {
	req := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	st := testutil.NewTestStore(t)
	id, err := st.EnsureParticipant("other@example.test", "Other", "example.test")
	req.NoError(err)
	calls := 0
	old := runDerivedCacheSubprocess
	runDerivedCacheSubprocess = func(ctx context.Context, dir string) error {
		calls++
		person, err := st.PersonForParticipantsContext(ctx, []int64{id})
		req.NoError(err)
		req.NotNil(person)
		req.Equal(cfg.AnalyticsDir(), dir)
		return nil
	}
	t.Cleanup(func() { runDerivedCacheSubprocess = old })
	logger := slog.New(slog.DiscardHandler)
	sched := scheduler.New(nil)
	t.Cleanup(func() { sched.Stop() })
	service := &committingCardDAVService{store: st, participant: id}
	state := testInvocationWithConfig(cfg)
	req.NoError(reconcileCardDAVSchedulerJob(sched, config.CardDAVConfig{Enabled: true, Schedule: "0 */6 * * *"}, service, logger, config.DefaultCardDAVConnection, state, st))
	req.ErrorContains(sched.TriggerJob(api.CardDAVJobName), "later book failed")
	req.Equal(1, calls)
}
