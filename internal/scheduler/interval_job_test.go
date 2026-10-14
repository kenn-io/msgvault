package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
)

// Internal maintenance needs second-based intervals without widening the
// five-field cron syntax accepted for user-configured schedules.
func TestSchedulerGenericIntervalJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		s := New(nil)
		var calls atomic.Int64
		require.NoError(s.AddJob(Job{Name: "synthetic-interval", Interval: 5 * time.Second,
			Run: func(context.Context) error { calls.Add(1); return nil }}))
		require.Error(s.AddAccount("synthetic@example.test", "@every 5s"))
		require.Error(s.AddJob(Job{Name: "configured", Schedule: "@every 5s",
			Run: func(context.Context) error { return nil }}), "a configured schedule string cannot select an interval")
		assert.Equal("@every 5s", s.JobStatus()[0].Schedule)
		s.Start()
		t.Cleanup(func() { <-s.Stop().Done() })
		time.Sleep(4 * time.Second)
		synctest.Wait()
		assert.Zero(calls.Load())
		time.Sleep(time.Second)
		synctest.Wait()
		assert.Equal(int64(1), calls.Load())
		time.Sleep(5 * time.Second)
		synctest.Wait()
		assert.Equal(int64(2), calls.Load())
	})
}

func TestSchedulerGenericIntervalRejectsInvalid(t *testing.T) {
	for _, job := range []Job{
		{Interval: -time.Second}, {Interval: 500 * time.Millisecond}, {Interval: 1500 * time.Millisecond},
		{Interval: 5 * time.Second, Schedule: "0 0 * * *"}, {Schedule: "@every 5s"}, {Schedule: "@daily"},
	} {
		t.Run(job.Interval.String()+" "+job.Schedule, func(t *testing.T) {
			s := New(nil)
			job.Name, job.Run = "synthetic-interval", func(context.Context) error { return nil }
			err := s.AddJob(job)
			require.Error(t, err)
			_, err = s.StartJob("synthetic-interval")
			assert.Error(t, err, "invalid interval cannot leave a registered job")
		})
	}
}

// A background job queued behind a long preemptible holder must not ask it to
// yield; otherwise five-second maintenance interrupts every long sync.
func TestBackgroundJobDoesNotPreemptHolder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		oldPoll := yieldPollInterval
		yieldPollInterval = time.Second
		defer func() { yieldPollInterval = oldPoll }()

		preempted := make(chan bool, 1)
		var maintenance atomic.Int64
		s := New(nil).WithWorkTracker(newSerialWorkTracker())
		defer func() { <-s.Stop().Done() }()
		require.NoError(s.AddJob(Job{Name: "long", Preemptible: true, Schedule: "0 0 1 1 *", Run: func(ctx context.Context) error {
			time.Sleep(3 * preemptAfter)
			preempted <- jobctx.PreemptionRequested(ctx)
			return nil
		}}))
		require.NoError(s.AddJob(Job{Name: "maintenance", Background: true, Schedule: "0 0 1 1 *", Run: func(context.Context) error {
			maintenance.Add(1)
			return nil
		}}))
		go s.onJobTick("long")
		synctest.Wait()
		go s.onJobTick("maintenance")
		time.Sleep(4 * preemptAfter)
		synctest.Wait()
		assert.False(<-preempted, "background maintenance waited without asking the holder to yield")
		assert.Equal(int64(1), maintenance.Load(), "background maintenance runs after the holder finishes")
	})
}
