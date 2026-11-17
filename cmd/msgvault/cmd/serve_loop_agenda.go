package cmd

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
)

func registerLoopAgendaJob(sched *scheduler.Scheduler, cfg config.TaskIntegrationConfig, agenda *api.LoopAgenda) error {
	if !cfg.Enabled || cfg.AutoAgenda != nil && !*cfg.AutoAgenda {
		return nil
	}
	return sched.AddJob(scheduler.Job{Name: "loops-agenda", Schedule: "0 * * * *", Run: func(ctx context.Context) error { return agenda.Run(ctx, time.Now()) }})
}
