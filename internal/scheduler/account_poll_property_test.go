package scheduler

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestAccountPollingOrdinaryBurstsHaveOneFollowup(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		burst := hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64))
		var calls atomic.Int32
		started, release := make(chan struct{}), make(chan struct{})
		s := New(func(context.Context, string) error {
			if calls.Add(1) == 1 {
				close(started)
				<-release
			}
			return nil
		})
		defer func() { <-s.Stop().Done() }()
		require.NoError(ht, s.AddAccount("sample@example.test", "0 0 1 1 *"))
		go s.onAccountTick("sample@example.test")
		<-started
		// Bound physical materialization, not the drawn burst domain. Every
		// nonempty drawn burst still exercises its observable follow-up.
		for range min(burst, 1000) {
			s.onAccountTick("sample@example.test")
		}
		close(release)
		s.wg.Wait()
		want := int32(1)
		if burst != 0 {
			want = 2
		}
		assert.Equal(ht, want, calls.Load())
	})
}

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
		failures := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		delay := accountPollBackoff(time.Duration(seconds)*time.Second, failures)
		minimum := time.Duration(min(seconds<<min(failures, 10), 900)) * time.Second
		assert.GreaterOrEqual(ht, delay, minimum, "jitter never shortens the adaptive delay")
		assert.LessOrEqual(ht, delay, 15*time.Minute)
		if failures == 0 {
			assert.Equal(ht, time.Duration(seconds)*time.Second, delay)
		}
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
