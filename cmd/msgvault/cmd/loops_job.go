package cmd

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const openLoopsJob = "open-loops"
const openLoopsKataJob = "open-loops-kata"

type loopRuntime struct {
	service *loops.Service
	runner  *loops.Runner
	deriver *loops.Deriver
	bridge  *loops.Bridge
}

// Disabled loops expose neither a scheduled job nor an API capability. An
// empty schedule keeps the daemon-owned manual runner available.
func newLoopRuntime(
	ctx context.Context, sched *scheduler.Scheduler, gate api.LabeledOperationGate,
	st *store.Store, cfg *config.Config, kataOptions ...api.KataIssueOperations,
) (*loopRuntime, error) {
	if !cfg.Loops.Enabled {
		return &loopRuntime{}, nil
	}
	deriver, err := loops.NewDeriver(st, &cfg.Loops)
	if err != nil {
		return nil, err
	}
	var serviceOptions []loops.ServiceOption
	var bridge *loops.Bridge
	if cfg.Integrations.Kata.Enabled {
		var operations api.KataIssueOperations
		if len(kataOptions) > 0 {
			operations = kataOptions[0]
		} else {
			operations = api.NewKataIssueOperations(cfg, st, nil)
		}
		if operations != nil {
			serviceOptions = append(serviceOptions, loops.WithKata(operations))
			bridge, err = loops.NewBridge(st, operations, &cfg.Loops.Kata)
			if err != nil {
				return nil, err
			}
		}
	}
	service, err := loops.NewService(st, cfg, deriver.Freshness, serviceOptions...)
	if err != nil {
		return nil, err
	}
	var after []func(context.Context)
	if bridge != nil {
		after = append(after, func(ctx context.Context) {
			bridgeCtx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			if _, err := bridge.RunOnce(bridgeCtx); err != nil {
				slog.Error("open loops Kata bridge failed", "error", err)
			}
		})
		if cfg.Loops.Schedule != "" {
			if sched == nil {
				return nil, errors.New("scheduled open loops require a scheduler")
			}
			if err := sched.AddJob(scheduler.Job{Name: openLoopsKataJob, Schedule: cfg.Loops.Schedule, Preemptible: true, MaxRuntime: time.Minute, Run: bridge.RunScheduled}); err != nil {
				return nil, err
			}
		}
	}
	runner, err := newLoopRunner(ctx, sched, gate, cfg.Loops, func(ctx context.Context) error {
		_, runErr := deriver.RunOnce(ctx)
		return runErr
	}, deriver.Rebuild, after...)
	if err != nil {
		return nil, err
	}
	return &loopRuntime{service: service, runner: runner, deriver: deriver, bridge: bridge}, nil
}

func newLoopRunner(
	ctx context.Context, sched *scheduler.Scheduler, gate api.LabeledOperationGate,
	cfg config.LoopsConfig, run, rebuild func(context.Context) error, after ...func(context.Context),
) (*loops.Runner, error) {
	opts := loops.RunnerOptions{Context: ctx, Run: run, Rebuild: rebuild}
	if cfg.Schedule == "" {
		if gate == nil {
			return nil, errors.New("manual open loops require an operation gate")
		}
		opts.BeginWork = gate.BeginLabeledWorkContext
		if len(after) > 0 {
			opts.AfterRun = after[0]
		}
	} else {
		if sched == nil {
			return nil, errors.New("scheduled open loops require a scheduler")
		}
		opts.StartScheduled = func() (scheduler.JobDisposition, error) {
			disposition, err := sched.StartJob(openLoopsJob)
			if err == nil && len(after) > 0 {
				_, _ = sched.StartJob(openLoopsKataJob)
			}
			return disposition, err
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
