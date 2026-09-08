package cmd

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/api"
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

// newPersonBriefManualRun is the daemon's manual brief runner: one forced,
// single-person sweep attempt through the same production worker the schedule
// uses. It still requires enrollment, consent, and budget; it bypasses only the
// minimum interval and the new-activity check.
func newPersonBriefManualRun(
	cfg *config.Config, st *store.Store,
) func(context.Context, int64) (api.PersonBriefRun, error) {
	return func(ctx context.Context, personID int64) (api.PersonBriefRun, error) {
		worker, err := newProductionPersonSweepWorker(cfg, st)
		if err != nil {
			return api.PersonBriefRun{}, err
		}
		result, err := worker.Run(ctx, peoplesweep.RunRequest{
			Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
			PersonID: personID, Limit: 1, Brief: peoplesweep.BriefModeForce,
		})
		if err != nil {
			return api.PersonBriefRun{}, err
		}
		return personBriefRunResult(personID, result), nil
	}
}

// personBriefRunResult projects the run onto the one person the route asked
// about. A run that claimed no attempt for that person reports the run ID only.
func personBriefRunResult(
	personID int64, result peoplesweep.RunResult,
) api.PersonBriefRun {
	run := api.PersonBriefRun{RunID: result.RunID}
	for _, person := range result.People {
		if person.PersonID != personID {
			continue
		}
		run.AttemptID = person.AttemptID
		run.BriefVersion = person.BriefVersion
		run.BriefFailureClass = string(person.BriefFailureClass)
		break
	}
	return run
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
		Runner: runner, Catalog: st, Brief: st, Archive: st,
		Clock: time.Now, NewID: uuid.NewString,
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
