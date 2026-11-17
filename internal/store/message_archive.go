package store

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/msgvault/internal/emailtags"
)

// MessageArchiveChatContext resolves the exact source conversation, never a
// merged conversation or an identifier guessed from message contents.
func (s *Store) MessageArchiveChatContext(ctx context.Context, id int64) (int64, string, error) {
	var sourceID int64
	var chatID string
	err := s.db.QueryRowContext(ctx, `SELECT m.source_id,c.source_conversation_id
 FROM messages m JOIN conversations c ON c.id=m.conversation_id AND c.source_id=m.source_id
 WHERE m.id=? AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL`, id).Scan(&sourceID, &chatID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && chatID == "") {
		return 0, "", emailtags.Failure("message_not_found", "Message not found", nil, nil)
	}
	return sourceID, chatID, err
}
