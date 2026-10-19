package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
)

func TestJobResultsExcludeControlFlow(t *testing.T) {
	failure := errors.New("synthetic failure")
	for _, tc := range []struct {
		name    string
		err     error
		preempt bool
		want    int
	}{
		{"success", nil, false, 1}, {"failure", failure, false, 1},
		{"defer", ErrDeferUntilNextTrigger, false, 0}, {"reschedule", ErrReschedule, false, 0},
		{"cancel", context.Canceled, false, 0}, {"deadline", context.DeadlineExceeded, false, 1},
		{"client timeout", fmt.Errorf("HTTP request: %w", context.DeadlineExceeded), false, 1},
		{"yield", ErrYieldedToWaiter, false, 0}, {"preempt", nil, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ctx, requestPreemption := jobctx.WithPreemption(t.Context())
			type testKey struct{}
			key := testKey{}
			sched := New(nil).WithJobResults(func(ctx context.Context, name string) (context.Context, func(error)) {
				return context.WithValue(ctx, key, "synthetic"), func(err error) {
					calls++
					assert.Equal(t, "synthetic", name)
					assert.Equal(t, tc.err, err)
				}
			})
			require.NoError(t, sched.AddJob(Job{Name: "synthetic", Schedule: "0 0 * * *", Run: func(ctx context.Context) error {
				assert.Equal(t, "synthetic", ctx.Value(key))
				if tc.preempt {
					requestPreemption()
				}
				return tc.err
			}, Preemptible: tc.preempt}))
			_ = sched.genericFuncs["synthetic"](ctx)
			assert.Equal(t, tc.want, calls)
		})
	}
}
