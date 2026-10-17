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
	run, err := s.GetLatestSyncContext(ctx, sourceID, 0)
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
