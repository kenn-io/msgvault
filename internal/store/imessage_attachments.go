package store

import "context"

// MessageIMessageAttachmentsContext returns captured and unavailable Messages occurrences.
func (s *Store) MessageIMessageAttachmentsContext(ctx context.Context, messageID int64) (map[string]AttachmentRef, error) {
	return s.messageProviderAttachmentsContext(ctx, messageID, "imessage:")
}

// ReplaceMessageIMessageAttachmentsContext reconciles only Messages-owned rows.
func (s *Store) ReplaceMessageIMessageAttachmentsContext(ctx context.Context, messageID int64, refs []AttachmentRef) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.replaceMessageAttachmentsWhereTxContext(ctx, tx, messageID, `source_attachment_id LIKE 'imessage:%'`, false, refs); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE messages SET
			attachment_count = (SELECT COUNT(*) FROM attachments WHERE message_id = ?),
			has_attachments = has_attachments OR EXISTS (SELECT 1 FROM attachments WHERE message_id = ?)
			WHERE id = ?`, messageID, messageID, messageID)
		return err
	})
}
