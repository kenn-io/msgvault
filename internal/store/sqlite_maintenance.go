package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/cenkalti/backoff/v7"
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
	delays := s.checkpointRetryBackoff
	if delays == nil {
		delays = defaultCheckpointRetryBackoff
	}
	policy := backoff.NewExponentialBackOff()
	policy.Multiplier = 3
	if len(delays) != 0 {
		policy.InitialInterval = delays[0]
		policy.MaxInterval = delays[len(delays)-1]
	}
	var lastCheckpointErr error
	_, checkpointErr := backoff.Retry(ctx, func() (struct{}, error) {
		if err := ctx.Err(); err != nil {
			return struct{}{}, backoff.Permanent(errors.Join(lastCheckpointErr, err))
		}
		report.CheckpointAttempts++
		lastCheckpointErr = s.CheckpointWALContext(ctx)
		return struct{}{}, lastCheckpointErr
	}, backoff.WithBackOff(policy), backoff.WithMaxTries(uint(len(delays)+1)), backoff.WithMaxElapsedTime(0))
	if checkpointErr != nil {
		retryErr := backoff.AsRetryError(checkpointErr)
		checkpointErr = retryErr.LastErr
		if !errors.Is(retryErr.Cause, backoff.ErrPermanent) && !errors.Is(retryErr.Cause, backoff.ErrExhausted) {
			checkpointErr = errors.Join(checkpointErr, ctx.Err())
		}
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
