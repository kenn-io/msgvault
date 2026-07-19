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
