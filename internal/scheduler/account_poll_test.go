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

func TestAcquisitionFilteredCoverageUsesShortStatusText(t *testing.T) {
	assert.Equal(t, "Rapid polling: remove folder/date filters or disable poll_interval.",
		(&AcquisitionError{Reason: AcquisitionFilteredCoverage}).Error())
	assert.Equal(t, "email acquisition failed; retry scheduled",
		(&AcquisitionError{Outcome: AcquisitionFailed}).Error(),
		"the scheduler keeps its safe failure detail for other consumers")
}

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

func TestAccountPollingRearmsEarlyCronTickAtEligibility(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		gate := newSerialWorkTracker()
		release, ok := gate.BeginWork()
		require.True(ok)
		var calls atomic.Int32
		s := pollingSchedulerFixture(t, func(context.Context, string) error {
			calls.Add(1)
			return nil
		}).WithWorkTracker(gate)
		defer func() {
			release()
			<-s.Stop().Done()
		}()

		s.Start()
		synctest.Wait()
		require.True(s.Status()[0].Queued, "startup catchup is waiting for the shared gate")
		time.Sleep(time.Second)
		release()
		synctest.Wait()
		require.EqualValues(1, calls.Load(), "the first successful acquisition follows gate admission")
		nextEligible := s.Status()[0].NextEligiblePoll
		require.True(nextEligible.After(time.Now()))

		time.Sleep(29 * time.Second) // the periodic timer reaches its original 30s phase
		synctest.Wait()
		assert.EqualValues(1, calls.Load(), "an early periodic tick must not acquire before eligibility")
		time.Sleep(time.Second) // the 1s gate delay makes eligibility t31, just after that tick
		synctest.Wait()
		assert.EqualValues(2, calls.Load(), "the deferred tick wakes at eligibility instead of waiting until t60")
		assert.True(time.Now().Before(nextEligible.Add(30 * time.Second)))
	})
}

func TestAccountPollingDeadlineWakeIsFencedByLifecycle(t *testing.T) {
	for _, action := range []string{"disable", "change interval", "remove", "stop", "remove and re-add"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				gate := newSerialWorkTracker()
				release, ok := gate.BeginWork()
				require.True(ok)
				var calls atomic.Int32
				s := pollingSchedulerFixture(t, func(context.Context, string) error {
					calls.Add(1)
					return nil
				}).WithWorkTracker(gate)
				released, stopped := false, false
				defer func() {
					if !released {
						release()
					}
					if !stopped {
						<-s.Stop().Done()
					}
				}()

				s.Start()
				synctest.Wait()
				require.True(s.Status()[0].Queued, "startup catchup waits for the shared gate")
				time.Sleep(time.Second)
				release()
				released = true
				synctest.Wait()
				require.EqualValues(1, calls.Load())
				nextEligible := s.Status()[0].NextEligiblePoll
				time.Sleep(29 * time.Second)
				synctest.Wait()

				email := "sample@example.test"
				s.mu.RLock()
				registration := s.accounts[email]
				var wake *accountPollWake
				if registration != nil {
					wake = registration.pollWake
				}
				s.mu.RUnlock()
				require.NotNil(registration)
				require.NotNil(wake, "the early cron tick arms a deadline wake")
				require.NotNil(wake.timer)
				require.True(wake.deadline.Equal(nextEligible), "the wake is anchored to the poll eligibility deadline")

				switch action {
				case "disable":
					require.NoError(s.SetAccountPollInterval(email, 0))
				case "change interval":
					require.NoError(s.SetAccountPollInterval(email, time.Minute))
				case "remove":
					s.RemoveAccount(email)
				case "stop":
					<-s.Stop().Done()
					stopped = true
				case "remove and re-add":
					s.RemoveAccount(email)
					require.NoError(s.AddAccount(email, "0 0 1 1 *"))
					require.NoError(s.SetAccountPollInterval(email, 30*time.Second))
				}

				// Simulate a callback already dispatched when its timer was cancelled.
				s.onAccountPollWake(email, registration, wake)
				synctest.Wait()
				assert.EqualValues(1, calls.Load(), "obsolete deadline callbacks cannot run an account sync")
				time.Sleep(time.Second)
				synctest.Wait()
				assert.EqualValues(1, calls.Load(), "cancelled deadline wakes do not fire after lifecycle changes")

				s.mu.RLock()
				current := s.accounts[email]
				wakeStillArmed := current != nil && current.pollWake != nil
				s.mu.RUnlock()
				assert.False(wakeStillArmed, "lifecycle changes leave no active one-shot wake")
				if action == "remove and re-add" {
					assert.False(registration == current, "re-adding the same identifier creates a new registration")
				}
			})
		})
	}
}

func TestAccountPollingDeadlineWakeHonorsProviderFloor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		gate := newSerialWorkTracker()
		release, ok := gate.BeginWork()
		require.True(ok)
		floor := time.Now().Add(2 * time.Minute)
		var calls atomic.Int32
		s := pollingSchedulerFixture(t, func(context.Context, string) error {
			if calls.Add(1) == 1 {
				return &AcquisitionError{Outcome: AcquisitionIncomplete, NotBefore: floor}
			}
			return nil
		}).WithWorkTracker(gate)
		released, stopped := false, false
		defer func() {
			if !released {
				release()
			}
			if !stopped {
				<-s.Stop().Done()
			}
		}()

		s.Start()
		synctest.Wait()
		require.True(s.Status()[0].Queued)
		time.Sleep(time.Second)
		release()
		released = true
		synctest.Wait()
		require.EqualValues(1, calls.Load())
		require.True(s.Status()[0].NextEligiblePoll.Equal(floor), "the provider floor extends the adaptive deadline")

		time.Sleep(29 * time.Second)
		synctest.Wait()
		require.EqualValues(1, calls.Load())
		email := "sample@example.test"
		s.mu.RLock()
		registration := s.accounts[email]
		var firstWake *accountPollWake
		if registration != nil {
			firstWake = registration.pollWake
		}
		s.mu.RUnlock()
		require.NotNil(firstWake)
		require.True(firstWake.deadline.Equal(floor), "the one-shot timer waits through the provider floor")

		time.Sleep(60 * time.Second)
		synctest.Wait()
		assert.EqualValues(1, calls.Load(), "repeated cron ticks cannot shorten the provider floor")
		s.mu.RLock()
		currentWake := registration.pollWake
		s.mu.RUnlock()
		require.NotNil(currentWake)
		assert.True(currentWake == firstWake, "early ticks retain the timer for the same deadline")
		assert.True(currentWake.deadline.Equal(floor))
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
