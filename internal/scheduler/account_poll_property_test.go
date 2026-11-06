package scheduler

import (
	"context"
	"math"
	"sync/atomic"
	"testing"

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
