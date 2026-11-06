package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestAccountPollingStartupCatchup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		require.NoError(t, os.WriteFile(path, []byte(`[[accounts]]
email = "sample@example.test"
schedule = "0 0 1 1 *"
enabled = true
poll_interval = "30s"
`), 0o600))
		cfg, err := config.Load(path, dir)
		require.NoError(t, err)
		var calls atomic.Int32
		s := New(func(context.Context, string) error { calls.Add(1); return nil })
		n, errs := s.AddAccountsFromConfig(cfg)
		require.Empty(t, errs)
		require.Equal(t, 1, n)
		assert.Zero(t, calls.Load(), "registration must not run before Start")
		s.Start()
		synctest.Wait()
		assert.EqualValues(t, 1, calls.Load(), "rapid accounts catch up when the scheduler starts")
		time.Sleep(30 * time.Second)
		synctest.Wait()
		assert.EqualValues(t, 2, calls.Load(), "typed polling joins the account runner")
		<-s.Stop().Done()
	})
}

func pollingSchedulerFixture(t *testing.T, run SyncFunc) *Scheduler {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[[accounts]]
email = "sample@example.test"
schedule = "0 0 1 1 *"
enabled = true
poll_interval = "30s"
`), 0o600))
	cfg, err := config.Load(path, dir)
	require.NoError(t, err)
	s := New(run).WithWorkTracker(newSerialWorkTracker())
	_, errs := s.AddAccountsFromConfig(cfg)
	require.Empty(t, errs)
	return s
}

func TestAccountPollingFailureDoesNotImmediatelyReplayQueuedPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		s := pollingSchedulerFixture(t, func(context.Context, string) error {
			if calls.Add(1) == 1 {
				<-release
			}
			return context.DeadlineExceeded
		})
		s.Start()
		synctest.Wait()
		time.Sleep(30 * time.Second) // a poll arrives while acquisition is executing
		synctest.Wait()
		close(release)
		synctest.Wait()
		assert.EqualValues(t, 1, calls.Load(), "a pending poll must honor the failure's new backoff")
		time.Sleep(30 * time.Second)
		synctest.Wait()
		assert.EqualValues(t, 1, calls.Load(), "first failure backs off beyond the ordinary interval")
		<-s.Stop().Done()
	})
}

func TestAccountPollingOrdinaryIntentSurvivesPollAndFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		s := pollingSchedulerFixture(t, func(context.Context, string) error {
			if calls.Add(1) == 1 {
				<-release
				return context.DeadlineExceeded
			}
			return nil
		})
		s.Start()
		synctest.Wait()
		go s.onAccountTick("sample@example.test")
		synctest.Wait()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		close(release)
		synctest.Wait()
		assert.EqualValues(t, 2, calls.Load(), "polls must not erase an accepted ordinary follow-up")
		<-s.Stop().Done()
	})
}

func TestAccountPollingProviderFloorDefersAllOriginsAndRetainsManualReceipt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls, completions atomic.Int32
		floor := time.Now().Add(time.Hour)
		s := pollingSchedulerFixture(t, func(context.Context, string) error {
			if calls.Add(1) == 1 {
				return &AcquisitionError{Outcome: AcquisitionIncomplete, NotBefore: floor}
			}
			return nil
		}).WithCompletion(func() { completions.Add(1) })
		s.Start()
		synctest.Wait()
		status := s.Status()[0]
		assert.True(t, status.LastCompleted.IsZero(), "partial acquisition is not catchup")
		assert.True(t, status.LastRun.IsZero())
		assert.False(t, status.NextEligiblePoll.Before(floor), "provider floor is independent of the 15m adaptive cap")
		require.NoError(t, s.TriggerSync("sample@example.test", true))
		synctest.Wait()
		assert.Error(t, s.TriggerSync("sample@example.test", true), "one deferred manual request remains accepted")
		s.onAccountTick("sample@example.test")
		synctest.Wait()
		assert.EqualValues(t, 1, calls.Load(), "manual and cron cannot bypass a known provider prohibition")
		assert.Zero(t, completions.Load())
		time.Sleep(time.Hour)
		synctest.Wait()
		assert.EqualValues(t, 2, calls.Load())
		assert.EqualValues(t, 1, completions.Load(), "accepted manual work completes after the floor")
		assert.False(t, s.Status()[0].LastCompleted.IsZero())
		<-s.Stop().Done()
	})
}

func TestAccountPollingOrdinaryNonSuccessPreservesAdaptiveDeadline(t *testing.T) {
	for _, outcome := range []error{ErrDeferUntilNextTrigger, &AcquisitionError{Outcome: AcquisitionDeferred}, &AcquisitionError{Outcome: AcquisitionCancelled}} {
		t.Run(outcome.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				s := pollingSchedulerFixture(t, func(context.Context, string) error {
					if calls.Add(1) <= 2 {
						return context.DeadlineExceeded
					}
					return outcome
				})
				s.Start()
				synctest.Wait()
				time.Sleep(90 * time.Second)
				synctest.Wait()
				require.EqualValues(t, 2, calls.Load())
				deadline := s.Status()[0].NextEligiblePoll
				time.Sleep(time.Second)
				s.onAccountTick("sample@example.test")
				synctest.Wait()
				assert.False(t, s.Status()[0].NextEligiblePoll.Before(deadline), "ordinary bypass cannot reset poll backoff without complete acquisition")
				time.Sleep(60 * time.Second)
				synctest.Wait()
				assert.EqualValues(t, 3, calls.Load(), "poll cannot run before the retained adaptive deadline")
				<-s.Stop().Done()
			})
		})
	}
}

func TestAccountPollingStartupPreservesDeferredManualIntent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls, completions atomic.Int32
		floor := time.Now().Add(time.Hour)
		s := pollingSchedulerFixture(t, func(context.Context, string) error {
			if calls.Add(1) == 1 {
				return &AcquisitionError{Outcome: AcquisitionDeferred, NotBefore: floor}
			}
			return nil
		}).WithCompletion(func() { completions.Add(1) })
		require.NoError(t, s.TriggerSync("sample@example.test", false))
		synctest.Wait()
		require.NoError(t, s.TriggerSync("sample@example.test", true))
		synctest.Wait()
		s.Start()
		synctest.Wait()
		assert.EqualValues(t, 1, calls.Load())
		time.Sleep(time.Hour)
		synctest.Wait()
		assert.EqualValues(t, 2, calls.Load())
		assert.EqualValues(t, 1, completions.Load(), "startup cannot replace an accepted ordinary manual request with poll-only intent")
		<-s.Stop().Done()
	})
}
