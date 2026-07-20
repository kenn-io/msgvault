package store

import (
	"encoding/hex"
	"strings"
)

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

// DiscordAttachmentMessage identifies a Discord message with provider-managed
// attachment rows, regardless of whether every row is already downloaded.
type DiscordAttachmentMessage = PendingAttachmentMessage

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
// Pending rows retain an observed CDN URL or deterministic provider sentinel.
// Hashless rows with a trusted local CAS path are duplicate-content aliases.
func (s *Store) ReplaceMessageDiscordAttachments(messageID int64, refs []AttachmentRef) error {
	refs = normalizeDiscordAttachmentRefs(refs)
	return s.replaceMessageProviderAttachments(messageID, "discord:", refs)
}

// normalizeDiscordAttachmentRefs preserves one row per stable Discord
// attachment ID when several attachments on a message share one CAS blob. The
// schema's (message_id, content_hash) uniqueness keeps the real hash on the
// first row; later aliases retain the same trusted local path with an empty
// hash. Empty source URLs become provider sentinels so generic replacement
// never drops their metadata.
func normalizeDiscordAttachmentRefs(refs []AttachmentRef) []AttachmentRef {
	normalized := append([]AttachmentRef(nil), refs...)
	seen := make(map[string]struct{}, len(normalized))
	for i := range normalized {
		if normalized[i].StoragePath == "" {
			attachmentID := strings.TrimPrefix(normalized[i].SourceAttachmentID, "discord:")
			normalized[i].StoragePath = "discord:pending:" + attachmentID
		}
		contentHash := strings.ToLower(normalized[i].ContentHash)
		if contentHash == "" {
			pathHash, ok := discordCASPathHash(normalized[i].StoragePath)
			if !ok {
				continue
			}
			contentHash = pathHash
			normalized[i].ContentHash = pathHash
		}
		if _, ok := seen[contentHash]; ok {
			normalized[i].ContentHash = ""
			continue
		}
		seen[contentHash] = struct{}{}
	}
	return normalized
}

// IsDiscordAttachmentDownloaded reports whether a Discord row references a
// trusted local SHA-256 CAS path. A duplicate-content alias may omit its hash;
// URLs, provider sentinels, malformed paths, and hash/path mismatches are not
// considered downloaded.
func IsDiscordAttachmentDownloaded(ref AttachmentRef) bool {
	pathHash, ok := discordCASPathHash(ref.StoragePath)
	if !ok {
		return false
	}
	return ref.ContentHash == "" || ref.ContentHash == pathHash
}

func discordCASPathHash(storagePath string) (string, bool) {
	if len(storagePath) != 67 || storagePath[2] != '/' {
		return "", false
	}
	contentHash := storagePath[3:]
	if contentHash != strings.ToLower(contentHash) || storagePath[:2] != contentHash[:2] {
		return "", false
	}
	if _, err := hex.DecodeString(contentHash); err != nil {
		return "", false
	}
	return contentHash, true
}

// MessageDiscordAttachments returns Discord-managed rows keyed by source ID.
func (s *Store) MessageDiscordAttachments(messageID int64) (map[string]AttachmentRef, error) {
	refs, err := s.messageProviderAttachments(messageID, "discord:")
	if err != nil {
		return nil, err
	}
	for sourceAttachmentID, ref := range refs {
		if ref.ContentHash == "" {
			if pathHash, ok := discordCASPathHash(ref.StoragePath); ok {
				ref.ContentHash = pathHash
				refs[sourceAttachmentID] = ref
			}
		}
	}
	return refs, nil
}

// ListDiscordPendingAttachmentMessages returns messages containing at least
// one Discord attachment that does not resolve to a trusted local CAS path.
func (s *Store) ListDiscordPendingAttachmentMessages(sourceID int64) ([]DiscordPendingAttachmentMessage, error) {
	rows, err := s.db.Query(`
		SELECT m.id, m.source_message_id, c.source_conversation_id,
		       a.storage_path, COALESCE(a.content_hash, '')
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN attachments a ON a.message_id = m.id
		WHERE m.source_id = ?
		  AND a.source_attachment_id LIKE ?
		ORDER BY m.id, a.id
	`, sourceID, "discord:%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var pending []DiscordPendingAttachmentMessage
	var current DiscordPendingAttachmentMessage
	var haveCurrent, currentPending bool
	flushCurrent := func() {
		if haveCurrent && currentPending {
			pending = append(pending, current)
		}
	}
	for rows.Next() {
		var item DiscordPendingAttachmentMessage
		var ref AttachmentRef
		if err := rows.Scan(
			&item.MessageID, &item.SourceMessageID, &item.ChatID,
			&ref.StoragePath, &ref.ContentHash,
		); err != nil {
			return nil, err
		}
		if !haveCurrent || item.MessageID != current.MessageID {
			flushCurrent()
			current = item
			haveCurrent = true
			currentPending = false
		}
		if !IsDiscordAttachmentDownloaded(ref) {
			currentPending = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	flushCurrent()
	return pending, nil
}

// ListDiscordAttachmentMessages returns every source-scoped message with at
// least one Discord-managed attachment. The one-query selection is used by a
// full media refresh; callers that only want incomplete rows use
// ListDiscordPendingAttachmentMessages.
func (s *Store) ListDiscordAttachmentMessages(sourceID int64) ([]DiscordAttachmentMessage, error) {
	rows, err := s.db.Query(`
		SELECT m.id, m.source_message_id, c.source_conversation_id
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ?
		  AND EXISTS (
		    SELECT 1
		    FROM attachments a
		    WHERE a.message_id = m.id
		      AND a.source_attachment_id LIKE ?
		  )
		ORDER BY m.id
	`, sourceID, "discord:%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var messages []DiscordAttachmentMessage
	for rows.Next() {
		var message DiscordAttachmentMessage
		if err := rows.Scan(&message.MessageID, &message.SourceMessageID, &message.ChatID); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}
