package store

import (
	"context"
	"errors"
)

// UpsertRemoteImageAttachment stores a distinct remote URL occurrence. Matching
// bytes never establish that a legacy MIME attachment is the same occurrence.
func (s *Store) UpsertRemoteImageAttachment(ctx context.Context, messageID int64, write AttachmentWrite) error {
	write = write.normalized()
	if err := write.validate(); err != nil {
		return err
	}
	if write.SourcePartKey == "" {
		return errors.New("remote image attachment requires a source-part key")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
			return err
		}
		// Serialize image summary increments with other writes to this message.
		var id int64
		if err := tx.QueryRow(`SELECT id FROM messages WHERE id = ?`+s.dialect.SelectForUpdate(), messageID).Scan(&id); err != nil {
			return err
		}
		var before, after int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`, messageID).Scan(&before); err != nil {
			return err
		}
		if err := s.upsertAttachmentRecordWithPolicy(tx, messageID, write, preserveLegacyAttachmentRows); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`, messageID).Scan(&after); err != nil {
			return err
		}
		// Only a newly stored occurrence increases the summary. Preserve any
		// expected MIME occurrences not yet represented by attachment rows.
		_, err := tx.Exec(`UPDATE messages SET has_attachments = TRUE,
			attachment_count = CASE WHEN COALESCE(attachment_count, 0) + ? < ?
				THEN ? ELSE COALESCE(attachment_count, 0) + ? END WHERE id = ?`,
			after-before, after, after, after-before, messageID)
		return err
	})
}

// MessageRemoteImages returns only this message's archived remote images.
// The namespace survives MIME repair, which replaces MIME-owned rows only.
func (s *Store) MessageRemoteImages(messageID int64) (map[string]AttachmentRef, error) {
	return s.MessageRemoteImagesContext(context.Background(), messageID)
}

func (s *Store) MessageRemoteImagesContext(ctx context.Context, messageID int64) (map[string]AttachmentRef, error) {
	return s.messageProviderAttachmentsContext(ctx, messageID, "remote-image:")
}

// RecomputeRemoteImageAttachmentStatsContext retains expected MIME occurrences
// while the acquisition ledger still requires them to be saved.
func (s *Store) RecomputeRemoteImageAttachmentStatsContext(ctx context.Context, messageID int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
			return err
		}
		q := boundQuerier{ctx: ctx, q: tx}
		// Serialize this decision with MIME and remote-image publication.
		var sourceID int64
		var sourceMessageID string
		if err := q.QueryRow(`SELECT source_id, source_message_id FROM messages WHERE id = ?`+s.dialect.SelectForUpdate(), messageID).
			Scan(&sourceID, &sourceMessageID); err != nil {
			return err
		}
		// Use the same latest-outcome and completed-incremental boundary as
		// ListUnresolvedSyncItemErrors. Image-only debt does not retain MIME counts.
		var pending bool
		if err := q.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM sync_run_items WHERE id IN (`+latestAcquisitionOutcomeIDsSQL(true)+`)
			AND status = 'error' AND error_kind IN ('`+SyncItemKindAttachmentError+`', '`+SyncItemKindAttachmentAndRemoteImageError+`')
		)`, sourceID, sourceID, "incremental", sourceMessageID).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return nil
		}
		return recomputeMessageAttachmentStatsWith(q, messageID)
	})
}

// RemoteImageBackfillMessageIDs pages email identities without scanning body
// storage. Bodies are subsequently read by message primary key.
func (s *Store) RemoteImageBackfillMessageIDs(ctx context.Context, after, sourceID int64, limit int) ([]int64, error) {
	// Match IsEmailMessageType: legacy NULL and empty types also denote email.
	query := "SELECT id FROM messages WHERE id > ? AND COALESCE(message_type, '') IN ('', 'email') AND deleted_at IS NULL"
	args := []any{after}
	if sourceID != 0 {
		query += " AND source_id = ?"
		args = append(args, sourceID)
	}
	query += " ORDER BY id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
