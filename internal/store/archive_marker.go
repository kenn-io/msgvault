package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SetArchiveMarker stores value under key in archive_metadata, replacing any
// previous value. Markers record durable operator-visible conditions, such as
// a source that must be re-anchored before it syncs again.
func (s *Store) SetArchiveMarker(ctx context.Context, key, value string) error {
	if _, err := s.db.ExecContext(ctx, s.Rebind(`
		INSERT INTO archive_metadata (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`), key, value); err != nil {
		return fmt.Errorf("set archive marker %s: %w", key, err)
	}
	return nil
}

// GetArchiveMarker returns the value stored under key and whether it exists.
func (s *Store) GetArchiveMarker(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key = ?`), key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read archive marker %s: %w", key, err)
	}
	return value, true, nil
}

// DeleteArchiveMarker removes key. Removing a missing marker is a no-op.
func (s *Store) DeleteArchiveMarker(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, s.Rebind(`DELETE FROM archive_metadata WHERE key = ?`), key); err != nil {
		return fmt.Errorf("delete archive marker %s: %w", key, err)
	}
	return nil
}

// BeeperReanchorMarkerKey returns the archive marker key for a Beeper source
// whose installation must be manually re-verified before scheduled syncs
// resume.
func BeeperReanchorMarkerKey(sourceID int64) string {
	return fmt.Sprintf("beeper.reanchor_required:%d", sourceID)
}
