package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"
	activitypkg "go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const (
	activityBuildSubcommand      = "build"
	activityProjectionJob        = "activity-projection"
	activityProjectionMaxBatches = 10
	// The scheduler asks a preemptible job to stop after it has held the gate
	// for a minute while other work waits. Projection stops at its next batch
	// boundary; the longer hard limit leaves room for that batch to commit.
	activityProjectionMaxRuntime = 2 * time.Minute
)

func newActivityCommand() *cobra.Command {
	activityCmd := &cobra.Command{
		Use:   "activity",
		Short: "Build the derived activity spine",
	}
	var backstop bool
	activityBuildCmd := &cobra.Command{
		Use:   activityBuildSubcommand,
		Short: "Project archived messages into dated activity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			return runActivityBuildLocal(cmd, backstop)
		},
	}
	activityBuildCmd.Flags().BoolVar(
		&backstop,
		"backstop",
		false,
		"Force a complete archive rescan",
	)
	activityCmd.AddCommand(activityBuildCmd)
	return activityCmd
}

func runActivityBuildLocal(cmd *cobra.Command, backstop bool) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	logger := state.logger
	st, cleanup, err := openWritableStoreAndInitForInvocation(cmd.Context(), state)
	if err != nil {
		return err
	}
	defer cleanup()
	projector, err := activitypkg.NewProjector(st, activitypkg.Options{
		Timezone:              cfg.Activity.Timezone,
		MaxDirectCounterparts: cfg.Activity.MaxDirectCounterparts,
		BatchSize:             cfg.Activity.BatchSize,
		Log:                   logger,
	})
	if err != nil {
		return err
	}
	var result activitypkg.RunResult
	if backstop {
		result, err = projector.RunBackstop(cmd.Context())
	} else {
		result, err = projector.RunOnce(cmd.Context())
	}
	if err != nil {
		return fmt.Errorf("build activity spine: %w", err)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(),
		"Projected %d event(s) in %d batch(es); touched %d and recomputed %d person(s); watermark %d.\n",
		result.EventsProjected,
		result.Batches,
		result.PersonsTouched,
		result.PersonsRecomputed,
		result.Watermark,
	)
	if err != nil {
		return fmt.Errorf("write activity summary: %w", err)
	}
	return nil
}

func registerActivityProjectionJob(
	sched *scheduler.Scheduler,
	st *store.Store,
	activityConfig config.ActivityConfig,
	log *slog.Logger,
) error {
	if activityConfig.Schedule == "" {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	projector, err := activitypkg.NewProjector(st, activitypkg.Options{
		Timezone:              activityConfig.Timezone,
		MaxDirectCounterparts: activityConfig.MaxDirectCounterparts,
		BatchSize:             activityConfig.BatchSize,
		MaxBatches:            activityProjectionMaxBatches,
		Log:                   log,
	})
	if err != nil {
		return err
	}
	return sched.AddJob(scheduler.Job{
		Name:        activityProjectionJob,
		Preemptible: true,
		MaxRuntime:  activityProjectionMaxRuntime,
		Schedule:    activityConfig.Schedule,
		Run:         activityProjectionRun(projector.RunOnce, st.ContactRevisionsContext, log),
	})
}

// A job's callbacks are serialized, so the baseline belongs to its current
// continuation chain, including passes resumed after scheduler interruptions.
func activityProjectionRun(
	runOnce func(context.Context) (activitypkg.RunResult, error),
	revisions func(context.Context) (store.ContactRevisions, error),
	log *slog.Logger,
) func(context.Context) error {
	var baseline *store.ContactRevisions
	return func(ctx context.Context) (err error) {
		budgetExpired := func() bool { return errors.Is(context.Cause(ctx), jobctx.ErrRunBudgetExceeded) }
		yielded := func() bool { return budgetExpired() || jobctx.YieldedToWaiter(ctx) || jobctx.PreemptionRequested(ctx) }
		defer func() {
			// Match runJob's priority: deferral and a budget without progress
			// end the chain even if another scheduler signal is also present.
			if err == scheduler.ErrDeferUntilNextTrigger || //nolint:errorlint // Only bare deferral ends a clean chain.
				(budgetExpired() && !jobctx.HasProgress(ctx)) ||
				(!errors.Is(err, scheduler.ErrReschedule) && !yielded()) {
				baseline = nil
			}
		}()
		current, err := revisions(ctx)
		if err != nil {
			return fmt.Errorf("read activity projection revisions: %w", err)
		}
		if baseline == nil {
			baseline = &current
		}
		moved := current != *baseline
		// Accept this pass even after a revision change: its trigger may already
		// include an explicit request coalesced behind the preceding pass.
		result, runErr := runOnce(ctx)
		clean := jobctx.ErrorAfterYield(ctx, runErr, activitypkg.ErrWorkRemaining) == nil
		continuing := errors.Is(runErr, activitypkg.ErrWorkRemaining) || yielded()
		if moved && clean && continuing && (!budgetExpired() || jobctx.HasProgress(ctx)) {
			log.Info("activity projection pass complete; continuation deferred after identity change",
				"processed", result.Processed, "batches", result.Batches)
			return scheduler.ErrDeferUntilNextTrigger
		}
		if clean && errors.Is(runErr, activitypkg.ErrWorkRemaining) {
			log.Info("activity projection pass complete; continuing behind queued work", "processed", result.Processed, "batches", result.Batches)
			return scheduler.ErrReschedule
		}
		if runErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded)) {
			return runErr
		}
		if runErr != nil {
			log.Error("activity projection failed",
				"error", runErr,
				"processed", result.Processed,
				"batches", result.Batches)
			return runErr
		}
		log.Info("activity projection complete",
			"events_projected", result.EventsProjected,
			"persons_touched", result.PersonsTouched,
			"persons_recomputed", result.PersonsRecomputed,
			"batches", result.Batches,
			"watermark", result.Watermark)
		return nil
	}
}

func init() {
	rootCmd.AddCommand(newActivityCommand())
}
