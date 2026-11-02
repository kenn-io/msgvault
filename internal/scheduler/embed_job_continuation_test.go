package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

func TestEmbedJobCountsResumedContextualBackstopCompletion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var events []string
	stopped, requestStop := jobctx.WithPreemption(t.Context())
	runner := &fakeRunner{
		backstopResult: embed.RunResult{Contextual: &embed.ContextConvergence{}},
		onRunOnce:      func(vector.GenerationID) { events = append(events, "forward") },
		onBackstop:     func() { events = append(events, "backstop"); requestStop() },
	}
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	job := &EmbedJob{
		Worker: runner, Backend: &fakeBackend{active: vector.Generation{ID: 1}},
		BackstopInterval: time.Hour, Now: func() time.Time { return now },
	}
	require.NoError(job.Run(stopped))
	assert.Equal([]string{"forward", "backstop"}, events, "new mail runs before the due full scan")

	next, requestStop := jobctx.WithPreemption(t.Context())
	runner.onRunOnce = func(vector.GenerationID) { requestStop() }
	runner.runOnceResult = embed.RunResult{Contextual: &embed.ContextConvergence{}}
	require.NoError(job.Run(next))
	backstops, _ := runner.backstops()
	assert.Equal(1, backstops, "an unfinished forward continuation preserves the backstop cursor")

	// The next forward pass completes the interrupted reconciliation.
	runner.onRunOnce, runner.onBackstop = nil, nil
	runner.runOnceResult = embed.RunResult{Contextual: &embed.ContextConvergence{ReconcileComplete: true}}
	now = now.Add(time.Minute)
	require.NoError(job.Run(t.Context()))
	backstops, _ = runner.backstops()
	assert.Equal(1, backstops, "the completed reconciliation satisfies the interval instead of restarting the scan")

	now = now.Add(time.Hour)
	require.NoError(job.Run(t.Context()))
	backstops, _ = runner.backstops()
	assert.Equal(2, backstops, "the next interval starts a fresh backstop")
}

func TestEmbedJobStoppedBackstopRecordsOnlyCompletedWork(t *testing.T) {
	for _, tc := range []struct {
		name     string
		result   embed.RunResult
		recorded bool
	}{
		{name: "completed scan", result: embed.RunResult{ScanComplete: true}, recorded: true},
		{name: "pending people", result: embed.RunResult{ScanComplete: true, PersonsPending: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, requestYield := jobctx.WithPreemption(t.Context())
			job := &EmbedJob{Worker: &fakeRunner{backstopResult: tc.result, onBackstop: requestYield}, BackstopInterval: time.Hour}

			ran, err := job.maybeRunBackstop(ctx, 7, slog.Default(), time.Now().UTC())

			require.True(t, ran)
			_, recorded := job.lastBackstop[7]
			assert.Equal(t, tc.recorded, recorded, "only a finished backstop satisfies the interval")
			require.NoError(t, err)
		})
	}
}

func TestEmbedJobStopReturnsFailuresAndDefersActivation(t *testing.T) {
	providerErr := errors.New("provider unavailable")
	lookupErr := errors.New("generation lookup failed")
	for _, tc := range []struct {
		name           string
		configure      func(*EmbedJob, *fakeBackend, *fakeRunner, context.CancelCauseFunc)
		independentErr error
		wantRuns       int
		wantBackstops  int
	}{
		{name: "provider failure", independentErr: providerErr, wantRuns: 1,
			configure: func(_ *EmbedJob, _ *fakeBackend, runner *fakeRunner, stop context.CancelCauseFunc) {
				runner.runErr, runner.yieldCancel = providerErr, stop
			}},
		{name: "active generation lookup failure", independentErr: lookupErr,
			configure: func(_ *EmbedJob, backend *fakeBackend, _ *fakeRunner, stop context.CancelCauseFunc) {
				backend.activeErr, backend.yieldActive = lookupErr, stop
			}},
		{name: "convergence", wantRuns: 1,
			configure: func(job *EmbedJob, backend *fakeBackend, _ *fakeRunner, stop context.CancelCauseFunc) {
				backend.building = &vector.Generation{ID: 7, Fingerprint: "m:768"}
				job.SequenceBoundActivation = true
				job.Convergence = &fakeConvergenceChecker{cancel: stop, result: ConvergenceResult{
					MessageCoverageComplete: true, PersonCoverageComplete: true,
					LatestJournalSequence: 12, ConsumedJournalSequence: 12, ReconciliationComplete: true,
				}}
			}},
		{name: "completed building scan", wantRuns: 1,
			configure: func(_ *EmbedJob, backend *fakeBackend, runner *fakeRunner, stop context.CancelCauseFunc) {
				backend.building = &vector.Generation{ID: 7, State: vector.GenerationBuilding, Fingerprint: "m:768"}
				runner.runOnceResult = embed.RunResult{ScanComplete: true,
					Contextual: &embed.ContextConvergence{Converged: true, ReconcileComplete: true}}
				runner.yieldCancel = stop
			}},
		{name: "completed building backstop", wantRuns: 1, wantBackstops: 1,
			configure: func(job *EmbedJob, backend *fakeBackend, runner *fakeRunner, stop context.CancelCauseFunc) {
				backend.building = &vector.Generation{ID: 7, State: vector.GenerationBuilding, Fingerprint: "m:768"}
				runner.runOnceResult = embed.RunResult{ScanComplete: true}
				runner.backstopResult = embed.RunResult{ScanComplete: true}
				runner.yieldBackstop = stop
				job.BackstopInterval, job.Store = time.Hour, &fakeCoverage{}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			ctx, stop := context.WithCancelCause(t.Context())
			defer stop(nil)
			backend := &fakeBackend{active: vector.Generation{ID: 1, Fingerprint: "m:768"}}
			runner := &fakeRunner{}
			job := &EmbedJob{Worker: runner, Backend: backend, Fingerprint: "m:768", BackstopInterval: -1}
			tc.configure(job, backend, runner, stop)
			assert.Equal(tc.independentErr, job.Run(ctx), "the scheduler filters the stop and keeps independent failures")
			assert.Empty(backend.activations(), "yield leaves activation for the next pass")
			assert.Equal(tc.wantRuns, runner.runCalls)
			backstops, _ := runner.backstops()
			assert.Equal(tc.wantBackstops, backstops)
		})
	}
}
