package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ReassignSourceConversationIDContext moves a conversation from a legacy
// provider identity to its current one within a source, for importers whose
// provider learns a stronger identity for an existing chat.
//
// When no conversation has newSourceConversationID, the legacy row is
// renamed in place and keeps its messages, members and metadata. When both
// exist, the legacy conversation's messages move to the current one, its
// members are added there, and the emptied legacy row stays. It returns
// whether a legacy conversation was found.
func (s *Store) ReassignSourceConversationIDContext(
	ctx context.Context, sourceID int64, legacySourceConversationID, newSourceConversationID string,
) (bool, error) {
	if legacySourceConversationID == "" || legacySourceConversationID == newSourceConversationID {
		return false, nil
	}
	if err := s.requireSyncSource(sourceID); err != nil {
		return false, err
	}
	found := false
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var legacyID int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`,
			sourceID, legacySourceConversationID,
		).Scan(&legacyID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find legacy conversation: %w", err)
		}
		found = true

		var currentID int64
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`,
			sourceID, newSourceConversationID,
		).Scan(&currentID)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE conversations SET source_conversation_id = ? WHERE id = ?`,
				newSourceConversationID, legacyID,
			); err != nil {
				return fmt.Errorf("rename legacy conversation: %w", err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("find current conversation: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE messages SET conversation_id = ? WHERE source_id = ? AND conversation_id = ?`,
			currentID, sourceID, legacyID,
		); err != nil {
			return fmt.Errorf("move legacy conversation messages: %w", err)
		}
		if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(`
			INSERT OR IGNORE INTO conversation_participants (conversation_id, participant_id, role, joined_at, left_at)
			SELECT ?, participant_id, role, joined_at, left_at
			FROM conversation_participants WHERE conversation_id = ?`),
			currentID, legacyID,
		); err != nil {
			return fmt.Errorf("copy legacy conversation members: %w", err)
		}
		return nil
	})
	return found, err
}
