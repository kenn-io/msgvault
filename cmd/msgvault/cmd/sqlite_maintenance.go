package cmd

import (
	"context"

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
