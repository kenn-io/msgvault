package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// backfillMeetingProjectionsContext bounds the scan and commits each locked
// message independently. A cancelled upgrade retains completed messages and
// runOnceMigration leaves its ledger unfinished so the next open resumes.
func (s *Store) backfillMeetingProjectionsContext(ctx context.Context) error {
	return s.backfillMessageProjectionsContext(ctx, "meeting_transcript", "meeting_details", meetingProjectionVersion, s.refreshMeetingProjectionWith)
}

// backfillMessageProjectionsContext pages through stale rows and rechecks each
// message under its evidence lock. Completed rows survive a cancelled upgrade.
// table is an internal projection table name, supplied only by the wrappers.
func (s *Store) backfillMessageProjectionsContext(ctx context.Context, messageType, table string, version int, refresh func(context.Context, *loggedTx, int64) error) error {
	// The first batch has no lower bound: zero, negative IDs, and MinInt64
	// are legal. Later batches advance past the last selected ID. A restarted
	// invocation starts without a cursor and excludes already current versions.
	var afterID int64
	var haveCursor bool
	for {
		query := `
			SELECT m.id FROM messages m
			WHERE m.message_type = ?
			  AND NOT EXISTS (
				SELECT 1 FROM ` + table + ` d
				WHERE d.message_id = m.id AND d.projection_version >= ?
			  )`
		args := []any{messageType, version}
		if haveCursor {
			query += ` AND m.id > ?`
			args = append(args, afterID)
		}
		query += ` ORDER BY m.id LIMIT 100`
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("scan message projection backfill: %w", err)
		}
		ids := make([]int64, 0, 100)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan message projection ID: %w", err)
			}
			ids = append(ids, id)
		}
		iterationErr := rows.Err()
		closeErr := rows.Close()
		if iterationErr != nil {
			return fmt.Errorf("iterate message projection backfill: %w", iterationErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close message projection backfill rows: %w", closeErr)
		}
		if len(ids) == 0 {
			return ctx.Err()
		}
		for _, id := range ids {
			err := s.withTxContext(ctx, func(tx *loggedTx) error {
				if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
					return fmt.Errorf("lock message projection %d: %w", id, err)
				}
				q := boundQuerier{ctx: ctx, q: tx}
				var currentType sql.NullString
				var currentVersion sql.NullInt64
				err := q.QueryRow(`
					SELECT m.message_type,
						(SELECT projection_version FROM `+table+` WHERE message_id = m.id)
					FROM messages m WHERE m.id = ?`, id).Scan(&currentType, &currentVersion)
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}
				if err != nil {
					return fmt.Errorf("recheck message projection %d: %w", id, err)
				}
				if currentType.String != messageType || (currentVersion.Valid && currentVersion.Int64 >= int64(version)) {
					return nil
				}
				return refresh(ctx, tx, id)
			})
			if err != nil {
				return fmt.Errorf("backfill message projection %d: %w", id, err)
			}
		}
		afterID = ids[len(ids)-1]
		haveCursor = true
	}
}
