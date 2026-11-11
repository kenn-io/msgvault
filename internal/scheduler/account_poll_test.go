package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
)

func TestCronRegistrationPreservesPolling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		var calls atomic.Int32
		s := New(func(context.Context, string) error { return nil }).WithAccountPoll(func(context.Context, string) error { calls.Add(1); return nil })
		cfg := &config.Config{Accounts: []config.AccountSchedule{
			{Email: "a@example.test", Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second},
			{Email: "a@example.test", Enabled: true, Schedule: "0 * * * *"},
		}}
		_, errs := s.AddAccountsFromConfig(cfg)
		requirements.Empty(errs)
		s.Start()
		synctest.Wait()
		checks.EqualValues(1, calls.Load())
		checks.Equal(15*time.Second, time.Until(s.Status()[0].NextRun))
		time.Sleep(15 * time.Second)
		synctest.Wait()
		checks.EqualValues(2, calls.Load())
		<-s.Stop().Done()
	})
}

func TestPollingAllowsLongAcquisition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirements := require.New(t)
		completed := make(chan struct{}, 1)
		s := New(func(context.Context, string) error { return nil }).
			WithAccountPoll(func(ctx context.Context, _ string) error {
				select {
				case <-time.After(6 * time.Minute):
					completed <- struct{}{}
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		defer func() { <-s.Stop().Done() }()
		_, errs := s.AddAccountsFromConfig(&config.Config{Accounts: []config.AccountSchedule{{Email: "gmail@example.test", Enabled: true, Schedule: "0 * * * *", PollInterval: 120 * time.Second}}})
		requirements.Empty(errs)
		s.Start()
		time.Sleep(6 * time.Minute)
		synctest.Wait()
		select {
		case <-completed:
		default:
			requirements.FailNow("long poll did not complete")
		}
	})
}

func TestIncompletePollingPreservesCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		var calls atomic.Int32
		s := New(func(context.Context, string) error { return nil }).WithAccountPoll(func(context.Context, string) error {
			if calls.Add(1) == 2 {
				return ErrAcquisitionIncomplete
			}
			return nil
		})
		_, errs := s.AddAccountsFromConfig(&config.Config{Accounts: []config.AccountSchedule{{Email: "a@example.com", Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second}}})
		requirements.Empty(errs)
		s.Start()
		synctest.Wait()
		first := s.Status()[0].LastCompleted
		time.Sleep(15 * time.Second)
		synctest.Wait()
		checks.EqualValues(2, calls.Load())
		checks.Equal(first, s.Status()[0].LastCompleted)
		checks.Empty(s.Status()[0].LastError)
		s.RemoveAccount("a@example.com")
		time.Sleep(15 * time.Second)
		synctest.Wait()
		checks.EqualValues(2, calls.Load())
		<-s.Stop().Done()
	})
}

func TestCronPromotesQueuedPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		gate := newSerialWorkTracker()
		release, ok := gate.BeginWorkContext(t.Context())
		requirements.True(ok)
		var cronCalls, pollCalls int
		s := New(func(context.Context, string) error { cronCalls++; return nil }).WithAccountPoll(func(context.Context, string) error { pollCalls++; return nil }).WithWorkTracker(gate)
		requirements.NoError(s.AddAccount("a@example.com", "0 * * * *"))
		go s.onAccountTrigger("a@example.com", true)
		synctest.Wait()
		checks.False(s.hasQueuedRuns())
		s.onAccountTick("a@example.com")
		checks.True(s.hasQueuedRuns())
		release()
		synctest.Wait()
		checks.Equal(1, cronCalls)
		checks.Zero(pollCalls)
		checks.False(s.hasQueuedRuns())
		<-s.Stop().Done()
	})
}

func TestAccountCompletionDistinguishesPartialAndFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		checks := assert.New(t)
		requirements := require.New(t)
		var result error
		s := New(func(context.Context, string) error { return result })
		requirements.NoError(s.AddAccount("a@example.com", "0 * * * *"))
		s.onAccountTick("a@example.com")
		first := s.Status()[0].LastCompleted
		time.Sleep(time.Second)
		result = ErrAcquisitionIncomplete
		s.onAccountTick("a@example.com")
		status := s.Status()[0]
		checks.Equal(first, status.LastCompleted)
		checks.True(status.LastRun.After(first))
		checks.Empty(status.LastError)
		result = errors.Join(ErrAcquisitionIncomplete, errors.New("provider failed"))
		s.onAccountTick("a@example.com")
		checks.Contains(s.Status()[0].LastError, "provider failed")
		time.Sleep(time.Second)
		result = ErrDeferUntilNextTrigger
		s.onAccountTick("a@example.com")
		checks.Equal(first, s.Status()[0].LastCompleted)
		result = nil
		s.onAccountTick("a@example.com")
		checks.True(s.Status()[0].LastCompleted.After(first))
		<-s.Stop().Done()
	})
}

func TestPollingErrorPreservesCronError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirements := require.New(t)
		checks := assert.New(t)
		pollErr := errors.New("source unavailable")
		s := New(func(context.Context, string) error { return errors.New("cron failed") }).WithAccountPoll(func(context.Context, string) error { return pollErr })
		requirements.NoError(s.AddAccount("a@example.com", "0 * * * *"))
		s.onAccountTick("a@example.com")
		s.onAccountTrigger("a@example.com", true)
		synctest.Wait()
		checks.Contains(s.Status()[0].LastError, "cron failed")
		checks.Contains(s.Status()[0].LastError, "source unavailable")
		pollErr = nil
		s.onAccountTrigger("a@example.com", true)
		synctest.Wait()
		checks.Contains(s.Status()[0].LastError, "cron failed")
		checks.NotContains(s.Status()[0].LastError, "source unavailable")
		<-s.Stop().Done()
	})
}

func TestRemoveAccountClearsPollingTimes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirements := require.New(t)
		checks := assert.New(t)
		s := New(func(context.Context, string) error { return nil })
		requirements.NoError(s.AddAccount("a@example.com", "0 * * * *"))
		s.onAccountTick("a@example.com")
		requirements.False(s.Status()[0].LastCompleted.IsZero())
		s.RemoveAccount("a@example.com")
		requirements.NoError(s.AddAccount("a@example.com", "0 * * * *"))
		checks.True(s.Status()[0].LastAttempted.IsZero())
		checks.True(s.Status()[0].LastCompleted.IsZero())
		<-s.Stop().Done()
	})
}

func TestInvalidIntervalReplacesEarlierPollingSchedule(t *testing.T) {
	requirements := require.New(t)
	sched := New(func(context.Context, string) error { return nil }).WithAccountPoll(func(context.Context, string) error { return nil })
	cfg := &config.Config{Accounts: []config.AccountSchedule{{Email: "a@example.com", Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second}}}
	count, errs := sched.AddAccountsFromConfig(cfg)
	requirements.Equal(1, count)
	requirements.Empty(errs)
	cfg.Accounts[0].PollInterval = 14 * time.Second
	count, errs = sched.AddAccountsFromConfig(cfg)
	requirements.Equal(1, count)
	requirements.NotEmpty(errs)
	requirements.Empty(sched.Status()[0].PollInterval)
	<-sched.Stop().Done()
}
