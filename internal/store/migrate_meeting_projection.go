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
	// The first batch has no lower bound: zero, negative IDs, and MinInt64
	// are legal. Later batches advance past the last selected ID. A restarted
	// invocation starts without a cursor and excludes already current versions.
	var afterID int64
	var haveCursor bool
	for {
		query := `
			SELECT m.id FROM messages m
			WHERE m.message_type = 'meeting_transcript'
			  AND NOT EXISTS (
				SELECT 1 FROM meeting_details d
				WHERE d.message_id = m.id AND d.projection_version >= ?
			  )`
		args := []any{meetingProjectionVersion}
		if haveCursor {
			query += ` AND m.id > ?`
			args = append(args, afterID)
		}
		query += ` ORDER BY m.id LIMIT 100`
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("scan meeting projection backfill: %w", err)
		}
		ids := make([]int64, 0, 100)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan meeting projection ID: %w", err)
			}
			ids = append(ids, id)
		}
		iterationErr := rows.Err()
		closeErr := rows.Close()
		if iterationErr != nil {
			return fmt.Errorf("iterate meeting projection backfill: %w", iterationErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close meeting projection backfill rows: %w", closeErr)
		}
		if len(ids) == 0 {
			return ctx.Err()
		}
		for _, id := range ids {
			err := s.withTxContext(ctx, func(tx *loggedTx) error {
				if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
					return fmt.Errorf("lock meeting projection %d: %w", id, err)
				}
				q := boundQuerier{ctx: ctx, q: tx}
				var messageType sql.NullString
				var version sql.NullInt64
				err := q.QueryRow(`
					SELECT m.message_type,
						(SELECT projection_version FROM meeting_details WHERE message_id = m.id)
					FROM messages m WHERE m.id = ?`, id).Scan(&messageType, &version)
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}
				if err != nil {
					return fmt.Errorf("recheck meeting projection %d: %w", id, err)
				}
				if messageType.String != "meeting_transcript" || (version.Valid && version.Int64 >= meetingProjectionVersion) {
					return nil
				}
				return s.refreshMeetingProjectionWith(ctx, tx, id)
			})
			if err != nil {
				return fmt.Errorf("backfill meeting projection %d: %w", id, err)
			}
		}
		afterID = ids[len(ids)-1]
		haveCursor = true
	}
}
