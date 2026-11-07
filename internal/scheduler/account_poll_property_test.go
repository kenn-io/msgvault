package scheduler

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestAccountPollingGenerationReplacementInvalidatesCallbacks(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		generations := hegel.Draw(ht, hegel.Integers[uint64](1, math.MaxUint64))
		var calls int
		s := New(func(context.Context, string) error { calls++; return nil })
		defer func() { <-s.Stop().Done() }()
		require.NoError(ht, s.AddAccount("sample@example.test", "0 0 1 1 *"))
		for range min(generations, 128) {
			s.mu.RLock()
			old := s.cron.Entry(s.jobs["sample@example.test"]).Job
			s.mu.RUnlock()
			s.RemoveAccount("sample@example.test")
			require.NoError(ht, s.AddAccount("sample@example.test", "0 0 1 1 *"))
			old.Run()
		}
		assert.Zero(ht, calls, "dispatched callbacks cannot cross registration generations")
	})
}

func TestAccountPollingBackoffStaysWithinAdaptiveBounds(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		seconds := hegel.Draw(ht, hegel.Integers(15, 120))
		failures := int(min(hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64)), 10))
		interval := time.Duration(seconds) * time.Second
		var entered time.Time
		fail := true
		s := New(func(context.Context, string) error {
			entered = time.Now()
			if fail {
				return errors.New("synthetic acquisition failure")
			}
			return nil
		})
		defer func() { <-s.Stop().Done() }()
		require.NoError(ht, s.AddAccount("sample@example.test", "0 0 1 1 *"))
		require.NoError(ht, s.SetAccountPollInterval("sample@example.test", interval))
		checkFailure := func(streak int) {
			require.NoError(ht, s.TriggerSync("sample@example.test", false))
			s.wg.Wait()
			observed := time.Now()
			status := s.Status()[0]
			// Independent closed-form contract bounds around the real callback
			// and completed observation; no private backoff result is the oracle.
			base := time.Duration(min(seconds<<streak, 900)) * time.Second
			jitter := min(base/10, 15*time.Minute-base)
			assert.False(ht, status.NextEligiblePoll.Before(entered.Add(base)), "failure delay never starts early")
			assert.False(ht, status.NextEligiblePoll.After(observed.Add(base+jitter)), "jitter and adaptive cap bound real eligibility")
		}
		for streak := 1; streak <= failures; streak++ {
			checkFailure(streak)
		}
		fail = false
		require.NoError(ht, s.TriggerSync("sample@example.test", false))
		s.wg.Wait()
		status := s.Status()[0]
		assert.False(ht, status.LastCompleted.IsZero())
		assert.Equal(ht, status.LastAttempted.Add(interval), status.NextEligiblePoll, "complete acquisition resets the cadence")
		fail = true
		checkFailure(1) // a fresh failure after success starts a new streak
	})
}

func TestAccountPollingMixedBurstFailureRetainsOrdinaryIntent(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		actions := hegel.Draw(ht, hegel.Lists(hegel.Booleans()))
		// Limit materialized work while drawing the complete sequence domain.
		actions = actions[:min(len(actions), 1000)]
		var s *Scheduler
		calls := 0
		ordinary := false
		s = pollingSchedulerFixture(ht.T, func(context.Context, string) error {
			calls++
			if calls != 1 {
				return nil
			}
			s.mu.Lock()
			r := s.accounts["sample@example.test"]
			r.nextEligible = time.Time{} // a due timer during the executing pass
			s.mu.Unlock()
			for _, cron := range actions {
				if cron {
					ordinary = true
					s.onAccountTick("sample@example.test")
				} else {
					s.onRegisteredAccountTick("sample@example.test", r, true)
				}
			}
			return errors.New("synthetic acquisition failure")
		})
		defer func() { <-s.Stop().Done() }()
		s.onAccountTick("sample@example.test")
		s.wg.Wait()
		want := 1
		if ordinary {
			want = 2
		}
		assert.Equal(ht, want, calls, "only accepted ordinary intent bypasses adaptive poll backoff")
	})
}
