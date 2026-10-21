package store

import (
	"context"
	"fmt"
)

// migrateLoopConversationDecisions moves message-keyed marks into stable
// conversation storage and snapshots the order tokens covered by each mark.
// The old table remains empty for subset readers that still need to recognize
// archives written by an earlier build of this feature.
func (s *Store) migrateLoopConversationDecisions(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, `WITH ranked AS (
			SELECT m.conversation_id, d.state, d.snoozed_until, d.through_at, d.through_message_id, d.through_order_id,
				ROW_NUMBER() OVER (PARTITION BY m.conversation_id ORDER BY
					CASE WHEN d.state = 'done' THEN 0 ELSE 1 END,
					CASE WHEN d.state = 'done' THEN d.through_at END DESC,
					CASE WHEN d.state = 'done' THEN d.through_order_id END DESC,
					CASE WHEN d.state = 'snoozed' THEN d.snoozed_until END DESC)
				AS decision_rank
			FROM loop_decisions d JOIN messages m ON m.id = d.anchor_message_id
			WHERE m.conversation_id IS NOT NULL
		)
		INSERT INTO loop_conversation_decisions
			(conversation_id, state, snoozed_until, through_at, through_message_id, through_order_id)
			SELECT conversation_id, state, snoozed_until, through_at, through_message_id, through_order_id
			FROM ranked WHERE decision_rank = 1
			ON CONFLICT (conversation_id) DO UPDATE SET
				state = excluded.state, snoozed_until = excluded.snoozed_until,
				through_at = excluded.through_at, through_message_id = excluded.through_message_id,
				through_order_id = excluded.through_order_id
			WHERE (excluded.state = ? AND (loop_conversation_decisions.state = ? OR
				(loop_conversation_decisions.state = ? AND
				(excluded.through_at, excluded.through_order_id) >
				(loop_conversation_decisions.through_at, loop_conversation_decisions.through_order_id))))
				OR (excluded.state = ? AND loop_conversation_decisions.state = ? AND
					excluded.snoozed_until > loop_conversation_decisions.snoozed_until)`,
			LoopStateDone, LoopStateSnoozed, LoopStateDone, LoopStateSnoozed, LoopStateSnoozed); err != nil {
			return fmt.Errorf("copy legacy loop decisions by conversation: %w", err)
		}

		coveredAt := s.loopTimeKey("d.through_at")
		messageAt := s.loopTimeKey("m.sent_at")
		if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO loop_decision_covered_orders
			(conversation_id, order_id)
			SELECT d.conversation_id, lmo.id
			FROM loop_conversation_decisions d
			JOIN messages m ON m.conversation_id = d.conversation_id
			JOIN loop_message_order lmo ON lmo.message_id = m.id
			WHERE `+messageAt+` < `+coveredAt+`
				OR (`+messageAt+` = `+coveredAt+` AND lmo.id <= d.through_order_id)
				OR m.id = d.through_message_id`)); err != nil {
			return fmt.Errorf("snapshot legacy loop decision coverage: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM loop_decisions`); err != nil {
			return fmt.Errorf("clear migrated message-keyed loop decisions: %w", err)
		}
		return nil
	})
}
