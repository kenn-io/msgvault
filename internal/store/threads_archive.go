package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ThreadsArchiveMessageMetadataContext looks up one source-qualified media ID
// and its actual archive conversation, without loading any message body.
func (s *Store) ThreadsArchiveMessageMetadataContext(ctx context.Context, sourceID int64, mediaID string) (MessageMetadataRecord, bool, error) {
	var record MessageMetadataRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT m.id, m.metadata, c.source_conversation_id
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id AND c.source_id = m.source_id
		WHERE m.source_id = ? AND m.source_message_id = ?`, sourceID, mediaID).
		Scan(&record.ID, &record.Metadata, &record.SourceConversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return MessageMetadataRecord{}, false, nil
	}
	if err != nil {
		return MessageMetadataRecord{}, false, fmt.Errorf("get Threads archive message metadata: %w", err)
	}
	return record, true, nil
}
