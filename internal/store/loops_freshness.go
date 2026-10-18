package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AuthenticatedChatSourceTypes returns the platform-authenticated sender sources.
func AuthenticatedChatSourceTypes() []string {
	return append([]string(nil), personSweepAuthenticatedSenderSourceTypes()...)
}

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

func (s *Store) latestSourceSyncContext(ctx context.Context, sourceID int64) (*SyncRun, error) {
	row := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT id, source_id, started_at, completed_at, status,
		       messages_processed, messages_added, messages_updated, errors_count,
		       error_message, cursor_before, cursor_after, request_fingerprint
		FROM sync_runs
		WHERE source_id = ? AND sync_type NOT IN (?, ?)
		ORDER BY started_at DESC, id DESC
		LIMIT 1
	`), sourceID, "beeper_media", "slack_media")
	run, err := scanSyncRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("latest source sync for source %d: %w", sourceID, ErrSyncRunNotFound)
	}
	return run, err
}

// GetLastSuccessfulSourceSyncContext returns the latest completed sync that
// refreshed source records, excluding attachment-only provider runs.
func (s *Store) GetLastSuccessfulSourceSyncContext(ctx context.Context, sourceID int64) (*SyncRun, error) {
	row := s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT id, source_id, started_at, completed_at, status,
		       messages_processed, messages_added, messages_updated, errors_count,
		       error_message, cursor_before, cursor_after, request_fingerprint
		FROM sync_runs
		WHERE source_id = ? AND status = 'completed' AND sync_type NOT IN (?, ?)
		ORDER BY completed_at DESC, id DESC
		LIMIT 1
	`), sourceID, "beeper_media", "slack_media")
	run, err := scanSyncRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("last successful source sync for source %d: %w", sourceID, ErrSyncRunNotFound)
	}
	return run, err
}

// ActivityProjectionComputedAtContext returns the newest projection computation for the requested people.
func (s *Store) ActivityProjectionComputedAtContext(ctx context.Context, personIDs []int64) (*time.Time, error) {
	if len(personIDs) == 0 {
		return nil, nil //nolint:nilnil // An empty requested set has no projection timestamp.
	}
	var newest *time.Time
	for start := 0; start < len(personIDs); start += 512 {
		ids := personIDs[start:min(start+512, len(personIDs))]
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		// Read actual timestamp columns: SQLite MAX(datetime) loses driver time typing.
		rows, err := s.db.QueryContext(ctx, s.Rebind("SELECT computed_at FROM person_contact_state WHERE person_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")"), args...)
		if err != nil {
			return nil, fmt.Errorf("load activity projection computation: %w", err)
		}
		for rows.Next() {
			var at sql.NullTime
			if err := rows.Scan(&at); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if at.Valid && (newest == nil || at.Time.After(*newest)) {
				v := at.Time
				newest = &v
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return newest, nil
}
