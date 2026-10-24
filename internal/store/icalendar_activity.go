package store

import (
	"context"
	"fmt"
)

// Use typed source-local cancellation state. Cancelled single rows act as
// UID-wide master tombstones even when they retain event bounds.
const activityICSMasterCancelled = `EXISTS (
	SELECT 1
	FROM calendar_events exception
	JOIN message_raw exception_raw ON exception_raw.message_id = exception.message_id
	JOIN messages master ON master.source_id = m.source_id
	JOIN calendar_events series ON series.message_id = master.id
	JOIN message_raw master_raw ON master_raw.message_id = master.id
	WHERE exception.message_id = m.id AND exception_raw.raw_format = 'ics'
	  AND master.deleted_at IS NULL AND master.deleted_from_source_at IS NULL
	  AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = master.id)
	  AND exception.kind = 'exception'
	  AND series.provider_event_id = exception.series_provider_id
	  AND series.status = 'cancelled' AND master_raw.raw_format = 'ics'
	  AND series.kind IN ('series', 'single')
)`

func (s *Store) finishICSRevisionWith(ctx context.Context, tx *loggedTx, sourceID, id int64, item preparedICS, oldStatus string) error {
	q := boundQuerier{ctx: ctx, q: tx}
	var conversationID int64
	if err := q.QueryRow(`SELECT conversation_id FROM messages WHERE id=?`, id).Scan(&conversationID); err != nil {
		return err
	}
	if err := s.recomputeConversationStatsWith(q, "id = ?", conversationID); err != nil {
		return err
	}
	event := item.component.Event
	if event.Kind == calendarExceptionKind || oldStatus == event.Status {
		return nil
	}
	// Drained exceptions have no pending revision of their own when only the
	// master changes. Queue them so existing activity is retracted or restored.
	_, err := q.Exec(`INSERT INTO activity_projection_queue (message_id, revision, queued_at)
		SELECT m.id, 1, CURRENT_TIMESTAMP
		FROM messages m JOIN calendar_events e ON e.message_id=m.id
		WHERE m.source_id=? AND e.kind='exception' AND e.series_provider_id=?
		ON CONFLICT(message_id) DO UPDATE SET
		  revision=activity_projection_queue.revision+1, queued_at=CURRENT_TIMESTAMP`, sourceID, event.ProviderEventID)
	if err != nil {
		return fmt.Errorf("queue ICS exceptions after master status change: %w", err)
	}
	return nil
}
