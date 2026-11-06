package scheduler

import (
	"context"
	"errors"
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
		assert := assert.New(t)
		require := require.New(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		require.NoError(os.WriteFile(path, []byte(`[[accounts]]
email = "sample@example.test"
schedule = "0 0 1 1 *"
enabled = true
poll_interval = "30s"
`), 0o600))
		cfg, err := config.Load(path, dir)
		require.NoError(err)
		var calls atomic.Int32
		s := New(func(context.Context, string) error { calls.Add(1); return nil })
		n, errs := s.AddAccountsFromConfig(cfg)
		require.Empty(errs)
		require.Equal(1, n)
		assert.Zero(calls.Load(), "registration must not run before Start")
		s.Start()
		synctest.Wait()
		assert.EqualValues(1, calls.Load(), "rapid accounts catch up when the scheduler starts")
		time.Sleep(30 * time.Second)
		synctest.Wait()
		assert.EqualValues(2, calls.Load(), "typed polling joins the account runner")
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
		assert := assert.New(t)
		require := require.New(t)
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
		assert.True(status.LastCompleted.IsZero(), "partial acquisition is not catchup")
		assert.True(status.LastRun.IsZero())
		assert.False(status.NextEligiblePoll.Before(floor), "provider floor is independent of the 15m adaptive cap")
		require.NoError(s.TriggerSync("sample@example.test", true))
		synctest.Wait()
		require.Error(s.TriggerSync("sample@example.test", true), "one deferred manual request remains accepted")
		s.onAccountTick("sample@example.test")
		synctest.Wait()
		assert.EqualValues(1, calls.Load(), "manual and cron cannot bypass a known provider prohibition")
		assert.Zero(completions.Load())
		time.Sleep(time.Hour)
		synctest.Wait()
		assert.EqualValues(2, calls.Load())
		assert.EqualValues(1, completions.Load(), "accepted manual work completes after the floor")
		assert.False(s.Status()[0].LastCompleted.IsZero())
		<-s.Stop().Done()
	})
}

func TestAccountPollingOffDoesNotRetainProviderFloor(t *testing.T) {
	for _, enabledBeforeFailure := range []bool{false, true} {
		name := "cron-only"
		if enabledBeforeFailure {
			name = "disabled-after-floor"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			var calls atomic.Int32
			floor := time.Now().Add(time.Hour)
			s := New(func(context.Context, string) error {
				if calls.Add(1) == 1 {
					return &AcquisitionError{Outcome: AcquisitionIncomplete, NotBefore: floor}
				}
				return nil
			})
			defer func() { <-s.Stop().Done() }()
			require.NoError(s.AddAccount("sample@example.test", "0 0 1 1 *"))
			if enabledBeforeFailure {
				require.NoError(s.SetAccountPollInterval("sample@example.test", 30*time.Second))
			}
			require.NoError(s.TriggerSync("sample@example.test", false))
			s.wg.Wait()
			if enabledBeforeFailure {
				require.NoError(s.SetAccountPollInterval("sample@example.test", 0))
			}
			require.NoError(s.TriggerSync("sample@example.test", false))
			s.wg.Wait()
			assert.EqualValues(t, 2, calls.Load(), "polling-off manual sync keeps cron-only retry behavior")
		})
	}
}

func TestAccountPollingOrdinaryNonSuccessPreservesAdaptiveDeadline(t *testing.T) {
	for _, outcome := range []error{ErrDeferUntilNextTrigger, &AcquisitionError{Outcome: AcquisitionDeferred}, &AcquisitionError{Outcome: AcquisitionCancelled}} {
		t.Run(outcome.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
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
				require.EqualValues(2, calls.Load())
				deadline := s.Status()[0].NextEligiblePoll
				time.Sleep(time.Second)
				s.onAccountTick("sample@example.test")
				synctest.Wait()
				assert.False(s.Status()[0].NextEligiblePoll.Before(deadline), "ordinary bypass cannot reset poll backoff without complete acquisition")
				time.Sleep(60 * time.Second)
				synctest.Wait()
				assert.EqualValues(3, calls.Load(), "poll cannot run before the retained adaptive deadline")
				<-s.Stop().Done()
			})
		})
	}
}

func TestAccountPollingStartupPreservesDeferredManualIntent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		var calls, completions atomic.Int32
		floor := time.Now().Add(time.Hour)
		s := pollingSchedulerFixture(t, func(context.Context, string) error {
			if calls.Add(1) == 1 {
				return &AcquisitionError{Outcome: AcquisitionDeferred, NotBefore: floor}
			}
			return nil
		}).WithCompletion(func() { completions.Add(1) })
		require.NoError(s.TriggerSync("sample@example.test", false))
		synctest.Wait()
		require.NoError(s.TriggerSync("sample@example.test", true))
		synctest.Wait()
		s.Start()
		synctest.Wait()
		assert.EqualValues(1, calls.Load())
		time.Sleep(time.Hour)
		synctest.Wait()
		assert.EqualValues(2, calls.Load())
		assert.EqualValues(1, completions.Load(), "startup cannot replace an accepted ordinary manual request with poll-only intent")
		<-s.Stop().Done()
	})
}

func TestAccountPollingOutcomeStatusAndManualCompletion(t *testing.T) {
	for _, outcome := range []error{nil, ErrDeferUntilNextTrigger, &AcquisitionError{Outcome: AcquisitionIncomplete}, &AcquisitionError{Outcome: AcquisitionDeferred}, &AcquisitionError{Outcome: AcquisitionCancelled}, errors.New("synthetic-secret-marker")} {
		name := "complete"
		if outcome != nil {
			name = outcome.Error()
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				var events atomic.Int32
				s := pollingSchedulerFixture(t, func(context.Context, string) error { return outcome }).WithCompletion(func() { events.Add(1) })
				require.NoError(t, s.TriggerSync("sample@example.test", true))
				synctest.Wait()
				status := s.Status()[0]
				assert.False(status.LastAttempted.IsZero())
				assert.NotContains(status.LastError, "synthetic-secret-marker")
				if outcome == nil {
					assert.False(status.LastCompleted.IsZero())
					assert.EqualValues(1, events.Load())
				} else {
					assert.True(status.LastCompleted.IsZero())
					assert.True(status.LastRun.IsZero())
					assert.Zero(events.Load())
				}
				<-s.Stop().Done()
			})
		})
	}
}

func TestAccountPollingStopJoinsStartupAndRejectsCapturedTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		cancelled, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		s := pollingSchedulerFixture(t, func(ctx context.Context, _ string) error {
			calls.Add(1)
			<-ctx.Done()
			close(cancelled)
			<-release
			return nil
		})
		s.mu.RLock()
		entry := s.cron.Entry(s.accounts["sample@example.test"].entry).Job
		s.mu.RUnlock()
		s.Start()
		s.Start()
		synctest.Wait()
		stopped := s.Stop()
		<-cancelled
		select {
		case <-stopped.Done():
			assert.Fail("Stop completed before the admitted callback unwound")
		default:
		}
		close(release)
		<-stopped.Done()
		entry.Run()
		assert.EqualValues(1, calls.Load())
		assert.True(s.Status()[0].LastCompleted.IsZero(), "a nil return after cancellation is not catchup")
		assert.Error(s.TriggerSync("sample@example.test", true))
	})
}

func TestAccountPollingIncompleteSkipsPostSyncAndKeepsIndependentIndexing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		var post, index atomic.Int32
		s := pollingSchedulerFixture(t, func(context.Context, string) error { return &AcquisitionError{Outcome: AcquisitionIncomplete} })
		s.SetVisualPostSyncJob(func(context.Context) error { post.Add(1); return nil })
		require.NoError(s.AddJob(Job{Name: "index backstop", Schedule: "0 0 1 1 *", Run: func(context.Context) error { index.Add(1); return nil }}))
		require.NoError(s.TriggerSync("sample@example.test", false))
		synctest.Wait()
		assert.Zero(post.Load(), "incomplete acquisition is not a successful post-sync trigger")
		require.True(s.IsJobScheduled("index backstop"))
		require.NoError(s.TriggerJob("index backstop"))
		assert.EqualValues(1, index.Load(), "independent indexing remains usable and the gate is released")
		<-s.Stop().Done()
	})
}
