package cmd

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/importantdate"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
)

const importantDateJobName = "important-dates"

func addImportantDateJob(s *scheduler.Scheduler, cfg importantdate.Config, run func(context.Context) error) error {
	if !cfg.Enabled {
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	return s.AddJob(scheduler.Job{Name: importantDateJobName, Schedule: cfg.Schedule, Run: run})
}

func newImportantDateScheduledRun(cfg *config.Config, st *store.Store) func(context.Context) error {
	return func(ctx context.Context) error {
		settings := cfg.People.ImportantDates
		if !settings.Enabled {
			return nil
		}
		if !cfg.Integrations.Kata.Enabled {
			return fmt.Errorf("important-date alerts require the Kata integration")
		}
		client, err := taskclient.ConnectKata(ctx, taskclient.IntegrationConfig{Enabled: true, Endpoint: cfg.Integrations.Kata.Endpoint, APIKey: cfg.Integrations.Kata.APIKey, DefaultProject: settings.Project})
		if err != nil {
			return fmt.Errorf("connect important-date destination: %w", err)
		}
		projectUID, err := client.ProjectUID(ctx, settings.Project)
		if err != nil {
			return fmt.Errorf("resolve important-date project: %w", err)
		}
		settings.ProjectUID = projectUID
		settings.ConsentScope = importantdate.DestinationScope(cfg.Integrations.Kata.Endpoint, settings.Project, projectUID)
		_, more, err := importantdate.RunOnce(ctx, st, client, settings, time.Now())
		if err != nil {
			return err
		}
		if more {
			return scheduler.ErrReschedule
		}
		return nil
	}
}
