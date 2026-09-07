package embed

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
)

func TestRunResultFromOperationRunPreservesTerminalReplayOutcome(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 8, 30, 15, 0, 0, 0, time.UTC)
	finished := started.Add(time.Second)
	trigger := operations.TriggerScheduled
	id, err := operations.NewInt64ID(operations.KindMessageEmbedding, 7)
	require.NoError(t, err)
	tests := []struct {
		name      string
		state     operations.State
		counters  operations.InvocationCounters
		publicErr *operations.PublicError
		wantIs    error
		wantErr   bool
	}{
		{name: "success", state: operations.StateSucceeded,
			counters: operations.InvocationCounters{Attempted: 1, Succeeded: 1}},
		{name: "partial", state: operations.StatePartial,
			counters:  operations.InvocationCounters{Attempted: 2, Succeeded: 1, Failed: 1},
			publicErr: operations.FixedPublicError(operations.PublicErrorInvocationUpstreamFailed)},
		{name: "failed", state: operations.StateFailed,
			counters:  operations.InvocationCounters{Attempted: 1, Failed: 1},
			publicErr: operations.FixedPublicError(operations.PublicErrorInvocationUpstreamFailed), wantErr: true},
		{name: "cancelled", state: operations.StateCancelled,
			publicErr: operations.FixedPublicError(operations.PublicErrorInvocationCancelled),
			wantIs:    context.Canceled, wantErr: true},
		{name: "timed out", state: operations.StateFailed,
			publicErr: operations.FixedPublicError(operations.PublicErrorInvocationTimeout),
			wantIs:    context.DeadlineExceeded, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			run := &operations.Run{
				ID: id, Lane: operations.LaneMessages, State: test.state, Trigger: &trigger,
				StartedAt: started, FinishedAt: &finished,
				Counters: test.counters.PublicCounters(operations.KindMessageEmbedding), Error: test.publicErr,
			}
			result, replayErr := runResultFromOperationRun(run)
			assert.Equal(int(test.counters.Attempted), result.Claimed)
			assert.Equal(int(test.counters.Succeeded), result.Succeeded)
			assert.Equal(int(test.counters.Failed), result.Failed)
			if test.wantErr {
				require.Error(replayErr)
				if test.wantIs != nil {
					require.ErrorIs(replayErr, test.wantIs)
				}
			} else {
				require.NoError(replayErr)
			}
		})
	}
}
