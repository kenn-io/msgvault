package store

import (
	"context"
	"fmt"
)

// DiscordMessageSnapshot contains native state that must commit before a
// message arrival is published. Attachment downloads remain optional.
type DiscordMessageSnapshot struct {
	Attachments               []AttachmentRef
	Edited                    bool
	ReplySourceMessageID      string
	ReplySourceConversationID string
	ReplySourceGuildID        string
}

func (s *Store) persistDiscordMessageSnapshotTx(ctx context.Context, tx *loggedTx, messageID int64, data *MessagePersistData) error {
	snapshot := data.DiscordSnapshot
	if snapshot == nil {
		return nil
	}
	q := boundQuerier{ctx: ctx, q: tx}
	refs := append([]AttachmentRef(nil), snapshot.Attachments...)
	// Preserve completed downloads under the same writer fence as replacement.
	// A refreshed provider URL or metadata must not downgrade local CAS evidence.
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(source_attachment_id, ''),
		COALESCE(storage_path, ''), COALESCE(content_hash, ''), COALESCE(size, 0),
		COALESCE(attachment_state, ''), COALESCE(attachment_skip_reason, ''),
		COALESCE(attachment_role, ''), COALESCE(role_source, '')
		FROM attachments WHERE message_id=? AND source_attachment_id LIKE 'discord:%'`, messageID)
	if err != nil {
		return err
	}
	previous := make(map[string]AttachmentRef)
	for rows.Next() {
		var ref AttachmentRef
		if err := rows.Scan(&ref.SourceAttachmentID, &ref.StoragePath, &ref.ContentHash, &ref.Size, &ref.State, &ref.SkipReason, &ref.Role, &ref.RoleSource); err != nil {
			_ = rows.Close()
			return err
		}
		previous[ref.SourceAttachmentID] = ref
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for i := range refs {
		ref := &refs[i]
		prior, exists := previous[ref.SourceAttachmentID]
		if !exists {
			continue
		}
		ref.State, ref.SkipReason = prior.State, prior.SkipReason
		if prior.Size > ref.Size {
			ref.Size = prior.Size
		}
		if IsDiscordAttachmentDownloaded(prior) {
			ref.StoragePath, ref.ContentHash = prior.StoragePath, prior.ContentHash
			ref.Role, ref.RoleSource = prior.Role, prior.RoleSource
		}
	}
	refs = normalizeDiscordAttachmentRefs(refs)
	if err := s.replaceMessageAttachmentsWhereTx(tx, messageID, `source_attachment_id LIKE 'discord:%'`, false, refs); err != nil {
		return fmt.Errorf("persist Discord attachment snapshot: %w", err)
	}
	if err := recomputeMessageAttachmentStatsWith(q, messageID); err != nil {
		return err
	}
	if _, err := q.Exec(`UPDATE messages SET is_edited=CASE WHEN ? THEN TRUE ELSE is_edited END,
		deleted_from_source_at=NULL WHERE id=?`, snapshot.Edited, messageID); err != nil {
		return err
	}
	return setDiscordReplyWith(q, data.Message.SourceID, data.Message.SourceMessageID,
		snapshot.ReplySourceMessageID, snapshot.ReplySourceConversationID, snapshot.ReplySourceGuildID)
}

// SetDiscordReplyContext repairs an optional native reference without granting
// access to its parent. Supplied guild and channel identities must agree.
func (s *Store) SetDiscordReplyContext(ctx context.Context, sourceID int64, childID, parentID, parentConversationID, parentGuildID string) error {
	return s.withSyncSourceWriteContext(ctx, sourceID, func(q querier) error {
		return setDiscordReplyWith(q, sourceID, childID, parentID, parentConversationID, parentGuildID)
	})
}

func setDiscordReplyWith(q querier, sourceID int64, childID, parentID, parentConversationID, parentGuildID string) error {
	_, err := q.Exec(`UPDATE messages SET reply_to_message_id=(
		SELECT parent.id FROM messages parent
		WHERE parent.source_id=? AND parent.source_message_id=?
		AND (?='' OR EXISTS (SELECT 1 FROM conversations c WHERE c.id=parent.conversation_id
			AND c.source_id=parent.source_id AND c.source_conversation_id=?))
		AND (?='' OR EXISTS (SELECT 1 FROM sources s WHERE s.id=parent.source_id AND s.identifier=?))
	) WHERE source_id=? AND source_message_id=?`,
		sourceID, parentID, parentConversationID, parentConversationID,
		parentGuildID, parentGuildID, sourceID, childID)
	return err
}
