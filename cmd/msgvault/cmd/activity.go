package cmd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"
	activitypkg "go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const (
	activityBuildSubcommand = "build"
	activityProjectionJob   = "activity-projection"
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
	st, cleanup, err := openWritableStoreAndInit()
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
		Log:                   log,
	})
	if err != nil {
		return err
	}
	return sched.AddJob(scheduler.Job{
		Name:     activityProjectionJob,
		Schedule: activityConfig.Schedule,
		Run: func(ctx context.Context) error {
			result, runErr := projector.RunOnce(ctx)
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
		},
	})
}

func init() {
	rootCmd.AddCommand(newActivityCommand())
}
