package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// ErrStaleArchiveWatermark reports that an archived message update arrived
// after a newer provider observation or event had already been committed.
var ErrStaleArchiveWatermark = errors.New("stale archive watermark")

// ArchiveWatermark orders a provider snapshot or event against other writes
// for one archived message. Floor carries a watermark kept in legacy message
// metadata until that message is next updated.
type ArchiveWatermark struct {
	Key          string
	ObservedAt   int64
	Floor        int64
	RequireNewer bool
}

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

// AdvanceArchiveWatermarkContext stores the greater of the current value,
// floor, and observedAt. It serializes updates on the marker row.
func (s *Store) AdvanceArchiveWatermarkContext(ctx context.Context, key string, observedAt, floor int64) error {
	if key == "" || observedAt <= 0 || floor < 0 {
		return errors.New("invalid archive watermark")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		current, err := s.lockArchiveWatermarkWith(q, key, floor)
		if err != nil {
			return err
		}
		return advanceArchiveWatermarkWith(q, key, current, observedAt)
	})
}

// lockArchiveWatermarkWith ensures a numeric marker row exists, locks it on
// PostgreSQL, and raises it to floor when migrating a legacy message value.
func (s *Store) lockArchiveWatermarkWith(q querier, key string, floor int64) (int64, error) {
	if key == "" || floor < 0 {
		return 0, errors.New("invalid archive watermark")
	}
	if _, err := q.Exec(`
		INSERT INTO archive_metadata (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO NOTHING`, key, strconv.FormatInt(floor, 10)); err != nil {
		return 0, fmt.Errorf("create archive watermark %s: %w", key, err)
	}
	var value string
	if err := q.QueryRow(`SELECT value FROM archive_metadata WHERE key = ?`+s.dialect.SelectForUpdate(), key).Scan(&value); err != nil {
		return 0, fmt.Errorf("lock archive watermark %s: %w", key, err)
	}
	current, err := strconv.ParseInt(value, 10, 64)
	if err != nil || current < 0 {
		return 0, fmt.Errorf("invalid archive watermark %s", key)
	}
	if current < floor {
		if _, err := q.Exec(`UPDATE archive_metadata SET value = ? WHERE key = ?`, strconv.FormatInt(floor, 10), key); err != nil {
			return 0, fmt.Errorf("raise archive watermark %s: %w", key, err)
		}
		current = floor
	}
	return current, nil
}

func advanceArchiveWatermarkWith(q querier, key string, current, observedAt int64) error {
	if observedAt <= current {
		return nil
	}
	if _, err := q.Exec(`UPDATE archive_metadata SET value = ? WHERE key = ?`, strconv.FormatInt(observedAt, 10), key); err != nil {
		return fmt.Errorf("advance archive watermark %s: %w", key, err)
	}
	return nil
}

// BeeperReanchorMarkerKey returns the archive marker key for a Beeper source
// whose installation must be manually re-verified before scheduled syncs
// resume.
func BeeperReanchorMarkerKey(sourceID int64) string {
	return fmt.Sprintf("%s%d", beeperReanchorMarkerPrefix, sourceID)
}

const beeperReanchorMarkerPrefix = "beeper.reanchor_required:"
