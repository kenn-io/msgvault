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
	syncMeasurementUnmeasuredError = "unmeasured: "

	// SyncNoArchiveWritesErrorPrefix identifies attempts recorded before an importer
	// starts. Cache interruption counters exclude only this explicit no-write case.
	SyncNoArchiveWritesErrorPrefix = "unmeasured (no archive writes): "
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
// source's writer was known to be stopped and the run changed nothing. A run
// that added or updated messages read them from the source either way.
func (m SyncMeasurement) ClassifyOutcome(changed bool) SyncMeasurement {
	if m.Outcome != "" {
		return m
	}
	m.Outcome = SyncOutcomeCompleted
	if !changed && m.WriterAlive != nil && !*m.WriterAlive {
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

// GetLastMeasuredSyncContext returns the newest completed run that did not
// record an unmeasured outcome, so a run that could not see new data does not
// count as fresh. Measurements are stored as compact JSON written by
// SetSyncMeasurement, which makes the text match exact on both dialects.
func (s *Store) GetLastMeasuredSyncContext(ctx context.Context, sourceID int64) (*SyncRun, error) {
	row := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT id, source_id, started_at, completed_at, status,
		       messages_processed, messages_added, messages_updated, errors_count,
		       error_message, cursor_before, cursor_after, request_fingerprint
		FROM sync_runs
		WHERE source_id = ? AND status = 'completed'
		  AND (measurement IS NULL OR measurement NOT LIKE ?)
		ORDER BY completed_at DESC, id DESC
		LIMIT 1
	`), sourceID, `%"outcome":"`+SyncOutcomeUnmeasured+`"%`)
	run, err := scanSyncRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("last measured sync for source %d: %w", sourceID, ErrSyncRunNotFound)
	}
	return run, err
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

// UnmeasuredSyncError is the error message of a run that lost its source after
// the importer started. Unlike SyncNoArchiveWritesErrorPrefix, it does not mark
// the run as write-free: the importer may have committed messages first.
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
		_ = s.FailSyncContext(ctx, syncID, SyncNoArchiveWritesErrorPrefix+reason)
		return err
	}
	return s.FailSyncContext(ctx, syncID, SyncNoArchiveWritesErrorPrefix+reason+": "+detail)
}
