package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// batchRunner publishes one batch at a time and, like the production
// workers, stops before a new batch once a stop is requested.
type batchRunner struct {
	log       *eventLog
	publish   func()
	total     int
	committed int
}

func (r *batchRunner) ReclaimStale(context.Context) (int, error) { return 0, nil }

func (r *batchRunner) RunOnce(ctx context.Context, _ vector.GenerationID, _ operations.PassScope) (embed.RunResult, error) {
	for batches := 0; r.committed < r.total; batches++ {
		if batches > 0 && jobctx.PreemptionRequested(ctx) {
			return embed.RunResult{}, nil
		}
		r.publish()
		if err := ctx.Err(); err != nil {
			r.log.add("batch interrupted")
			return embed.RunResult{}, err
		}
		r.committed++
		r.log.add(fmt.Sprintf("batch %d", r.committed))
		jobctx.RecordProgress(ctx)
	}
	return embed.RunResult{}, nil
}

func (r *batchRunner) RunBackstop(context.Context, vector.GenerationID, operations.PassScope) (embed.RunResult, error) {
	return embed.RunResult{}, nil
}

func TestQueuedSyncGetsGateAfterIndexingPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		events := &eventLog{}
		s := New(func(context.Context, string) error {
			events.add("sync")
			return nil
		}).WithWorkTracker(newSerialWorkTracker()).WithLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
		defer func() { <-s.Stop().Done() }()
		runner := &batchRunner{log: events, total: 3, publish: func() { time.Sleep(10 * time.Second) }}
		job := &EmbedJob{Worker: runner, Backend: &fakeBackend{active: vector.Generation{ID: 1}}, BackstopInterval: -1}
		require.NoError(s.SetEmbedJob(job, "", false))
		require.NoError(s.AddAccount("user-a@example.com", "0 0 1 1 *"))

		_, err := s.StartJob(embedJobName)
		require.NoError(err)
		time.Sleep(3 * time.Second)
		queuedAt := time.Now()
		require.NoError(s.TriggerSync("user-a@example.com"))
		synctest.Wait()
		statuses := s.Status()
		require.Len(statuses, 1)
		assert.True(statuses[0].Queued)
		assert.Equal(queuedAt, statuses[0].QueuedAt)

		time.Sleep(time.Minute)
		synctest.Wait()
		assert.Equal([]string{"batch 1", "sync", "batch 2", "batch 3"}, events.list(),
			"the sync waits for the batch in progress, then indexing resumes after it")
		assert.Equal("idle", s.EmbeddingStatus().Job.State)
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

func TestSchedulerShutdownCancelsCooperativeJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		s := New(nil).WithLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
		started := make(chan struct{})
		var cause error
		require.NoError(s.AddJob(Job{Name: "indexing", Schedule: "0 0 1 1 *", Cooperative: true, MaxRuntime: time.Minute,
			Run: func(ctx context.Context) error {
				close(started)
				<-ctx.Done()
				cause = ctx.Err()
				return cause
			}}))
		_, err := s.StartJob("indexing")
		require.NoError(err)
		<-started
		<-s.Stop().Done()
		require.ErrorIs(cause, context.Canceled)
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

func TestInterruptedContextualBackstopCompletionCountsForInterval(t *testing.T) {
	ctx, requestStop := jobctx.WithPreemption(jobctx.WithProgress(t.Context()))
	runner := &fakeRunner{
		backstopResult: embed.RunResult{Contextual: &embed.ContextConvergence{}},
		onBackstop:     requestStop,
	}
	now := time.Now()
	job := &EmbedJob{
		Worker: runner, Backend: &fakeBackend{active: vector.Generation{ID: 1}},
		BackstopInterval: time.Hour, Now: func() time.Time { return now },
	}
	job.Run(ctx)
	n, _ := runner.backstops()
	require.Equal(t, 1, n)
	assert.Empty(t, job.lastBackstop)

	// ContextWorker.RunOnce resumes the durable cursor and returns done.
	runner.runOnceResult = embed.RunResult{Contextual: &embed.ContextConvergence{ReconcileComplete: true}}
	runner.onBackstop = nil
	now = now.Add(time.Minute)
	job.Run(t.Context())
	n, _ = runner.backstops()
	assert.Equal(t, 1, n, "the resumed scan must satisfy this interval instead of resetting its completed cursor")
	assert.WithinDuration(t, now, job.lastBackstop[1], 0)

	now = now.Add(time.Hour)
	job.Run(t.Context())
	n, _ = runner.backstops()
	assert.Equal(t, 2, n, "the next interval still starts a fresh backstop")
}

func TestCompletedContextualBackstopCountsDespiteStopRequest(t *testing.T) {
	ctx, requestStop := jobctx.WithPreemption(jobctx.WithProgress(t.Context()))
	runner := &fakeRunner{
		backstopResult: embed.RunResult{Contextual: &embed.ContextConvergence{ReconcileComplete: true}},
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
	assert.WithinDuration(t, now, job.lastBackstop[1], 0)
	assert.Empty(t, job.interruptedBackstop, "a completed backstop clears an earlier interruption")

	runner.onBackstop = nil
	now = now.Add(time.Minute)
	job.Run(t.Context())
	n, _ = runner.backstops()
	assert.Equal(t, 1, n, "the continuation must preserve the completed cursor until the next interval")
}
