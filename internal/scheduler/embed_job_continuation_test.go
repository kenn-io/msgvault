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
	for _, mode := range []string{"contextual", "ordinary"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var events []string
			stopped, requestStop := jobctx.WithPreemption(t.Context())
			runner := &fakeRunner{
				backstopResult: embed.RunResult{Contextual: &embed.ContextConvergence{}},
				onRunContext:   func(context.Context) error { events = append(events, "forward"); return nil },
				onBackstop:     func() { events = append(events, "backstop"); requestStop() },
			}
			if mode == "ordinary" {
				runner.backstopResult = embed.RunResult{ScanComplete: true}
			}
			now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
			job := &EmbedJob{
				Worker: runner, Backend: &fakeBackend{active: vector.Generation{ID: 1}},
				BackstopInterval: time.Hour, Now: func() time.Time { return now },
			}
			if mode == "contextual" {
				require.ErrorIs(job.Run(stopped), ErrReschedule)
			} else {
				require.NoError(job.Run(stopped))
			}
			backstops, _ := runner.backstops()
			assert.Equal(1, backstops)
			assert.Equal([]string{"forward", "backstop"}, events, "new mail runs before the due full scan")
			if mode == "contextual" {
				next, requestStop := jobctx.WithPreemption(t.Context())
				runner.onRunContext = func(context.Context) error { requestStop(); return nil }
				runner.runOnceResult = embed.RunResult{Contextual: &embed.ContextConvergence{}}
				require.ErrorIs(job.Run(next), ErrReschedule)
				backstops, _ = runner.backstops()
				assert.Equal(1, backstops, "an unfinished forward continuation preserves the backstop cursor")
			}
			runner.onRunContext = nil
			runner.onBackstop = nil
			if mode == "contextual" {
				// The next forward pass completes the interrupted reconciliation.
				runner.runOnceResult = embed.RunResult{Contextual: &embed.ContextConvergence{ReconcileComplete: true}}
			}
			now = now.Add(time.Minute)
			require.NoError(job.Run(t.Context()))
			backstops, _ = runner.backstops()
			assert.Equal(1, backstops, "the completed backstop satisfies the interval instead of restarting the scan")

			now = now.Add(time.Hour)
			require.NoError(job.Run(t.Context()))
			backstops, _ = runner.backstops()
			assert.Equal(2, backstops, "the next interval starts a fresh backstop")
		})
	}
}

func TestEmbedJob_BackstopYieldWithPendingPeopleDoesNotRecordCompletion(t *testing.T) {
	ctx, requestYield := jobctx.WithPreemption(t.Context())
	runner := &fakeRunner{
		backstopResult: embed.RunResult{ScanComplete: true, PersonsPending: true},
		onBackstop:     requestYield,
	}
	job := &EmbedJob{Worker: runner, BackstopInterval: time.Hour}

	ran, err := job.maybeRunBackstop(ctx, 7, slog.Default(), time.Now().UTC())

	require.True(t, ran)
	require.ErrorIs(t, err, ErrReschedule)
	_, recorded := job.lastBackstop[7]
	assert.False(t, recorded, "pending person maintenance must keep the backstop incomplete")
}

func TestEmbedJobYieldReturnsContinuationBeforeActivation(t *testing.T) {
	providerErr := errors.New("provider unavailable")
	lookupErr := errors.New("generation lookup failed")
	for _, tc := range []struct {
		name           string
		configure      func(*EmbedJob, *fakeBackend, *fakeRunner, func())
		independentErr error
		wantRuns       int
		wantBackstops  int
	}{
		{name: "provider failure", independentErr: providerErr, wantRuns: 1,
			configure: func(_ *EmbedJob, _ *fakeBackend, runner *fakeRunner, yield func()) {
				runner.onRunContext = func(context.Context) error { yield(); return providerErr }
			}},
		{name: "active generation lookup failure", independentErr: lookupErr,
			configure: func(_ *EmbedJob, backend *fakeBackend, _ *fakeRunner, yield func()) {
				backend.activeErr, backend.onActive = lookupErr, yield
			}},
		{name: "convergence", wantRuns: 1,
			configure: func(job *EmbedJob, backend *fakeBackend, _ *fakeRunner, yield func()) {
				backend.building = &vector.Generation{ID: 7, Fingerprint: "m:768"}
				job.SequenceBoundActivation = true
				job.Convergence = &fakeConvergenceChecker{onCheck: yield, result: ConvergenceResult{
					MessageCoverageComplete: true, PersonCoverageComplete: true,
					LatestJournalSequence: 12, ConsumedJournalSequence: 12, ReconciliationComplete: true,
				}}
			}},
		{name: "completed building scan", wantRuns: 1,
			configure: func(_ *EmbedJob, backend *fakeBackend, runner *fakeRunner, yield func()) {
				backend.building = &vector.Generation{ID: 7, State: vector.GenerationBuilding, Fingerprint: "m:768"}
				runner.runOnceResult = embed.RunResult{ScanComplete: true,
					Contextual: &embed.ContextConvergence{Converged: true, ReconcileComplete: true}}
				runner.onRunContext = func(context.Context) error { yield(); return nil }
			}},
		{name: "completed building backstop", wantRuns: 1, wantBackstops: 1,
			configure: func(job *EmbedJob, backend *fakeBackend, runner *fakeRunner, yield func()) {
				backend.building = &vector.Generation{ID: 7, State: vector.GenerationBuilding, Fingerprint: "m:768"}
				runner.runOnceResult = embed.RunResult{ScanComplete: true}
				runner.backstopResult = embed.RunResult{ScanComplete: true}
				runner.onBackstop = yield
				job.BackstopInterval, job.Store = time.Hour, &fakeCoverage{}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			ctx, yield := jobctx.WithPreemption(t.Context())
			backend := &fakeBackend{active: vector.Generation{ID: 1, Fingerprint: "m:768"}}
			runner := &fakeRunner{}
			job := &EmbedJob{Worker: runner, Backend: backend, Fingerprint: "m:768", BackstopInterval: -1}
			tc.configure(job, backend, runner, yield)
			err := job.Run(ctx)
			require.ErrorIs(t, err, ErrReschedule)
			if tc.independentErr != nil {
				require.ErrorIs(t, err, tc.independentErr)
			}
			assert.Empty(backend.activations(), "yield leaves activation for the next pass")
			assert.Equal(tc.wantRuns, runner.runCalls)
			backstops, _ := runner.backstops()
			assert.Equal(tc.wantBackstops, backstops)
		})
	}
}
