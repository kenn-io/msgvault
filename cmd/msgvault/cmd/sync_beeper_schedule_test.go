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
// yield after every minute it holds the gate. One account needs about five
// minutes of pages, more than one turn allows. Every scheduled time must still
// give each account one turn without cancelling an import mid-account, end its
// yield follow-ups once every account has had that turn, and let the large
// account finish across scheduled times.
func TestScheduledBeeperPassesShareTurnsWhileQueuedWorkKeepsAskingToYield(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		gate := api.NewSerialOperationGate()
		sched := scheduler.New(nil).
			WithWorkTracker(labelWorkTracker(gate, "a scheduled sync")).
			WithLogger(testDiscardLogger())
		defer func() { <-sched.Stop().Done() }()

		const unit = 10 * time.Second
		const triggers = 8
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
		for range triggers {
			_, err := sched.StartJob(api.BeeperJobName, false)
			require.NoError(err)
			time.Sleep(30 * time.Minute)
		}
		close(queuing)
		synctest.Wait()

		for _, id := range accounts[1:] {
			assert.Equal(triggers, completed[id], "account %s gets one turn per scheduled time", id)
		}
		assert.Equal(backlog["large"], committed["large"], "the large account finishes across scheduled times")
		assert.Positive(completed["large"])
		assert.Zero(cancelled, "a yield stops at a boundary instead of cancelling the import")
		assert.LessOrEqual(runs, 3*triggers, "follow-ups end once every account has had its turn")
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

func TestScheduledBeeperYieldFollowUpSkipsStoppedAccountUntilNextPass(t *testing.T) {
	assert := assert.New(t)
	rotation := &beeperAccountRotation{}
	accounts := []string{"large", "small"}
	visit := func(visited *[]string) func(string) (bool, error) {
		return func(id string) (bool, error) {
			*visited = append(*visited, id)
			return id == "large", nil // large stops at a boundary on every turn
		}
	}

	ctx, requestPreemption := jobctx.WithPreemption(t.Context())
	requestPreemption()
	var first []string
	require.NoError(t, runScheduledBeeperAttempts(ctx, accounts, rotation, nil, visit(&first), func() error { return nil }))
	assert.Equal([]string{"large"}, first)

	followUp, requestFollowUpPreemption := jobctx.WithPreemption(t.Context())
	requestFollowUpPreemption()
	var second []string
	err := runScheduledBeeperAttempts(followUp, accounts, rotation, nil, visit(&second), func() error { return nil })
	assert.Equal([]string{"small"}, second, "the stopped account already had its turn this pass")
	require.ErrorIs(t, err, scheduler.ErrDeferUntilNextTrigger)

	var next []string
	require.NoError(t, runScheduledBeeperAttempts(t.Context(), accounts, rotation, nil, visit(&next), func() error { return nil }))
	assert.Equal([]string{"large"}, next, "the next pass resumes the stopped account")
}
