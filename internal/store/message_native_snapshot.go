package store

import (
	"context"
	"fmt"
)

// persistNativeMessageSnapshot runs within message persistence's identity,
// sync-generation and Events fences, before the occurrence is appended.
func (s *Store) persistNativeMessageSnapshot(ctx context.Context, tx *loggedTx, messageID int64, data *MessagePersistData) error {
	if data.ReactionSnapshot != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM reactions WHERE message_id = ?`, messageID); err != nil {
			return fmt.Errorf("replace native reactions: %w", err)
		}
		for _, reaction := range *data.ReactionSnapshot {
			if reaction.ParticipantID == 0 {
				continue
			}
			// Embedded aggregates have no independently observed action time.
			if err := s.insertReactionTx(ctx, tx, messageID, reaction, false); err != nil {
				return fmt.Errorf("persist native reaction: %w", err)
			}
		}
	}
	if data.SlackAttachmentSnapshot != nil {
		if err := s.replaceMessageAttachmentsWhereTx(tx, messageID, `source_attachment_id LIKE ?`, false, *data.SlackAttachmentSnapshot, "slack:%"); err != nil {
			return fmt.Errorf("persist Slack attachments: %w", err)
		}
		if err := recomputeMessageAttachmentStatsWith(boundQuerier{ctx: ctx, q: tx}, messageID); err != nil {
			return fmt.Errorf("recompute Slack attachment stats: %w", err)
		}
	}
	if data.Edited != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET is_edited = ? WHERE id = ?`, *data.Edited, messageID); err != nil {
			return fmt.Errorf("persist native edit state: %w", err)
		}
	}
	if data.DeletedFromSource != nil {
		value := "NULL"
		if *data.DeletedFromSource {
			value = "COALESCE(deleted_from_source_at, " + s.dialect.Now() + ")"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET deleted_from_source_at = `+value+` WHERE id = ?`, messageID); err != nil {
			return fmt.Errorf("persist native deletion state: %w", err)
		}
	}
	if data.LinkAttachmentSnapshot != nil {
		if err := s.replaceMessageAttachmentsWhereTx(tx, messageID,
			linkAttachmentDeletePredicate, false, *data.LinkAttachmentSnapshot); err != nil {
			return fmt.Errorf("persist native link attachments: %w", err)
		}
	}
	return nil
}
