package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"
)

// Sync run outcomes. A run that finished without proving it read a live
// source is "unmeasured": its zero new messages say nothing about the source.
const (
	SyncOutcomeCompleted  = "completed"
	SyncOutcomeUnmeasured = "unmeasured"
	SyncOutcomeFailed     = "failed"
)

// Reasons recorded with an unmeasured or failed outcome.
const (
	SyncReasonFDADenied            = "fda_denied"
	SyncReasonSourceMissing        = "source_missing"
	SyncReasonWriterNotRunning     = "writer_not_running"
	SyncReasonDaemonShuttingDown   = "daemon_shutting_down"
	syncMeasurementUnmeasuredError = "unmeasured: "
)

// SyncMeasurement records what a run could observe about its source.
type SyncMeasurement struct {
	Outcome       string     `json:"outcome"`
	Reason        string     `json:"reason,omitempty"`
	ReadStartedAt *time.Time `json:"read_started_at,omitempty"`
	// SourceMtime is the newest modification time among the source database
	// and its write-ahead log.
	SourceMtime *time.Time `json:"source_mtime,omitempty"`
	// WriterAlive reports whether the application that writes the source was
	// running when it was read. Nil when the source has no such writer.
	WriterAlive *bool `json:"writer_alive,omitempty"`
}

// ClassifyOutcome returns completed, or unmeasured/writer_not_running when the
// source's writer was known to be stopped during the read.
func (m SyncMeasurement) ClassifyOutcome() SyncMeasurement {
	if m.Outcome != "" {
		return m
	}
	m.Outcome = SyncOutcomeCompleted
	if m.WriterAlive != nil && !*m.WriterAlive {
		m.Outcome = SyncOutcomeUnmeasured
		m.Reason = SyncReasonWriterNotRunning
	}
	return m
}

// ErrSyncMeasurementNotFound is returned when a run recorded no measurement.
var ErrSyncMeasurementNotFound = errors.New("sync measurement not found")

// SetSyncMeasurement stores m on a sync run.
func (s *Store) SetSyncMeasurement(ctx context.Context, syncID int64, m SyncMeasurement) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode sync measurement: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, s.Rebind(
		`UPDATE sync_runs SET measurement = ? WHERE id = ?`), string(payload), syncID); err != nil {
		return fmt.Errorf("set sync measurement %d: %w", syncID, err)
	}
	return nil
}

// SetLatestSyncMeasurement stores m on the source's newest run. Imports hold
// the per-source sync lock, so the newest run is the one that just finished.
func (s *Store) SetLatestSyncMeasurement(ctx context.Context, sourceID int64, m SyncMeasurement) error {
	run, err := s.GetLatestSyncContext(ctx, sourceID, 0)
	if err != nil {
		return err
	}
	return s.SetSyncMeasurement(ctx, run.ID, m)
}

// GetSyncMeasurement returns the measurement stored on a run, or
// ErrSyncMeasurementNotFound when the run recorded none.
func (s *Store) GetSyncMeasurement(ctx context.Context, syncID int64) (*SyncMeasurement, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, s.Rebind(
		`SELECT measurement FROM sync_runs WHERE id = ?`), syncID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (!raw.Valid || raw.String == "")) {
		return nil, ErrSyncMeasurementNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get sync measurement %d: %w", syncID, err)
	}
	var m SyncMeasurement
	if err := json.Unmarshal([]byte(raw.String), &m); err != nil {
		return nil, fmt.Errorf("decode sync measurement %d: %w", syncID, err)
	}
	return &m, nil
}

// UnmeasuredSyncError is the error message of a run that failed without
// reaching its source for the given unmeasured reason.
func UnmeasuredSyncError(reason, detail string) string {
	if detail == "" {
		return syncMeasurementUnmeasuredError + reason
	}
	return syncMeasurementUnmeasuredError + reason + ": " + detail
}

// RecordUnmeasuredSync writes a failed run that never reached its source, so
// status reports the gap instead of the previous run's result.
func (s *Store) RecordUnmeasuredSync(
	ctx context.Context, sourceID int64, syncType, reason, detail string, m SyncMeasurement,
) error {
	syncID, err := s.StartSyncContext(ctx, sourceID, syncType)
	if err != nil {
		return fmt.Errorf("start sync: %w", err)
	}
	m.Outcome, m.Reason = SyncOutcomeUnmeasured, reason
	if err := s.SetSyncMeasurement(ctx, syncID, m); err != nil {
		_ = s.FailSyncContext(ctx, syncID, syncMeasurementUnmeasuredError+reason)
		return err
	}
	return s.FailSyncContext(ctx, syncID, UnmeasuredSyncError(reason, detail))
}
