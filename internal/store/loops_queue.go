package store

import (
	"context"
	"fmt"
	"time"
)

type LoopQueuedConversation struct {
	ConversationID, Revision int64
	Epoch, Reason            string
	FeedBound                *time.Time
}

func (s *Store) PendingLoopConversationsContext(ctx context.Context, limit int) ([]LoopQueuedConversation, error) {
	if limit < 1 {
		return nil, fmt.Errorf("%w: queue limit must be positive", ErrInvalidLoop)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT conversation_id,revision,epoch,reason,feed_bound FROM loop_conversation_queue WHERE revision>processed_revision ORDER BY queued_at,conversation_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []LoopQueuedConversation{}
	for rows.Next() {
		var row LoopQueuedConversation
		var bound nullableTimestamp
		if err := rows.Scan(&row.ConversationID, &row.Revision, &row.Epoch, &row.Reason, &bound); err != nil {
			return nil, err
		}
		row.FeedBound = optionalTimestamp(bound)
		out = append(out, row)
	}
	return out, rows.Err()
}
func (s *Store) enqueueLoopTx(ctx context.Context, tx *loggedTx, ids []int64, reason, epoch string, now time.Time) error {
	for _, id := range ids {
		// A deleted conversation needs no work. Pending bounds (including NULL)
		// and epochs survive coalescence; only a new rescan replaces its epoch.
		_, err := tx.ExecContext(ctx, `INSERT INTO loop_conversation_queue(conversation_id,revision,feed_bound,epoch,reason,queued_at)
SELECT id,1,(SELECT feed_frontier FROM loop_derivation_state WHERE singleton=1),?,?,? FROM conversations WHERE id=?
ON CONFLICT(conversation_id) DO UPDATE SET revision=loop_conversation_queue.revision+1,
feed_bound=CASE WHEN loop_conversation_queue.revision=loop_conversation_queue.processed_revision THEN excluded.feed_bound ELSE loop_conversation_queue.feed_bound END,
epoch=CASE WHEN excluded.reason='rescan' THEN excluded.epoch WHEN loop_conversation_queue.revision=loop_conversation_queue.processed_revision THEN '' ELSE loop_conversation_queue.epoch END,
reason=excluded.reason,queued_at=excluded.queued_at`, epoch, reason, s.dialect.TimestampParam(now), id)
		if err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) EnqueueLoopConversationsContext(ctx context.Context, ids []int64, reason string, now time.Time) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error { return s.enqueueLoopTx(ctx, tx, ids, reason, "", now) })
}

// CommitLoopFeedContext enqueues against the pre-advance frontier and consumes
// the page in that same transaction. Cursor shape never uses a sentinel ID.
func (s *Store) CommitLoopFeedContext(ctx context.Context, ids []int64, cursor ChangedMessagesCursor, frontier, now time.Time) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := lockLoopState(ctx, tx); err != nil {
			return err
		}
		if err := s.enqueueLoopTx(ctx, tx, ids, "changed", "", now); err != nil {
			return err
		}
		var cursorAt any
		id, after := cursor.AfterID()
		if after {
			cursorAt = s.dialect.TimestampParam(cursor.At())
		}
		_, err := tx.ExecContext(ctx, `UPDATE loop_derivation_state SET feed_cursor_at=?,feed_cursor_message_id=?,feed_frontier=?,updated_at=? WHERE singleton=1`, cursorAt, id, s.dialect.TimestampParam(frontier), s.dialect.TimestampParam(now))
		return err
	})
}
func (s *Store) loopRescanIDs(ctx context.Context, q contextRowsQuerier, state LoopDerivationState, cutoff time.Time, limit int) ([]int64, error) {
	query := `SELECT c.id FROM conversations c WHERE (? OR c.last_message_at>=? OR
		EXISTS(SELECT 1 FROM messages m JOIN sources src ON src.id=m.source_id
			WHERE m.conversation_id=c.id AND COALESCE(m.sent_at,m.received_at,m.internal_date)>=? AND NOT ` + draftEvidenceSQL("m", "src") + `) OR
		EXISTS(SELECT 1 FROM open_loops l WHERE l.conversation_id=c.id AND l.status<>'closed'))`
	cutoffAt := s.dialect.TimestampParam(cutoff)
	args := []any{cutoff.IsZero(), cutoffAt, cutoffAt}
	if state.RescanStartedAt != nil {
		query += " AND c.id>?"
		args = append(args, state.RescanLastConversationID)
	}
	query += " ORDER BY c.id LIMIT ?"
	args = append(args, limit)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (s *Store) EnqueueLoopRescanContext(ctx context.Context, epoch LoopEpoch, cutoff time.Time, limit int, now time.Time) (int, error) {
	if limit < 1 {
		return 0, fmt.Errorf("%w: rescan limit must be positive", ErrInvalidLoop)
	}
	count := 0
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := lockLoopState(ctx, tx); err != nil {
			return err
		}
		state, err := readLoopState(ctx, tx)
		if err != nil {
			return err
		}
		if state.RescanReason == "" || state.RescanEpoch != epoch.String() {
			return nil
		}
		ids, err := s.loopRescanIDs(ctx, tx, state, cutoff, limit)
		if err != nil {
			return err
		}
		if err := s.enqueueLoopTx(ctx, tx, ids, "rescan", epoch.String(), now); err != nil {
			return err
		}
		last := state.RescanLastConversationID
		if len(ids) > 0 {
			last = ids[len(ids)-1]
		}
		_, err = tx.ExecContext(ctx, `UPDATE loop_derivation_state SET rescan_last_conversation_id=?,rescan_started_at=COALESCE(rescan_started_at,?),updated_at=? WHERE singleton=1`, last, s.dialect.TimestampParam(now), s.dialect.TimestampParam(now))
		count = len(ids)
		return err
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
