package cmd

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/scheduler"
)

// Other schedules keep queuing work, so the scheduler asks the Beeper job to
// yield after every minute it holds the gate. A full pass needs several
// minutes, and one account alone needs more than a minute. The pass must still
// finish once, without cancelling an account mid-import, and the yield
// follow-ups must stop until the next trigger.
func TestScheduledBeeperPassFinishesWhileQueuedWorkKeepsAskingItToYield(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		gate := api.NewSerialOperationGate()
		sched := scheduler.New(nil).
			WithWorkTracker(labelWorkTracker(gate, "a scheduled sync")).
			WithLogger(testDiscardLogger())
		defer func() { <-sched.Stop().Done() }()

		const unit = 10 * time.Second
		accounts := []string{"large"}
		backlog := map[string]int{"large": 30} // five minutes of pages
		for i := range 8 {
			accounts = append(accounts, fmt.Sprintf("small-%d", i))
		}
		committed := map[string]int{}
		completed := map[string]int{}
		cancelled := 0
		runs := 0
		rotation := &beeperAccountRotation{}
		require.NoError(registerScheduledBeeperJob(sched, "0 0 1 1 *", nil, func(ctx context.Context) error {
			runs++
			shouldStop := func() bool { return jobctx.PreemptionRequested(ctx) }
			return runScheduledBeeperAttempts(ctx, accounts, rotation, shouldStop, func(id string) (bool, error) {
				// Every visit rechecks recent history before the backlog.
				for done := false; !done; {
					select {
					case <-ctx.Done():
						cancelled++
						return false, ctx.Err()
					case <-time.After(unit):
					}
					if committed[id] < backlog[id] {
						committed[id]++
					} else {
						done = true
					}
					if !done && shouldStop() {
						return true, nil
					}
				}
				completed[id]++
				return false, nil
			}, func() error { return nil })
		}))
		require.NoError(sched.AddJob(scheduler.Job{Name: "other", Schedule: "0 0 1 1 *",
			Run: func(context.Context) error {
				time.Sleep(time.Second)
				return nil
			}}))

		_, err := sched.StartJob(api.BeeperJobName, false)
		require.NoError(err)
		queuing := make(chan struct{})
		go func() {
			for {
				select {
				case <-queuing:
					return
				case <-time.After(5 * time.Second):
					_, _ = sched.StartJob("other", false)
				}
			}
		}()
		time.Sleep(time.Hour)
		close(queuing)
		synctest.Wait()

		for _, id := range accounts {
			assert.Equal(1, completed[id], "account %s finishes exactly one pass", id)
		}
		assert.Equal(backlog["large"], committed["large"])
		assert.Zero(cancelled, "a yield stops at a boundary instead of cancelling the import")
		assert.Less(runs, 15, "follow-ups end with the pass")
		var status scheduler.JobStatus
		for _, job := range sched.JobStatus() {
			if job.Name == api.BeeperJobName {
				status = job
			}
		}
		assert.False(status.Running)
		assert.False(status.Pending, "a finished pass waits for the next trigger")
		assert.Empty(status.LastError)
		assert.False(status.LastRun.IsZero())
	})
}

func TestScheduledBeeperFinishedPassKeepsFailuresAfterYield(t *testing.T) {
	require := require.New(t)
	ctx, requestPreemption := jobctx.WithPreemption(t.Context())
	requestPreemption()
	outage := errors.New("synthetic outage")
	err := runScheduledBeeperAttempts(ctx, []string{"a", "b"}, &beeperAccountRotation{}, nil,
		func(id string) (bool, error) {
			if id == "a" {
				return false, outage
			}
			return false, nil
		}, func() error { return nil })
	require.ErrorIs(err, scheduler.ErrDeferUntilNextTrigger)
	require.ErrorIs(err, outage)
}
