package cmd

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const peopleSweepJobName = "people-sweep"

func addPeopleSweepJob(
	s *scheduler.Scheduler,
	cfg peoplesweep.Config,
	run func(context.Context) error,
) error {
	if !cfg.Enabled {
		return nil
	}
	return s.AddJob(scheduler.Job{Name: peopleSweepJobName, Schedule: cfg.Schedule, Run: run})
}

func newPeopleSweepScheduledRun(
	cfg *config.Config, st *store.Store,
) func(context.Context) error {
	return func(ctx context.Context) error {
		worker, err := newProductionPersonSweepWorker(cfg, st)
		if err != nil {
			return err
		}
		_, err = worker.Run(ctx, peoplesweep.RunRequest{
			Kind: peoplesweep.RunScheduled, Mode: peoplesweep.RunIncremental,
			Limit: cfg.People.Sweep.WorkBatchSize,
		})
		return err
	}
}

func newProductionPersonSweepWorker(
	cfg *config.Config, st *store.Store,
) (*peoplesweep.Worker, error) {
	if cfg == nil {
		return nil, errors.New("people sweep production config is unavailable")
	}
	if err := cfg.People.Sweep.Validate(); err != nil {
		return nil, err
	}
	runner, err := newProductionStructuredRunner(cfg, st)
	if err != nil {
		return nil, err
	}
	sweepConfig := cfg.People.Sweep
	return &peoplesweep.Worker{
		Config: sweepConfig, Store: st, Source: st,
		Context: peoplesweep.NewContextRetriever(st), Sink: st,
		Runner: runner, Catalog: st, Clock: time.Now, NewID: uuid.NewString,
		WorkerID: peopleSweepJobName + "-" + uuid.NewString(),
	}, nil
}

func newProductionStructuredRunner(
	cfg *config.Config, st *store.Store,
) (*peoplesweep.Runner, error) {
	registry, err := peoplesweep.NewDriverRegistry(
		http.DefaultClient,
		peoplesweep.NewCodexCommandStarter(), peoplesweep.NewReleasedCodexIsolationGate(),
	)
	if err != nil {
		return nil, err
	}
	credentials := peoplesweep.NewCredentialResolver(
		peoplesweep.NewFileCredentialStore(cfg.TokensDir()), os.LookupEnv,
	)
	return peoplesweep.NewRunner(cfg.People.Sweep, st, registry, credentials)
}
