package store

import (
	"context"
	"fmt"
)

// migrateLoopMessageOrdering seeds a non-reused ordering token for every
// existing message. Before this migration, loop_decisions.through_order_id
// stored the message ID, so seeding tokens from those same IDs preserves the
// existing sent-time/ID boundary. The sequence then starts above both live
// messages and retired cutoff tokens.
func (s *Store) migrateLoopMessageOrdering(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(`
			INSERT OR IGNORE INTO loop_message_order (id, message_id)
			SELECT id, id FROM messages ORDER BY id
		`)); err != nil {
			return fmt.Errorf("backfill loop message ordering tokens: %w", err)
		}

		var ceiling int64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(value) FROM (
			SELECT COALESCE(MAX(id), 0) AS value FROM messages
			UNION ALL SELECT COALESCE(MAX(id), 0) FROM loop_message_order
			UNION ALL SELECT COALESCE(MAX(through_order_id), 0) FROM loop_decisions
			UNION ALL SELECT COALESCE(MAX(through_order_id), 0) FROM loop_conversation_decisions
			UNION ALL SELECT COALESCE(MAX(order_id), 0) FROM loop_decision_covered_orders
		)`).Scan(&ceiling); err != nil {
			return fmt.Errorf("read loop ordering token ceiling: %w", err)
		}

		if s.IsPostgreSQL() {
			if _, err := tx.ExecContext(ctx, `SELECT setval(
				pg_get_serial_sequence('loop_message_order', 'id'), GREATEST(1, ?), ?
			)`, ceiling, ceiling > 0); err != nil {
				return fmt.Errorf("advance PostgreSQL loop ordering token sequence: %w", err)
			}
			return nil
		}

		if _, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence
			SET seq = max(seq, ?) WHERE name = 'loop_message_order'`, ceiling); err != nil {
			return fmt.Errorf("advance SQLite loop ordering token sequence: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sqlite_sequence (name, seq)
			SELECT 'loop_message_order', ? WHERE NOT EXISTS (
				SELECT 1 FROM sqlite_sequence WHERE name = 'loop_message_order'
			)`, ceiling); err != nil {
			return fmt.Errorf("initialize SQLite loop ordering token sequence: %w", err)
		}
		return nil
	})
}
