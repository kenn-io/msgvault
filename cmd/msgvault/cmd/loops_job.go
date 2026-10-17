package cmd

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const openLoopsJob = "open-loops"

type loopRuntime struct {
	service *loops.Service
	runner  *loops.Runner
	deriver *loops.Deriver
}

// Disabled loops expose neither a scheduled job nor an API capability. An
// empty schedule keeps the daemon-owned manual runner available.
func newLoopRuntime(
	ctx context.Context, sched *scheduler.Scheduler, gate api.LabeledOperationGate,
	st *store.Store, cfg *config.Config,
) (*loopRuntime, error) {
	if !cfg.Loops.Enabled {
		return nil, nil
	}
	deriver, err := loops.NewDeriver(st, &cfg.Loops)
	if err != nil {
		return nil, err
	}
	service, err := loops.NewService(st, cfg, deriver.Freshness)
	if err != nil {
		return nil, err
	}
	runner, err := newLoopRunner(ctx, sched, gate, cfg.Loops, func(ctx context.Context) error {
		_, runErr := deriver.RunOnce(ctx)
		return runErr
	}, deriver.Rebuild)
	if err != nil {
		return nil, err
	}
	return &loopRuntime{service: service, runner: runner, deriver: deriver}, nil
}

func newLoopRunner(
	ctx context.Context, sched *scheduler.Scheduler, gate api.LabeledOperationGate,
	cfg config.LoopsConfig, run, rebuild func(context.Context) error,
) (*loops.Runner, error) {
	opts := loops.RunnerOptions{Context: ctx, Run: run, Rebuild: rebuild}
	if cfg.Schedule == "" {
		if gate == nil {
			return nil, fmt.Errorf("manual open loops require an operation gate")
		}
		opts.BeginWork = gate.BeginLabeledWorkContext
	} else {
		if sched == nil {
			return nil, fmt.Errorf("scheduled open loops require a scheduler")
		}
		opts.StartScheduled = func() (scheduler.JobDisposition, error) {
			return sched.StartJob(openLoopsJob)
		}
	}
	runner, err := loops.NewRunner(opts)
	if err != nil {
		return nil, err
	}
	if cfg.Schedule != "" {
		if err := sched.AddJob(scheduler.Job{
			Name: openLoopsJob, Schedule: cfg.Schedule,
			Preemptible: true, MaxRuntime: 2 * time.Minute,
			Run: runner.RunScheduled,
		}); err != nil {
			return nil, err
		}
	}
	return runner, nil
}
