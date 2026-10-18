package cmd

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLoopRuntimeDisabledHasNoCapabilities(t *testing.T) {
	cfg := config.NewDefaultConfig()
	cfg.Loops.Enabled = false
	runtime, err := newLoopRuntime(t.Context(), nil, nil, nil, cfg)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	assert.Nil(t, runtime.service)
	assert.Nil(t, runtime.runner)
	assert.Nil(t, runtime.deriver)
}

func TestLoopRuntimeManualDerivesAndReportsAppliedProgress(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Loops.Schedule = ""
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	gate := api.NewSerialOperationGate()
	runtime, err := newLoopRuntime(ctx, nil, gate, st, cfg)
	requirements.NoError(err)
	requirements.NotNil(runtime)
	assertions.True(runtime.service.Enabled())
	disposition, err := runtime.runner.Start(true)
	requirements.NoError(err)
	assertions.Equal(scheduler.JobStarted, disposition)
	requirements.EventuallyWithT(func(collect *assert.CollectT) {
		state, stateErr := runtime.deriver.State(t.Context())
		if assert.NoError(collect, stateErr) {
			assert.NotNil(collect, state.LastSuccessAt)
			assert.NotNil(collect, state.AppliedThrough)
		}
	}, 10*time.Second, 10*time.Millisecond)
	state, err := runtime.deriver.State(t.Context())
	requirements.NoError(err)
	projection, err := runtime.deriver.Freshness(t.Context())
	requirements.NoError(err)
	assertions.Equal(state.AppliedThrough, projection.ComputedAt)
	assertions.Equal(state.LastSuccessAt, projection.LastSuccessAt)
	// Drain the real gate before the fixture closes the database.
	done, ok := gate.BeginWorkContext(t.Context())
	requirements.True(ok)
	done()
}

func TestLoopRunnerSchedulingAndGateOwnership(t *testing.T) {
	for _, scheduled := range []bool{false, true} {
		name := "manual"
		if scheduled {
			name = "scheduled"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)
				daemonCtx, stopDaemon := context.WithCancel(t.Context())
				defer stopDaemon()
				gate := api.NewSerialOperationGate()
				sched := scheduler.New(nil).WithWorkTracker(gate)
				defer func() { <-sched.Stop().Done() }()
				cfg := config.DefaultLoopsConfig()
				if !scheduled {
					cfg.Schedule = ""
				}
				started := make(chan context.Context, 2)
				release := make(chan struct{})
				var rebuilds atomic.Int32
				runner, err := newLoopRunner(daemonCtx, sched, gate, cfg,
					func(ctx context.Context) error {
						started <- ctx
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-release:
							return nil
						}
					}, func(context.Context) error { rebuilds.Add(1); return nil })
				requirements.NoError(err)
				if scheduled {
					jobs := sched.JobStatus()
					requirements.Len(jobs, 1)
					assertions.Equal("open-loops", jobs[0].Name)
					assertions.Equal("*/15 * * * *", jobs[0].Schedule)
				} else {
					assertions.Empty(sched.JobStatus())
				}
				requestCtx, cancelRequest := context.WithCancel(t.Context())
				done, ok := gate.BeginRequestWorkContext(requestCtx, "POST /api/v1/loops/run")
				requirements.True(ok)
				disposition, err := runner.Start(false)
				requirements.NoError(err)
				assertions.Equal(scheduler.JobStarted, disposition)
				synctest.Wait()
				disposition, err = runner.Start(true)
				requirements.NoError(err)
				assertions.Equal(scheduler.JobCoalesced, disposition)
				cancelRequest()
				done()
				runCtx := <-started
				requirements.NoError(runCtx.Err(), "request cancellation must not cancel daemon work")
				deadline, ok := runCtx.Deadline()
				requirements.True(ok)
				assertions.Equal(2*time.Minute, time.Until(deadline))
				label, _, held := gate.Holder()
				assertions.True(held)
				assertions.Equal("open-loops", label)
				assertions.Equal(int32(1), rebuilds.Load())
				disposition, err = runner.Start(false)
				requirements.NoError(err)
				assertions.Equal(scheduler.JobPending, disposition)
				disposition, err = runner.Start(true)
				requirements.NoError(err)
				assertions.Equal(scheduler.JobCoalesced, disposition)
				release <- struct{}{}
				<-started
				assertions.Equal(int32(2), rebuilds.Load(), "mid-pass rebuild must survive to the follow-up")
				stopDaemon()
				if scheduled {
					<-sched.Stop().Done()
				}
				synctest.Wait()
			})
		})
	}
}

func TestLoopRunnerDaemonShutdownCancelsQueuedManualWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirements := require.New(t)
		ctx, cancel := context.WithCancel(t.Context())
		gate := api.NewSerialOperationGate()
		done, ok := gate.BeginWorkContext(t.Context())
		requirements.True(ok)
		defer done()
		cfg := config.DefaultLoopsConfig()
		cfg.Schedule = ""
		var calls atomic.Int32
		run := func(context.Context) error { calls.Add(1); return nil }
		runner, err := newLoopRunner(ctx, nil, gate, cfg, run, run)
		requirements.NoError(err)
		_, err = runner.Start(true)
		requirements.NoError(err)
		synctest.Wait()
		cancel()
		synctest.Wait()
		assert.Zero(t, calls.Load())
		_, err = runner.Start(false)
		requirements.ErrorIs(err, context.Canceled)
	})
}

func TestLoopScheduledRunnerYieldsToQueuedJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirements := require.New(t)
		gate := api.NewSerialOperationGate()
		sched := scheduler.New(nil).WithWorkTracker(gate)
		defer func() { <-sched.Stop().Done() }()
		started := make(chan struct{}, 2)
		var passes atomic.Int32
		runner, err := newLoopRunner(t.Context(), sched, gate, config.DefaultLoopsConfig(),
			func(ctx context.Context) error {
				started <- struct{}{}
				if passes.Add(1) == 1 {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}, func(context.Context) error { return nil })
		requirements.NoError(err)
		otherStarted := make(chan struct{})
		requirements.NoError(sched.AddJob(scheduler.Job{Name: "other", Schedule: "0 0 1 1 *", Run: func(context.Context) error {
			close(otherStarted)
			return nil
		}}))
		_, err = runner.Start(false)
		requirements.NoError(err)
		<-started
		_, err = sched.StartJob("other")
		requirements.NoError(err)
		synctest.Wait()
		time.Sleep(75 * time.Second)
		select {
		case <-otherStarted:
		default:
			requirements.FailNow("preemptible loop pass must release the gate before its two-minute hard limit")
		}
		<-started
	})
}
