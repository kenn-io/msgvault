package store

// PendingAttachmentMessage identifies a message with at least one provider
// attachment marker that has not been downloaded yet.
type PendingAttachmentMessage struct {
	MessageID       int64
	SourceMessageID string
	ChatID          string // conversations.source_conversation_id
}

// BeeperPendingAttachmentMessage preserves the existing Beeper importer API.
type BeeperPendingAttachmentMessage = PendingAttachmentMessage

// DiscordPendingAttachmentMessage identifies a Discord message with pending media.
type DiscordPendingAttachmentMessage = PendingAttachmentMessage

func (s *Store) replaceMessageProviderAttachments(messageID int64, providerPrefix string, refs []AttachmentRef) error {
	return s.replaceMessageAttachmentsWhere(
		messageID, `source_attachment_id LIKE ?`, false, refs, providerPrefix+"%",
	)
}

func (s *Store) messageProviderAttachments(messageID int64, providerPrefix string) (map[string]AttachmentRef, error) {
	rows, err := s.db.Query(`
		SELECT COALESCE(filename, ''), COALESCE(mime_type, ''), storage_path, COALESCE(content_hash, ''), size, source_attachment_id,
		       COALESCE(media_type, ''), COALESCE(width, 0), COALESCE(height, 0), COALESCE(duration_ms, 0)
		FROM attachments
		WHERE message_id = ? AND source_attachment_id LIKE ?
	`, messageID, providerPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]AttachmentRef{}
	for rows.Next() {
		var ref AttachmentRef
		var size int64
		if err := rows.Scan(
			&ref.Filename, &ref.MimeType, &ref.StoragePath, &ref.ContentHash,
			&size, &ref.SourceAttachmentID, &ref.MediaType, &ref.Width,
			&ref.Height, &ref.DurationMS,
		); err != nil {
			return nil, err
		}
		ref.Size = int(size)
		out[ref.SourceAttachmentID] = ref
	}
	return out, rows.Err()
}

func (s *Store) listPendingAttachmentMessages(sourceID int64, providerPrefix string) ([]PendingAttachmentMessage, error) {
	rows, err := s.db.Query(`
		SELECT m.id, m.source_message_id, c.source_conversation_id
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ?
		  AND EXISTS (
		    SELECT 1 FROM attachments a
		    WHERE a.message_id = m.id
		      AND a.source_attachment_id LIKE ?
		      AND (a.content_hash IS NULL OR a.content_hash = '')
		  )
	`, sourceID, providerPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var items []PendingAttachmentMessage
	for rows.Next() {
		var item PendingAttachmentMessage
		if err := rows.Scan(&item.MessageID, &item.SourceMessageID, &item.ChatID); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ReplaceMessageDiscordAttachments replaces Discord-managed attachment rows.
// Rows without a content hash are pending-download markers whose storage path
// retains the observed CDN URL.
func (s *Store) ReplaceMessageDiscordAttachments(messageID int64, refs []AttachmentRef) error {
	return s.replaceMessageProviderAttachments(messageID, "discord:", refs)
}

// MessageDiscordAttachments returns Discord-managed rows keyed by source ID.
func (s *Store) MessageDiscordAttachments(messageID int64) (map[string]AttachmentRef, error) {
	return s.messageProviderAttachments(messageID, "discord:")
}

// ListDiscordPendingAttachmentMessages returns buffered messages containing at
// least one Discord attachment marker without downloaded content.
func (s *Store) ListDiscordPendingAttachmentMessages(sourceID int64) ([]DiscordPendingAttachmentMessage, error) {
	return s.listPendingAttachmentMessages(sourceID, "discord:")
}
