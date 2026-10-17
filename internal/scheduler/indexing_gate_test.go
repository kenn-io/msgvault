package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

func TestQueuedSyncGetsGateAfterIndexingPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var events []string
		s := New(func(context.Context, string) error {
			events = append(events, "sync")
			return nil
		}).WithWorkTracker(newSerialWorkTracker())
		defer func() { <-s.Stop().Done() }()
		require.NoError(t, s.AddAccount("user-a@example.com", "0 0 1 1 *"))
		committed := 0
		require.NoError(t, s.AddJob(Job{Name: "indexing", Schedule: "0 0 1 1 *", Cooperative: true, Preemptible: true, Background: true, MaxRuntime: time.Minute,
			Run: func(ctx context.Context) error {
				for committed < 3 {
					if jobctx.HasProgress(ctx) && jobctx.PreemptionRequested(ctx) {
						return nil
					}
					time.Sleep(10 * time.Second)
					committed++
					events = append(events, fmt.Sprintf("batch %d", committed))
					jobctx.RecordProgress(ctx)
				}
				return nil
			}}))
		_, err := s.StartJob("indexing")
		require.NoError(t, err)
		time.Sleep(3 * time.Second)
		require.NoError(t, s.TriggerSync("user-a@example.com"))
		time.Sleep(time.Minute)
		synctest.Wait()
		assert.Equal(t, []string{"batch 1", "sync", "batch 2", "batch 3"}, events)
		assert.False(t, s.JobStatus()[0].Running)
	})
}

func TestCooperativeJobStopsAtPassTargetWithoutCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		s := New(nil).WithLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
		defer func() { <-s.Stop().Done() }()
		var runs []time.Duration
		cancelled := false
		require.NoError(s.AddJob(Job{Name: "indexing", Schedule: "0 0 1 1 *", Cooperative: true, MaxRuntime: time.Minute,
			Run: func(ctx context.Context) error {
				started := time.Now()
				if len(runs) == 0 {
					// Commit 25-second units until the pass target asks to stop.
					for !jobctx.PreemptionRequested(ctx) && ctx.Err() == nil {
						time.Sleep(25 * time.Second)
						jobctx.RecordProgress(ctx)
					}
				} else {
					// Reach the target without committing anything.
					time.Sleep(70 * time.Second)
				}
				cancelled = cancelled || ctx.Err() != nil
				runs = append(runs, time.Since(started))
				return nil
			}}))

		_, err := s.StartJob("indexing")
		require.NoError(err)
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		assert.Equal([]time.Duration{75 * time.Second, 70 * time.Second}, runs,
			"a stop after progress continues once; a stop without progress waits for the next trigger")
		assert.False(cancelled, "the pass target never cancels the publication")
		status := s.JobStatus()[0]
		assert.False(status.Pending)
		assert.Empty(status.LastError)
	})
}

func TestEmbedJob_Run_StopRequestEndsPassAtPhaseBoundary(t *testing.T) {
	assert := assert.New(t)
	building := &vector.Generation{ID: 77, State: vector.GenerationBuilding, Fingerprint: "m:768"}
	backend := &fakeBackend{activeErr: vector.ErrNoActiveGeneration, building: building}
	ctx, requestStop := jobctx.WithPreemption(context.Background())
	runner := &fakeRunner{onRunOnce: func(vector.GenerationID) { requestStop() }}
	coverage := &fakeCoverage{}
	var logs bytes.Buffer
	job := &EmbedJob{
		Worker:      runner,
		Backend:     backend,
		Store:       coverage,
		Fingerprint: "m:768",
		Log:         slog.New(slog.NewTextHandler(&logs, nil)),
	}

	job.Run(ctx)

	_, run, _ := runner.calls()
	assert.Equal(1, run, "RunOnce calls")
	n, _ := runner.backstops()
	assert.Equal(0, n, "RunBackstop calls")
	assert.Equal(0, coverage.calls, "coverage queries")
	assert.Empty(backend.activations(), "activation calls")
	assert.Contains(logs.String(), "embed run complete", "the finished forward pass is still reported")
}

func TestContextualBackstopCompletionCountsForInterval(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprintf("completed=%t", completed), func(t *testing.T) {
			ctx, requestStop := jobctx.WithPreemption(jobctx.WithProgress(t.Context()))
			runner := &fakeRunner{
				backstopResult: embed.RunResult{Contextual: &embed.ContextConvergence{ReconcileComplete: completed}},
				onBackstop:     requestStop,
			}
			now := time.Now()
			job := &EmbedJob{
				Worker: runner, Backend: &fakeBackend{active: vector.Generation{ID: 1}},
				BackstopInterval: time.Hour, Now: func() time.Time { return now },
				interruptedBackstop: map[vector.GenerationID]bool{1: true},
			}
			job.Run(ctx)
			n, _ := runner.backstops()
			require.Equal(t, 1, n)
			if completed {
				assert.WithinDuration(t, now, job.lastBackstop[1], 0)
				assert.Empty(t, job.interruptedBackstop)
			} else {
				assert.Empty(t, job.lastBackstop)
			}
			// The forward pass resumes and completes the durable backstop cursor.
			runner.runOnceResult = embed.RunResult{Contextual: &embed.ContextConvergence{ReconcileComplete: true}}
			runner.onBackstop = nil
			now = now.Add(time.Minute)
			job.Run(t.Context())
			n, _ = runner.backstops()
			assert.Equal(t, 1, n, "completion satisfies the interval instead of restarting the scan")
			if !completed {
				assert.WithinDuration(t, now, job.lastBackstop[1], 0)
			}
			now = now.Add(time.Hour)
			job.Run(t.Context())
			n, _ = runner.backstops()
			assert.Equal(t, 2, n, "the next interval starts a fresh backstop")
		})
	}
}
