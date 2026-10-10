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
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
)

// policyEmbedRunner exercises EmbedJob's real invocation and phase boundaries.
// The callback models a provider call interrupted after a durable checkpoint.
type policyEmbedRunner struct {
	fakeRunner

	run func(context.Context) error
}

func (r *policyEmbedRunner) RunOnce(ctx context.Context, _ vector.GenerationID, _ operations.PassScope) (embed.RunResult, error) {
	return embed.RunResult{}, r.run(ctx)
}

func registerPolicyVectorJob(t *testing.T, s *Scheduler, name string, run func(context.Context) error) {
	t.Helper()
	require := require.New(t)
	if name == "embed" {
		require.NoError(s.SetEmbedJob(&EmbedJob{
			Worker: &policyEmbedRunner{run: run}, Backend: &fakeBackend{active: vector.Generation{ID: 42}}, BackstopInterval: -1,
		}, "0 0 1 1 *", false))
	} else {
		require.NoError(s.SetDocumentVectorJob(run, "0 0 1 1 *", false))
	}
}

func TestAutomaticVectorPassYieldsAndResumes(t *testing.T) {
	for _, name := range []string{"embed", "document-vector"} {
		for _, waiter := range []string{"account", "generic"} {
			t.Run(name+"/"+waiter, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					var events []string
					var started, admitted time.Time
					syncRuns := 0
					syncCompleted := false
					recordWaiter := func(ctx context.Context) error {
						syncRuns++
						admitted = time.Now()
						events = append(events, "sync")
						select {
						case <-time.After(3 * time.Minute):
							syncCompleted = true
							events = append(events, "complete")
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					s := New(func(ctx context.Context, _ string) error { return recordWaiter(ctx) }).WithWorkTracker(newSerialWorkTracker())
					defer func() { <-s.Stop().Done() }()
					runs := 0
					registerPolicyVectorJob(t, s, name, func(ctx context.Context) error {
						runs++
						if runs == 1 {
							started = time.Now()
							events = append(events, "checkpoint")
							jobctx.RecordProgress(ctx)
							<-ctx.Done()
							require.ErrorIs(context.Cause(ctx), ErrYieldedToWaiter)
							return ctx.Err()
						}
						events = append(events, "resume")
						if !syncCompleted {
							jobctx.RecordProgress(ctx)
							<-ctx.Done()
							return ctx.Err()
						}
						return nil
					})
					mustStartJob(t, s, name)
					synctest.Wait()
					if waiter == "account" {
						require.NoError(s.AddAccount("sync@example.test", "0 0 1 1 *"))
						require.NoError(s.TriggerSync("sync@example.test"))
					} else {
						require.NoError(s.AddJob(Job{Name: "carddav", Schedule: "0 0 1 1 *", Preemptible: true, Run: recordWaiter}))
						mustStartJob(t, s, "carddav")
					}
					synctest.Wait()
					time.Sleep(preemptAfter + 2*yieldPollInterval + 3*time.Minute)
					synctest.Wait()
					assert.Equal([]string{"checkpoint", "sync", "complete", "resume"}, events)
					assert.Equal(1, syncRuns)
					assert.LessOrEqual(admitted.Sub(started), preemptAfter+2*yieldPollInterval)
					if waiter == "account" {
						assert.False(s.Status()[0].LastRun.IsZero())
						assert.Empty(s.Status()[0].LastError)
					}
					for _, status := range s.JobStatus() {
						assert.Empty(status.LastError)
						assert.False(status.Queued)
						assert.True(status.QueuedSince.IsZero())
					}
				})
			})
		}
	}
}

func TestGateQueueAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		gate := newSerialWorkTracker()
		release, ok := gate.BeginWork()
		require.True(ok)
		s := New(func(context.Context, string) error { return nil }).WithWorkTracker(gate)
		defer func() { <-s.Stop().Done() }()
		require.NoError(s.AddAccount("queued@example.test", "0 0 1 1 *"))
		require.NoError(s.AddJob(Job{Name: "carddav", Schedule: "0 0 1 1 *", Run: func(context.Context) error { return nil }}))
		since := time.Now()
		require.NoError(s.TriggerSync("queued@example.test"))
		mustStartJob(t, s, "carddav")
		synctest.Wait()
		s.onAccountTick("queued@example.test")
		s.onJobTick("carddav")
		time.Sleep(preemptAfter)
		for range 3 {
			s.onAccountTick("queued@example.test")
			s.onJobTick("carddav")
		}
		assert.Equal(since, s.Status()[0].QueuedSince)
		assert.Equal(since, s.JobStatus()[0].QueuedSince)
		assert.True(s.Status()[0].Queued)
		assert.True(s.JobStatus()[0].Queued)
		release()
		synctest.Wait()
		assert.True(s.Status()[0].QueuedSince.IsZero())
		assert.True(s.JobStatus()[0].QueuedSince.IsZero())
	})
}

func TestPostSyncVectorPassReleasesSourceGate(t *testing.T) {
	for _, name := range []string{"embed", "document-vector"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				s := New(func(context.Context, string) error { return nil }).WithWorkTracker(newSerialWorkTracker())
				defer func() { <-s.Stop().Done() }()
				started := false
				run := func(ctx context.Context) error {
					started = true
					<-ctx.Done()
					return ctx.Err()
				}
				if name == "embed" {
					require.NoError(s.SetEmbedJob(&EmbedJob{Worker: &policyEmbedRunner{run: run}, Backend: &fakeBackend{active: vector.Generation{ID: 42}}, BackstopInterval: -1}, "", true))
				} else {
					require.NoError(s.SetDocumentVectorJob(run, "", true))
				}
				require.NoError(s.AddAccount("source@example.test", "0 0 1 1 *"))
				require.NoError(s.TriggerSync("source@example.test"))
				synctest.Wait()
				require.True(started)
				assert.False(s.Status()[0].Running, "embedding has its own gate reservation")
			})
		})
	}
}

func TestAutomaticEmbeddingReportsWorkerError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		workerErr := errors.New("embedding provider unavailable")
		s := New(nil)
		defer func() { <-s.Stop().Done() }()
		registerPolicyVectorJob(t, s, "embed", func(context.Context) error { return workerErr })
		err := s.TriggerJob("embed")
		require.ErrorIs(err, workerErr)
		statuses := s.JobStatus()
		require.Len(statuses, 1)
		status := statuses[0]
		assert.Equal(workerErr.Error(), status.LastError)
		assert.True(status.LastRun.IsZero())
	})
}

func TestReservedGenericJobKeepsPolicyOnReplacement(t *testing.T) {
	for _, change := range []string{"replaced", "removed"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				s := New(nil)
				defer func() { <-s.Stop().Done() }()
				var calls []string
				require.NoError(s.AddJob(Job{Name: "bounded", Schedule: "0 0 1 1 *", Preemptible: true, MaxRuntime: vectorPassBudget, Run: func(ctx context.Context) error {
					calls = append(calls, "original")
					deadline, bounded := ctx.Deadline()
					assert.True(bounded)
					assert.Equal(vectorPassBudget, time.Until(deadline))
					return ErrReschedule
				}}))
				run, disposition, err := s.reserveGenericJob("bounded", false)
				require.NoError(err)
				require.Equal(JobStarted, disposition)
				if change == "removed" {
					s.RemoveJob("bounded")
				} else {
					require.NoError(s.AddJob(Job{Name: "bounded", Schedule: "0 0 1 1 *", MaxRuntime: 30 * time.Second, Run: func(ctx context.Context) error {
						calls = append(calls, "replacement")
						deadline, bounded := ctx.Deadline()
						assert.True(bounded)
						assert.Equal(30*time.Second, time.Until(deadline))
						return nil
					}}))
				}

				require.NoError(s.runJob("bounded", run))
				synctest.Wait()
				if change == "removed" {
					assert.Equal([]string{"original"}, calls)
					assert.Empty(s.JobStatus())
				} else {
					assert.Equal([]string{"original", "replacement"}, calls)
				}
			})
		})
	}
}

func TestDisabledVectorJobsAreAbsent(t *testing.T) {
	for _, name := range []string{"embed", "document-vector"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s := New(nil)
			t.Cleanup(func() { <-s.Stop().Done() })
			registerPolicyVectorJob(t, s, name, func(context.Context) error { return nil })
			if name == "embed" {
				require.NoError(s.SetEmbedJob(&EmbedJob{}, "", false))
			} else {
				require.NoError(s.SetDocumentVectorJob(func(context.Context) error { return nil }, "", false))
			}
			assert.Empty(s.JobStatus())
			assert.Empty(s.cron.Entries())
			assert.False(s.IsJobScheduled(name))
			assert.Error(s.TriggerJob(name))
		})
	}
}

func TestAutomaticVectorPeersStopWithoutProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		s := New(nil).WithWorkTracker(newSerialWorkTracker())
		defer func() { <-s.Stop().Done() }()
		calls := map[string]int{}
		for _, name := range []string{"embed", "document-vector"} {
			registerPolicyVectorJob(t, s, name, func(ctx context.Context) error {
				calls[name]++
				<-ctx.Done()
				return ctx.Err()
			})
		}
		mustStartJob(t, s, "embed")
		synctest.Wait()
		mustStartJob(t, s, "document-vector")
		synctest.Wait()
		time.Sleep(2 * vectorPassBudget)
		synctest.Wait()
		for _, status := range s.JobStatus() {
			assert.Equal(1, calls[status.Name])
			assert.Contains(status.LastError, "runtime budget exceeded before committing progress")
			assert.False(status.Pending)
			assert.False(status.Running)
		}
	})
}

func TestInterruptedPassWithoutProgress(t *testing.T) {
	for _, interruption := range []string{"cooperative", "cancelled", "manual"} {
		t.Run(interruption, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				tracker := &yieldingWorkTracker{}
				var gate WorkTracker = newSerialWorkTracker()
				if interruption == "manual" {
					gate = tracker
				}
				s := New(nil).WithWorkTracker(gate)
				defer func() { <-s.Stop().Done() }()
				workerErr := errors.New("synthetic publication failure")
				complete := make(chan struct{})
				calls := 0
				require.NoError(s.AddJob(Job{Name: "test", Schedule: "0 0 1 1 *", Preemptible: true, MaxRuntime: vectorPassBudget, Run: func(ctx context.Context) error {
					calls++
					if calls > 1 {
						<-complete
						return nil
					}
					if interruption == "cooperative" {
						for !jobctx.PreemptionRequested(ctx) {
							time.Sleep(yieldPollInterval)
						}
					} else {
						<-ctx.Done()
					}
					return errors.Join(workerErr, ctx.Err())
				}}))
				mustStartJob(t, s, "test")
				synctest.Wait()
				if interruption == "manual" {
					tracker.yield.Store(true)
				} else {
					require.NoError(s.AddJob(Job{Name: "carddav", Schedule: "0 0 1 1 *", Run: func(context.Context) error { return nil }}))
					mustStartJob(t, s, "carddav")
					synctest.Wait()
				}
				time.Sleep(preemptAfter + 3*yieldPollInterval)
				synctest.Wait()
				var status JobStatus
				for _, candidate := range s.JobStatus() {
					if candidate.Name == "test" {
						status = candidate
					}
				}
				assert.Equal(2, calls)
				assert.Equal(workerErr.Error(), status.LastError)
				assert.False(status.Pending)
				close(complete)
				synctest.Wait()
			})
		})
	}
}
