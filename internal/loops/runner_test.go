package loops

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/scheduler"
)

func TestRunnerManualCoalescesAcrossGateAndRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		gate := make(chan struct{}, 1)
		started := make(chan struct{}, 2)
		release := make(chan struct{})
		var rebuilds atomic.Int32
		r, err := NewRunner(RunnerOptions{Context: ctx,
			BeginWork: func(ctx context.Context, label string) (func(), bool) {
				assert.Equal(t, "open-loops", label)
				select {
				case <-ctx.Done():
					return func() {}, false
				case <-gate:
					return func() { gate <- struct{}{} }, true
				}
			}, Rebuild: func(context.Context) error { rebuilds.Add(1); return nil },
			Run: func(ctx context.Context) error {
				started <- struct{}{}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
					return nil
				}
			},
		})
		requirements.NoError(err)
		disp, err := r.Start(false)
		requirements.NoError(err)
		assertions.Equal(scheduler.JobStarted, disp)
		synctest.Wait()
		disp, err = r.Start(true)
		requirements.NoError(err)
		assertions.Equal(scheduler.JobCoalesced, disp, "request while gate queued shares initial pass")
		gate <- struct{}{}
		<-started
		assertions.Equal(int32(1), rebuilds.Load())
		disp, err = r.Start(false)
		requirements.NoError(err)
		assertions.Equal(scheduler.JobPending, disp)
		disp, err = r.Start(true)
		requirements.NoError(err)
		assertions.Equal(scheduler.JobCoalesced, disp)
		release <- struct{}{}
		<-started
		assertions.Equal(int32(2), rebuilds.Load(), "rebuild arriving during a run survives to the follow-up")
		cancel()
		synctest.Wait()
		_, err = r.Start(false)
		requirements.ErrorIs(err, context.Canceled)
	})
}

func TestRunnerScheduledUsesAsyncSchedulerAndPreservesRebuild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		sched := scheduler.New(func(context.Context, string) error { return nil })
		release := make(chan struct{})
		started := make(chan struct{}, 2)
		var rebuilds atomic.Int32
		r, err := NewRunner(RunnerOptions{Context: t.Context(),
			StartScheduled: func() (scheduler.JobDisposition, error) { return sched.StartJob("open-loops") },
			Rebuild:        func(context.Context) error { rebuilds.Add(1); return nil },
			Run:            func(context.Context) error { started <- struct{}{}; <-release; return nil },
		})
		requirements.NoError(err)
		requirements.NoError(sched.AddJob(scheduler.Job{Name: "open-loops", Schedule: "0 0 1 1 *", Run: r.RunScheduled}))
		t.Cleanup(func() { close(release); <-sched.Stop().Done() })
		disp, err := r.Start(false)
		requirements.NoError(err)
		assertions.Equal(scheduler.JobStarted, disp)
		<-started
		disp, err = r.Start(true)
		requirements.NoError(err)
		assertions.Equal(scheduler.JobPending, disp)
		release <- struct{}{}
		<-started
		assertions.Equal(int32(1), rebuilds.Load())
	})
}
