package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// LatestSyncOutcomeContext reports the latest source outcome, including incomplete success.
func (s *Store) LatestSyncOutcomeContext(ctx context.Context, sourceID int64) (string, error) {
	run, err := s.latestSourceSyncContext(ctx, sourceID)
	if errors.Is(err, ErrSyncRunNotFound) {
		return "never", nil
	}
	if err != nil {
		return "", err
	}
	if run.Status == SyncStatusCompleted && run.ErrorsCount > 0 {
		return "partial", nil
	}
	return run.Status, nil
}

// GetLastSuccessfulSourceSyncContext returns the latest completed sync that
// refreshed source records, excluding attachment-only provider runs.
func (s *Store) GetLastSuccessfulSourceSyncContext(ctx context.Context, sourceID int64) (*SyncRun, error) {
	return s.getLastSuccessfulSyncContext(ctx, sourceID, "", false, "beeper_media", "slack_media", "inline_media")
}

func (s *Store) latestSourceSyncContext(ctx context.Context, sourceID int64) (*SyncRun, error) {
	row := s.db.QueryRowContext(ctx, `
 SELECT id, source_id, started_at, completed_at, status,
 messages_processed, messages_added, messages_updated, errors_count,
 error_message, cursor_before, cursor_after, request_fingerprint
 FROM sync_runs
 WHERE source_id = ? AND sync_type NOT IN ('beeper_media', 'slack_media', 'inline_media')
 ORDER BY started_at DESC, id DESC LIMIT 1`, sourceID)
	run, err := scanSyncRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("latest source sync for source %d: %w", sourceID, ErrSyncRunNotFound)
	}
	return run, err
}
