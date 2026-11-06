package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountPollingRemovedDispatchedCallbackCannotResurrect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		s := pollingSchedulerFixture(t, func(context.Context, string) error { calls.Add(1); return nil })
		s.mu.RLock()
		old := s.cron.Entry(s.jobs["sample@example.test"]).Job
		s.mu.RUnlock()
		s.RemoveAccount("sample@example.test")
		old.Run() // cron may already have dispatched this callback
		assert.Zero(t, calls.Load(), "removal invalidates dispatched callbacks")
		<-s.Stop().Done()
	})
}

func TestAccountPollingReplacedGateWaiterCannotIngest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		gate := newSerialWorkTracker()
		hold, ok := gate.BeginWork()
		require.True(ok)
		var calls atomic.Int32
		s := pollingSchedulerFixture(t, func(context.Context, string) error { calls.Add(1); return nil }).WithWorkTracker(gate)
		defer func() { hold(); <-s.Stop().Done() }()
		go s.onAccountTick("sample@example.test")
		synctest.Wait()
		require.True(s.Status()[0].Queued)
		s.RemoveAccount("sample@example.test")
		require.NoError(s.AddAccount("sample@example.test", "0 0 1 1 *"))
		require.NoError(s.TriggerSync("sample@example.test", false), "replacement has its own reservation")
		synctest.Wait()
		hold()
		synctest.Wait()
		assert.EqualValues(1, calls.Load(), "obsolete gate waiter must not ingest")
		assert.False(s.Status()[0].Running)
		assert.False(s.Status()[0].Pending)
	})
}

func TestAccountPollingOldFinisherCannotClearReplacementRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		s := pollingSchedulerFixture(t, func(context.Context, string) error {
			if calls.Add(1) == 1 {
				<-first
			} else {
				<-second
			}
			return nil
		})
		go s.onAccountTick("sample@example.test")
		synctest.Wait()
		s.RemoveAccount("sample@example.test")
		require.NoError(t, s.AddAccount("sample@example.test", "0 0 1 1 *"))
		err := s.TriggerSync("sample@example.test", false)
		if !assert.NoError(t, err) {
			close(first)
			close(second)
			<-s.Stop().Done()
			return
		}
		synctest.Wait()
		close(first)
		synctest.Wait()
		assert.True(t, s.Status()[0].Running, "old finisher cannot clear a replacement's executing status")
		close(second)
		synctest.Wait()
		<-s.Stop().Done()
	})
}
