package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// Daily maintenance budgets. The daemon runs it off-peak under the operation
// gate, so it can afford to wait for the connection pool and the checkpoint.
const (
	dailyOptimizeTimeout = 90 * time.Second
)

var defaultCheckpointRetryBackoff = []time.Duration{5 * time.Second, 15 * time.Second}

// MaintenanceReport describes one RunDailyMaintenance pass.
type MaintenanceReport struct {
	OptimizeErr        error
	CheckpointAttempts int
	WALBytesBefore     int64
	WALBytesAfter      int64
}

// RunDailyMaintenance refreshes SQLite planner statistics and truncates the
// WAL, retrying the checkpoint while readers keep it busy. It returns an
// error when checkpoint attempts fail or ctx is cancelled; an optimize failure
// is reported in the result. A no-op for PostgreSQL and read-only stores.
func (s *Store) RunDailyMaintenance(ctx context.Context) (MaintenanceReport, error) {
	var report MaintenanceReport
	if s.IsPostgreSQL() || s.readOnly {
		return report, nil
	}
	report.OptimizeErr = s.optimizeSQLiteWithin(ctx, dailyOptimizeTimeout)
	logSQLiteOptimizeError("daily maintenance", report.OptimizeErr)

	report.WALBytesBefore = s.walBytes()
	backoff := s.checkpointRetryBackoff
	if backoff == nil {
		backoff = defaultCheckpointRetryBackoff
	}
	var checkpointErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			checkpointErr = errors.Join(checkpointErr, err)
			break
		}
		report.CheckpointAttempts++
		checkpointErr = s.CheckpointWALContext(ctx)
		if checkpointErr == nil || attempt >= len(backoff) {
			break
		}
		timer := time.NewTimer(backoff[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			checkpointErr = errors.Join(checkpointErr, ctx.Err())
		case <-timer.C:
			continue
		}
		break
	}
	report.WALBytesAfter = s.walBytes()
	if checkpointErr != nil {
		slog.Warn("SQLite WAL checkpoint failed after retries",
			"attempts", report.CheckpointAttempts,
			"wal_bytes", report.WALBytesAfter,
			"error", checkpointErr)
		return report, fmt.Errorf("checkpoint SQLite WAL: %w", checkpointErr)
	}
	slog.Info("SQLite daily maintenance complete",
		"optimized", report.OptimizeErr == nil,
		"checkpoint_attempts", report.CheckpointAttempts,
		"wal_bytes_before", report.WALBytesBefore,
		"wal_bytes_after", report.WALBytesAfter)
	return report, nil
}

// CheckpointWALPassive copies as much of the WAL into the database as
// possible without waiting for readers or writers. ctx bounds pool acquisition
// and the PRAGMA; callers treat remaining frames as expected contention.
func (s *Store) CheckpointWALPassive(ctx context.Context) error {
	return s.dialect.CheckpointWALPassive(ctx, s.db.DB)
}

func (s *Store) walBytes() int64 {
	if s.dbPath == "" {
		return 0
	}
	info, err := os.Stat(s.dbPath + "-wal")
	if err != nil {
		return 0
	}
	return info.Size()
}
