package store

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/personscope"
)

// ListMediaSearchOccurrences reads the complete live population for a destination.
func (s *Store) ListMediaSearchOccurrences(ctx context.Context, destination string, scope *personscope.Scope) ([]MessageMediaOccurrence, error) {
	return s.listMessageOccurrenceRows(ctx, destination, 0, scope)
}

// MediaSearchLocalGaps counts audio that has no current mapping to search.
func (s *Store) MediaSearchLocalGaps(ctx context.Context, destination string, scope *personscope.Scope) (unavailable int, err error) {
	where := ""
	args := []any{destination}
	if scope != nil {
		predicate, values := personscope.MessagePredicate(*scope, "m", "c")
		where = " AND (" + predicate + ")"
		args = append(args, values...)
	}
	err = s.db.QueryRowContext(ctx, s.Rebind(`
		SELECT COUNT(*)
		FROM attachments a
		JOIN messages m ON m.id = a.message_id
		JOIN conversations c ON c.id = m.conversation_id
		JOIN sources src ON src.id = m.source_id
		WHERE `+LiveMessagesWhere("m", true)+` AND `+messageRecordingAudio("")+`
		  AND NOT EXISTS (SELECT 1 FROM beeper_media_occurrences o
		    WHERE o.destination_key = ? AND o.retention_state <> 'revoked'
		      AND o.source_type = src.source_type AND o.source_identifier = src.identifier
		      AND o.source_message_id = m.source_message_id
		      AND o.source_conversation_id = COALESCE(c.source_conversation_id, '')
		      AND o.source_attachment_id = COALESCE(a.source_attachment_id, '')
		      AND o.source_part_key = COALESCE(NULLIF(a.source_part_key, ''), NULLIF(a.source_attachment_id, ''), src.source_type || ':unknown')
		      AND o.source_sha256 = a.content_hash AND `+beeperMediaEligible+`)`+where), args...).Scan(&unavailable)
	if err != nil {
		err = fmt.Errorf("read media search gaps: %w", err)
	}
	return
}
