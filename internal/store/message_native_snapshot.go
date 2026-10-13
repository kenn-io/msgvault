package store

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
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
			if err := s.persistNativeSnapshotReactionTx(ctx, tx, messageID, reaction); err != nil {
				return fmt.Errorf("persist native reaction: %w", err)
			}
		}
	}
	for _, reaction := range data.AddReactionRefs {
		if reaction.ParticipantID == 0 {
			continue
		}
		if err := s.persistNativeSnapshotReactionTx(ctx, tx, messageID, reaction); err != nil {
			return fmt.Errorf("persist additive native reaction: %w", err)
		}
	}
	if data.RequireReadySlackAttachments {
		if data.SlackAttachmentSnapshot == nil {
			return ErrReadyAttachmentSnapshot
		}
		for _, ref := range *data.SlackAttachmentSnapshot {
			ready := ref.State == attachmentpolicy.StateStored && ref.StoragePath != "" && ref.ContentHash != ""
			skipped := ref.State == attachmentpolicy.StateSkipped && ref.SkipReason != ""
			if !ready && !skipped {
				return fmt.Errorf("slack attachment %s: %w", ref.SourceAttachmentID, ErrReadyAttachmentSnapshot)
			}
		}
	}
	if data.SlackAttachmentSnapshot != nil {
		if err := s.replaceMessageAttachmentsWhereTxContext(context.Background(), tx, messageID, `source_attachment_id LIKE ?`, *data.SlackAttachmentSnapshot, "slack:%"); err != nil {
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
		if err := s.replaceMessageAttachmentsWhereTxContext(context.Background(), tx, messageID,
			linkAttachmentDeletePredicate, *data.LinkAttachmentSnapshot); err != nil {
			return fmt.Errorf("persist native link attachments: %w", err)
		}
	}
	return nil
}

// Snapshot aggregates retain native import behavior without publishing actions
// whose independent occurrence time was never observed.
func (s *Store) persistNativeSnapshotReactionTx(ctx context.Context, tx *loggedTx, messageID int64, reaction ReactionRef) error {
	_, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO reactions (message_id, participant_id, reaction_type, reaction_value, created_at) VALUES (?, ?, ?, ?, ?)`), messageID, reaction.ParticipantID, reaction.Type, reaction.Value, reaction.CreatedAt)
	return err
}
