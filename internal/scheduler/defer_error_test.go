package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
)

func TestDeferErrorPreservesJoinedFailuresWithoutFollowUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		sched := New(nil)
		defer func() { <-sched.Stop().Done() }()
		first := errors.New("synthetic retention failure")
		second := errors.New("synthetic independent failure")
		calls := 0
		require.NoError(sched.AddJob(Job{Name: "completed", Schedule: "0 0 1 1 *", MaxRuntime: time.Minute, Run: func(ctx context.Context) error {
			calls++
			if calls > 1 {
				return ErrDeferUntilNextTrigger
			}
			jobctx.RecordProgress(ctx)
			<-ctx.Done()
			return errors.Join(fmt.Errorf("completed pass: %w", &DeferError{Err: first}), second)
		}}))
		err := sched.TriggerJob("completed")
		require.ErrorIs(err, first)
		require.ErrorIs(err, second)
		synctest.Wait()
		assert.Equal(1, calls)
		status := sched.JobStatus()
		require.Len(status, 1)
		assert.False(status[0].Running)
		assert.False(status[0].Pending)
		assert.Equal(err.Error(), status[0].LastError, "retain the complete joined error")
	})
}
