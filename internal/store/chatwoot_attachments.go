package store

import (
	"context"
	"fmt"
)

// MessageChatwootAttachments returns media occurrences keyed by stable provider
// IDs, independent of signed URLs.
func (s *Store) MessageChatwootAttachments(messageID int64) (map[string]AttachmentRef, error) {
	return s.messageProviderAttachments(messageID, "chatwoot:")
}

func (s *Store) ReplaceMessageChatwootAttachments(messageID int64, refs []AttachmentRef) error {
	return s.replaceMessageProviderAttachments(messageID, "chatwoot:", refs)
}

// ChatwootConversationHead returns the highest archived Chatwoot message ID in
// one conversation, or zero when none exist.
func (s *Store) ChatwootConversationHead(ctx context.Context, sourceID int64, sourceConversationID string) (int64, error) {
	var head int64
	if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT COALESCE(MAX(CAST(m.source_message_id AS BIGINT)), 0)
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ? AND c.source_id = ? AND c.source_conversation_id = ?`),
		sourceID, sourceID, sourceConversationID).Scan(&head); err != nil {
		return 0, fmt.Errorf("load Chatwoot conversation head: %w", err)
	}
	return head, nil
}
