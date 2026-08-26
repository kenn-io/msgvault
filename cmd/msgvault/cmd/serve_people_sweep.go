package cmd

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
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
	config peoplesweep.Config, st *store.Store,
) func(context.Context) error {
	return func(ctx context.Context) error {
		worker, err := newProductionPersonSweepWorker(config, st, os.LookupEnv)
		if err != nil {
			return err
		}
		_, err = worker.Run(ctx, peoplesweep.RunRequest{
			Kind: peoplesweep.RunScheduled, Mode: peoplesweep.RunIncremental,
			Limit: config.WorkBatchSize,
		})
		return err
	}
}

func newProductionPersonSweepWorker(
	config peoplesweep.Config,
	st *store.Store,
	lookup peoplesweep.CredentialLookup,
) (*peoplesweep.Worker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	transport, err := peoplesweep.NewStructuredTransport(
		config.Provider, http.DefaultClient,
		peoplesweep.NewCodexCommandStarter(), peoplesweep.NewReleasedCodexIsolationGate(),
	)
	if err != nil {
		return nil, err
	}
	runner, err := peoplesweep.NewRunner(config, st, transport, lookup)
	if err != nil {
		return nil, err
	}
	return &peoplesweep.Worker{
		Config: config, Store: st, Source: st,
		Context: peoplesweep.NewContextRetriever(st), Sink: st,
		Runner: runner, Catalog: st, Clock: time.Now, NewID: uuid.NewString,
		WorkerID: peopleSweepJobName + "-" + uuid.NewString(),
	}, nil
}
