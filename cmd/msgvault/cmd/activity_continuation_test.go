package cmd

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	activitypkg "go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

func TestScheduledProjectionDefersChangedIdentityChain(t *testing.T) {
	f := newScheduledProjectionFixture(t)
	synctest.Test(t, func(t *testing.T) {
		sched, waitIdle := startScheduledProjectionWithQueuedSync(t, f, func(ctx context.Context) error {
			return f.Store.AddAccountIdentityContext(ctx, f.Source.ID, "chain-owner@example.com", "manual")
		})
		waitIdle()
		var count int
		require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_events`).Scan(&count))
		assert.Equal(t, 2*activityProjectionMaxBatches, count, "accepted second pass commits another ten messages before deferring")
		current, err := f.Store.ContactRevisionsContext(t.Context())
		require.NoError(t, err)

		require.NoError(t, sched.TriggerJob(activityProjectionJob))
		waitIdle()
		require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_events`).Scan(&count))
		assert.Equal(t, 23, count, "a later trigger drains the stable remainder")
		reconciled, ok, err := f.Store.ActivityReconciledRevisionsContext(t.Context())
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, current, reconciled)
	})
}

// Controlled outcomes cover adapter branches whose cancellation/error combinations
// cannot be produced reliably by database timing. Scheduler contracts live in
// internal/scheduler; these cases check deferral and the adapter's chain baseline.
func TestActivityProjectionContinuationOutcomes(t *testing.T) {
	failure := errors.New("projection failed")
	for _, tc := range []struct {
		name                                                  string
		moved, preempt, waiter, budget, progress, readFailure bool
		runErr, want                                          error
		retain                                                bool
	}{
		{name: "stable backlog", runErr: activitypkg.ErrWorkRemaining, want: scheduler.ErrReschedule, retain: true},
		{name: "moved backlog", moved: true, runErr: activitypkg.ErrWorkRemaining, want: scheduler.ErrDeferUntilNextTrigger},
		{name: "moved nil preemption", moved: true, preempt: true, want: scheduler.ErrDeferUntilNextTrigger},
		{name: "stable nil preemption", preempt: true, retain: true},
		{name: "moved waiter cancellation", moved: true, waiter: true, runErr: context.Canceled, want: scheduler.ErrDeferUntilNextTrigger},
		{name: "moved budget without progress", moved: true, budget: true, runErr: context.DeadlineExceeded, want: context.DeadlineExceeded},
		{name: "moved budget with progress", moved: true, budget: true, progress: true, runErr: context.DeadlineExceeded, want: scheduler.ErrDeferUntilNextTrigger},
		{name: "stable budget with progress", budget: true, progress: true, runErr: context.DeadlineExceeded, want: context.DeadlineExceeded, retain: true},
		{name: "moved projection failure", moved: true, runErr: failure, want: failure},
		{name: "moved projection failure during waiter yield", moved: true, waiter: true, runErr: failure, want: failure, retain: true},
		{name: "ordinary completion", moved: true},
		{name: "revision read failure", moved: true, readFailure: true, want: failure},
		{name: "revision read failure during preemption", moved: true, readFailure: true, preempt: true, want: failure, retain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				revisions := store.ContactRevisions{IdentityRevision: 1}
				runErr := activitypkg.ErrWorkRemaining
				var readErr error
				var interrupt func()
				passes := 0
				run := activityProjectionRun(func(context.Context) (activitypkg.RunResult, error) {
					passes++
					if interrupt != nil {
						interrupt()
					}
					return activitypkg.RunResult{}, runErr
				}, func(context.Context) (store.ContactRevisions, error) { return revisions, readErr }, testDiscardLogger())
				require.ErrorIs(t, run(t.Context()), scheduler.ErrReschedule)
				if tc.moved {
					revisions.IdentityRevision++
				}
				ctx := jobctx.WithProgress(t.Context())
				if tc.preempt {
					var request func()
					ctx, request = jobctx.WithPreemption(ctx)
					request()
				}
				if tc.waiter {
					var cancel context.CancelCauseFunc
					ctx, cancel = context.WithCancelCause(ctx)
					defer cancel(nil)
					interrupt = func() { cancel(jobctx.ErrYieldedToWaiter) }
				}
				if tc.budget {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeoutCause(ctx, time.Second, jobctx.ErrRunBudgetExceeded)
					defer cancel()
					interrupt = func() { time.Sleep(time.Second); synctest.Wait() }
				}
				if tc.progress {
					jobctx.RecordProgress(ctx)
				}
				runErr = tc.runErr
				if tc.readFailure {
					readErr = failure
				}
				got := run(ctx)
				if tc.want == nil {
					assert.NoError(t, got)
				} else {
					assert.ErrorIs(t, got, tc.want)
				}
				if tc.want == scheduler.ErrDeferUntilNextTrigger { //nolint:errorlint // Only bare deferral ends a clean chain.
					assert.Same(t, tc.want, got, "deferral must be bare")
				}
				wantPasses := 2
				if tc.readFailure {
					wantPasses = 1
				}
				assert.Equal(t, wantPasses, passes, "a successful entry read always accepts a pass")
				// A third continuing pass reveals whether the prior outcome ended the chain.
				revisions.AccountIdentityRevision++
				readErr, runErr, interrupt = nil, activitypkg.ErrWorkRemaining, nil
				want := scheduler.ErrReschedule
				if tc.retain {
					want = scheduler.ErrDeferUntilNextTrigger
				}
				assert.Same(t, want, run(t.Context()))
			})
		})
	}
}
