package store

import (
	"database/sql"
	"fmt"
)

// SetConversationMetadata writes the conversations.metadata JSON/JSONB column.
// Passing an invalid sql.NullString clears the column.
func (s *Store) SetConversationMetadata(conversationID int64, metadata sql.NullString) error {
	_, err := s.db.Exec(fmt.Sprintf(`
		UPDATE conversations
		SET metadata = %s
		WHERE id = ?
	`, s.dialect.JSONBindExpr()), metadata, conversationID)
	if err != nil {
		return fmt.Errorf("set conversation metadata (id=%d): %w", conversationID, err)
	}
	return nil
}

// GetConversationMetadata reads the conversations.metadata JSON/JSONB column.
func (s *Store) GetConversationMetadata(conversationID int64) (sql.NullString, error) {
	var metadata sql.NullString
	if err := s.db.QueryRow(
		`SELECT metadata FROM conversations WHERE id = ?`, conversationID,
	).Scan(&metadata); err != nil {
		return sql.NullString{}, fmt.Errorf("get conversation metadata (id=%d): %w", conversationID, err)
	}
	return metadata, nil
}

// ConversationMetadataBatch returns provider metadata keyed by source
// conversation ID for the requested conversations that exist under sourceID.
// Existing conversations with SQL NULL metadata remain present with an invalid
// sql.NullString; missing or other-source conversations are omitted.
func (s *Store) ConversationMetadataBatch(
	sourceID int64, sourceConversationIDs []string,
) (map[string]sql.NullString, error) {
	if len(sourceConversationIDs) == 0 {
		return make(map[string]sql.NullString), nil
	}

	metadata := make(map[string]sql.NullString)
	err := queryInChunks(s.db, sourceConversationIDs, []any{sourceID},
		`SELECT source_conversation_id, metadata
		 FROM conversations
		 WHERE source_id = ? AND source_conversation_id IN (%s)`,
		func(rows *loggedRows) error {
			var sourceConversationID string
			var value sql.NullString
			if err := rows.Scan(&sourceConversationID, &value); err != nil {
				return err
			}
			metadata[sourceConversationID] = value
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("load conversation metadata batch: %w", err)
	}
	return metadata, nil
}
