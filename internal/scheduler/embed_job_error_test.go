package scheduler

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/vector"
)

type convergenceCheckerFunc func(context.Context, vector.GenerationID) (ConvergenceResult, error)

func (f convergenceCheckerFunc) CheckConvergence(ctx context.Context, gen vector.GenerationID) (ConvergenceResult, error) {
	return f(ctx, gen)
}

func TestEmbedJobRunPreservesErrorsDuringInterruption(t *testing.T) {
	failure := errors.New("independent database failure")
	for _, test := range []struct {
		name      string
		configure func(*EmbedJob, *fakeBackend, context.CancelCauseFunc)
	}{
		{"target building lookup", func(job *EmbedJob, backend *fakeBackend, interrupt context.CancelCauseFunc) {
			job.Fingerprint = ""
			backend.buildErr, backend.yieldBuilding = failure, interrupt
		}},
		{"target active lookup", func(job *EmbedJob, backend *fakeBackend, interrupt context.CancelCauseFunc) {
			job.Fingerprint = ""
			backend.building = nil
			backend.activeErr, backend.yieldActive = failure, interrupt
		}},
		{"person building lookup", func(_ *EmbedJob, backend *fakeBackend, interrupt context.CancelCauseFunc) {
			backend.buildErr, backend.yieldBuilding = failure, interrupt
		}},
		{"person active lookup", func(_ *EmbedJob, backend *fakeBackend, interrupt context.CancelCauseFunc) {
			backend.activeErr, backend.yieldActive = failure, interrupt
		}},
		{"convergence before backstop", func(job *EmbedJob, _ *fakeBackend, interrupt context.CancelCauseFunc) {
			job.Convergence = &fakeConvergenceChecker{err: failure, cancel: interrupt}
		}},
		{"convergence after backstop", func(job *EmbedJob, _ *fakeBackend, interrupt context.CancelCauseFunc) {
			job.BackstopInterval = 0
			checks := 0
			job.Convergence = convergenceCheckerFunc(func(context.Context, vector.GenerationID) (ConvergenceResult, error) {
				checks++
				if checks == 1 {
					return ConvergenceResult{}, nil
				}
				interrupt(jobctx.ErrYieldedToWaiter)
				return ConvergenceResult{}, failure
			})
		}},
		{"coverage", func(job *EmbedJob, _ *fakeBackend, interrupt context.CancelCauseFunc) {
			job.Store = &fakeCoverage{err: failure, yieldCancel: interrupt}
		}},
		{"activation", func(_ *EmbedJob, backend *fakeBackend, interrupt context.CancelCauseFunc) {
			backend.activateErr, backend.yieldActivate = failure, interrupt
		}},
		{"sequence-bound activation", func(job *EmbedJob, backend *fakeBackend, interrupt context.CancelCauseFunc) {
			backend.activateErr, backend.yieldActivate = failure, interrupt
			job.SequenceBoundActivation = true
			job.Convergence = &fakeConvergenceChecker{result: ConvergenceResult{
				MessageCoverageComplete: true, PersonCoverageComplete: true, ReconciliationComplete: true,
			}}
		}},
	} {
		for _, interruption := range []string{"cooperative", "cancelled"} {
			t.Run(test.name+"/"+interruption, func(t *testing.T) {
				require := require.New(t)
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				ctx, request := jobctx.WithPreemption(ctx)
				interrupt := cancel
				if interruption == "cooperative" {
					interrupt = func(error) { request() }
				}
				backend := &fakeBackend{
					building:  &vector.Generation{ID: 9, State: vector.GenerationBuilding, Fingerprint: "m:768"},
					activeErr: vector.ErrNoActiveGeneration,
				}
				job := &EmbedJob{
					Worker: &fakeRunner{}, Backend: backend, Store: &fakeCoverage{},
					Fingerprint: "m:768", BackstopInterval: -1,
				}
				test.configure(job, backend, interrupt)

				err := job.run(ctx)

				require.ErrorIs(err, failure)
				require.ErrorIs(jobctx.ErrorAfterYield(ctx, err), failure)
				if interruption == "cooperative" {
					assert.True(t, jobctx.PreemptionRequested(ctx))
					require.NoError(ctx.Err())
				} else {
					require.ErrorIs(context.Cause(ctx), jobctx.ErrYieldedToWaiter)
				}
			})
		}
	}
}

func TestEmbeddingPreemptionRecordsConvergenceFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		s := New(nil).WithWorkTracker(newSerialWorkTracker())
		resume := make(chan struct{})
		defer func() {
			close(resume)
			<-s.Stop().Done()
		}()
		failure := errors.New("independent convergence database failure")
		queryResult := make(chan struct{})
		var queryCtx context.Context
		runs, checks := 0, 0
		job := &EmbedJob{
			Worker: &policyEmbedRunner{run: func(context.Context) error {
				runs++
				if runs > 1 {
					<-resume
				}
				return nil
			}},
			Backend: &fakeBackend{
				building: &vector.Generation{ID: 9, State: vector.GenerationBuilding, Fingerprint: "m:768"},
			},
			Fingerprint: "m:768",
			Convergence: convergenceCheckerFunc(func(ctx context.Context, _ vector.GenerationID) (ConvergenceResult, error) {
				checks++
				if checks != 2 {
					return ConvergenceResult{}, nil
				}
				queryCtx = ctx
				select {
				case <-queryResult:
				case <-ctx.Done():
					return ConvergenceResult{}, ctx.Err()
				}
				return ConvergenceResult{}, failure
			}),
		}
		require.NoError(s.SetEmbedJob(job, "0 0 1 1 *", false))
		mustStartJob(t, s, "embed")
		synctest.Wait()
		require.NoError(s.AddJob(Job{Name: "sync", Schedule: "0 0 1 1 *", Run: func(context.Context) error { return nil }}))
		mustStartJob(t, s, "sync")
		synctest.Wait()
		time.Sleep(preemptAfter)
		synctest.Wait()
		require.NotNil(queryCtx)
		require.True(jobctx.PreemptionRequested(queryCtx))
		require.NoError(queryCtx.Err(), "the query fails before forced cancellation")
		close(queryResult)
		synctest.Wait()

		assert.Equal(2, runs, "embedding resumes after yielding to the queued sync")
		var status JobStatus
		for _, candidate := range s.JobStatus() {
			if candidate.Name == "embed" {
				status = candidate
			}
		}
		assert.Equal(failure.Error(), status.LastError)
	})
}
