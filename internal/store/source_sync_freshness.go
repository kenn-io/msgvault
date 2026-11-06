package store

import (
	"context"
	"errors"
)

// LatestSyncOutcomeContext reports the latest source outcome, including incomplete success.
func (s *Store) LatestSyncOutcomeContext(ctx context.Context, sourceID int64) (string, error) {
	run, err := s.getLatestSyncContext(ctx, sourceID, 0, "beeper_media", "slack_media", "inline_media")
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
