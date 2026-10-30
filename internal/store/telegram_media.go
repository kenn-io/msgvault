package store

import (
	"context"
	"errors"
	"fmt"
)

// ListTelegramPendingMediaAfterContext pages Telegram messages with an API
// attachment that has no stored bytes. Policy-skipped files remain eligible
// for an explicit backfill, which reevaluates the current policy.
func (s *Store) ListTelegramPendingMediaAfterContext(ctx context.Context, sourceID, afterID int64, limit int) ([]PendingAttachmentMessage, error) {
	if sourceID <= 0 || afterID < 0 || limit <= 0 || limit > 500 {
		return nil, errors.New("invalid Telegram media page")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, c.id, m.source_message_id, c.source_conversation_id,
		       c.conversation_type, COALESCE(c.participant_count, 0)
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ? AND m.message_type = ? AND m.id > ?
		  AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = m.id)
		  AND EXISTS (
		    SELECT 1 FROM attachments a
		    WHERE a.message_id = m.id AND a.source_attachment_id LIKE ?
		      AND COALESCE(a.storage_path, '') = ''
		      AND NOT EXISTS (SELECT 1 FROM source_merge_preserved_attachments spa WHERE spa.attachment_id = a.id)
		  )
		ORDER BY m.id LIMIT ?`, sourceID, "telegram", afterID, "api:%", limit)
	if err != nil {
		return nil, fmt.Errorf("list Telegram pending media: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []PendingAttachmentMessage
	for rows.Next() {
		var item PendingAttachmentMessage
		if err := rows.Scan(&item.MessageID, &item.ConversationID, &item.SourceMessageID, &item.ChatID, &item.ConversationType, &item.ParticipantCount); err != nil {
			return nil, fmt.Errorf("scan Telegram pending media: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Telegram pending media: %w", err)
	}
	return result, nil
}
