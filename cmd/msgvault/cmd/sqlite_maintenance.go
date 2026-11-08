package cmd

import (
	"context"
	"log/slog"

	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

// The daily SQLite maintenance job refreshes planner statistics and truncates
// the WAL off-peak, instead of after every sync, where a loaded archive keeps
// both from finishing.
const (
	sqliteMaintenanceJob  = "sqlite-maintenance"
	sqliteMaintenanceCron = "29 4 * * *"
)

func registerSQLiteMaintenanceJob(sched *scheduler.Scheduler, s *store.Store) error {
	if s.IsPostgreSQL() {
		return nil
	}
	return sched.AddJob(scheduler.Job{
		Name:     sqliteMaintenanceJob,
		Schedule: sqliteMaintenanceCron,
		Run: func(ctx context.Context) error {
			_, err := s.RunDailyMaintenance(ctx)
			return err
		},
	})
}

// startSQLiteMaintenanceJob runs the maintenance job once at daemon startup.
// PostgreSQL archives never register the job, so there is nothing to start.
func startSQLiteMaintenanceJob(sched *scheduler.Scheduler, s *store.Store, logger *slog.Logger) {
	if s.IsPostgreSQL() {
		return
	}
	if _, err := sched.StartJob(sqliteMaintenanceJob, false); err != nil {
		logger.Error("failed to start SQLite maintenance", "error", err)
	}
}
