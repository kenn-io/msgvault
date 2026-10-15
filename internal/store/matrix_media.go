package store

import (
	"context"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// MediaConversation treats non-direct Matrix rooms as channels for media policy.
func MediaConversation(conversation attachmentpolicy.Conversation) attachmentpolicy.Conversation {
	if conversation.Type != "direct_chat" {
		conversation.Type = "channel"
	}
	return conversation
}

// ListMatrixLegacyMediaMessagesContext pages archived messages without Matrix attachment markers.
func (s *Store) ListMatrixLegacyMediaMessagesContext(ctx context.Context, sourceID, afterID int64, include, exclude []string, policy attachmentpolicy.Policy) ([]PendingAttachmentMessage, error) {
	var query strings.Builder
	query.WriteString(`SELECT m.id, m.source_message_id, c.source_conversation_id,
		c.conversation_type, COALESCE(c.participant_count, 0), COALESCE(CAST(c.metadata AS TEXT), '')
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ? AND m.id > ?
		AND EXISTS (SELECT 1 FROM message_raw r WHERE r.message_id = m.id AND r.raw_format = 'matrix_json')
		AND NOT EXISTS (SELECT 1 FROM attachments a WHERE a.message_id = m.id AND a.source_attachment_id LIKE 'matrix:%')`)
	args := []any{sourceID, afterID}
	appendMatrixRoomFilters(&query, &args, include, exclude)
	query.WriteString(" ORDER BY m.id LIMIT 100")
	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var items []PendingAttachmentMessage
	for rows.Next() {
		var item PendingAttachmentMessage
		var metadata string
		if err := rows.Scan(&item.MessageID, &item.SourceMessageID, &item.ChatID, &item.ConversationType, &item.ParticipantCount, &metadata); err != nil {
			return nil, err
		}
		item.ParticipantCount = policyParticipantCount("matrix", item.ParticipantCount, metadata, policy)
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListMatrixRetryableAttachmentMessagesContext includes media from source-redacted messages under current policy.
func (s *Store) ListMatrixRetryableAttachmentMessagesContext(ctx context.Context, sourceID, afterID int64, limit int, policy attachmentpolicy.Policy) ([]PendingAttachmentMessage, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var where string
	args := []any{sourceID, "matrix:%"}
	if policy.DisabledReason != "" || policy.Scope == attachmentpolicy.ScopeNone {
		where += " AND COALESCE(a.attachment_state, '') <> 'skipped'"
	} else {
		if policy.MaxBytes > 0 {
			where += " AND (COALESCE(a.attachment_state, '') <> 'skipped' OR COALESCE(a.size, 0) <= ?)"
			args = append(args, policy.MaxBytes)
		}
		if policy.Scope == attachmentpolicy.ScopeDirect {
			where += " AND (COALESCE(a.attachment_state, '') <> 'skipped' OR c.conversation_type = 'direct_chat')"
		}
		if policy.MaxParticipants > 0 {
			count := `CASE WHEN json_valid(c.metadata) THEN json_extract(c.metadata, '$.member_count') END`
			if s.IsPostgreSQL() {
				count = `CASE WHEN jsonb_typeof(c.metadata->'member_count') = 'number' THEN (c.metadata->>'member_count')::numeric END`
			}
			where += " AND (COALESCE(a.attachment_state, '') <> 'skipped' OR COALESCE((" + count + ") <= ?, TRUE))"
			args = append(args, policy.MaxParticipants)
		}
	}
	query := `
		SELECT m.id, m.source_message_id, c.source_conversation_id,
		       c.conversation_type, COALESCE(c.participant_count, 0),
		       COALESCE(a.attachment_state, ''), COALESCE(a.size, 0),
		       a.storage_path, COALESCE(CAST(c.metadata AS TEXT), ''), a.id
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN attachments a ON a.message_id = m.id
		WHERE m.source_id = ?
		  AND a.source_attachment_id LIKE ?
		  AND COALESCE(a.content_hash, '') = ''
		  AND COALESCE(a.attachment_state, '') IN ('', 'pending', 'failed', 'skipped')
		` + where
	query += " AND (m.id > ? OR (m.id = ? AND a.id > ?)) ORDER BY m.id, a.id LIMIT ?"
	var items []PendingAttachmentMessage
	var lastAdded int64
	afterAttachmentID := int64(1<<63 - 1)
	// Roster and CAS checks stay in Go; bounded pages advance past rejected rows.
	for {
		pageArgs := append([]any(nil), args...)
		pageArgs = append(pageArgs, afterID, afterID, afterAttachmentID, limit)
		rows, err := s.db.QueryContext(ctx, query, pageArgs...)
		if err != nil {
			return nil, err
		}
		var scanned int
		for rows.Next() {
			var item PendingAttachmentMessage
			var state attachmentpolicy.DownloadState
			var size int64
			var storagePath, conversationMetadata string
			if err := rows.Scan(&item.MessageID, &item.SourceMessageID, &item.ChatID,
				&item.ConversationType, &item.ParticipantCount, &state, &size,
				&storagePath, &conversationMetadata, &afterAttachmentID); err != nil {
				_ = rows.Close()
				return nil, err
			}
			afterID = item.MessageID
			scanned++
			item.ParticipantCount = policyParticipantCount("matrix:", item.ParticipantCount, conversationMetadata, policy)
			if item.MessageID == lastAdded || casAttachmentDownloaded(AttachmentRef{StoragePath: storagePath}) {
				continue
			}
			conversation := MediaConversation(attachmentpolicy.Conversation{Type: item.ConversationType, ParticipantCount: item.ParticipantCount})
			if state == attachmentpolicy.StateSkipped && !policy.Allows(conversation, size) {
				continue
			}
			items = append(items, item)
			lastAdded = item.MessageID
			if len(items) == limit {
				break
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		if scanned < limit || len(items) == limit {
			return items, nil
		}
	}
}

// MatrixOutstandingMedia counts durable pending and failed occurrences in the selected rooms.
func (s *Store) MatrixOutstandingMedia(ctx context.Context, sourceID int64, include, exclude []string) (pending, failed int64, err error) {
	var query strings.Builder
	query.WriteString(`SELECT COUNT(CASE WHEN a.attachment_state = 'pending' THEN 1 END),
		COUNT(CASE WHEN a.attachment_state = 'failed' THEN 1 END)
		FROM attachments a JOIN messages m ON m.id = a.message_id
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ?
		AND a.source_attachment_id LIKE 'matrix:%'`)
	args := []any{sourceID}
	appendMatrixRoomFilters(&query, &args, include, exclude)
	err = s.db.QueryRowContext(ctx, query.String(), args...).Scan(&pending, &failed)
	return
}
