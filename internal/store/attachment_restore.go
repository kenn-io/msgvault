package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RestoreMIMEAttachmentRecordsContext adds missing MIME occurrences without
// replacing existing attachment metadata or provider-owned references. Blobs
// must already be published. Conflicting evidence leaves every row untouched.
func (s *Store) RestoreMIMEAttachmentRecordsContext(ctx context.Context, expected MessageIdentityGuard, writes []AttachmentWrite) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.requireSyncMessageSourceTx(tx, expected.ID); err != nil {
			return err
		}
		var actual MessageIdentityGuard
		if err := tx.QueryRow(`SELECT id, source_id, source_message_id FROM messages WHERE id = ?`+s.dialect.SelectForUpdate(), expected.ID).
			Scan(&actual.ID, &actual.SourceID, &actual.SourceMessageID); err != nil {
			return err
		}
		if actual != expected {
			return errors.New("attachment repair identity guard mismatch")
		}
		var oldCount int
		var oldHas bool
		if err := tx.QueryRow(`SELECT has_attachments, attachment_count FROM messages WHERE id = ?`, expected.ID).Scan(&oldHas, &oldCount); err != nil {
			return err
		}
		changed := false
		for _, write := range writes {
			write = write.normalized()
			if err := write.validate(); err != nil {
				return err
			}
			if write.SourcePartKey == "" || write.ContentHash == "" || write.SourceAttachmentID != "" {
				return errors.New("attachment repair requires a MIME part key and content hash")
			}
			var hash string
			var provider sql.NullString
			var size int64
			err := tx.QueryRow(`SELECT COALESCE(content_hash, ''), source_attachment_id, COALESCE(size, 0)
				FROM attachments WHERE message_id = ? AND source_part_key = ?`, expected.ID, write.SourcePartKey).Scan(&hash, &provider, &size)
			if err == nil {
				if provider.Valid || hash != write.ContentHash || size != write.Size {
					return fmt.Errorf("conflicting attachment metadata for part %q", write.SourcePartKey)
				}
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			// Upgrade only the missing provenance on a matching legacy row.
			// Keep its ID, path, metadata and references intact.
			result, err := tx.Exec(`UPDATE attachments SET source_part_key = ?
				WHERE message_id = ? AND source_part_key IS NULL AND content_hash = ?
				AND size = ? AND source_attachment_id IS NULL
				AND NOT EXISTS (SELECT 1 FROM source_merge_preserved_attachments spa WHERE spa.attachment_id = attachments.id)`,
				write.SourcePartKey, expected.ID, write.ContentHash, write.Size)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				if err := s.upsertAttachmentRecordWithPolicy(tx, expected.ID, write, preserveLegacyAttachmentRows); err != nil {
					return err
				}
			}
			changed = true
		}
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`, expected.ID).Scan(&count); err != nil {
			return err
		}
		if oldHas != (count > 0) || count != oldCount {
			if err := recomputeMessageAttachmentStatsWith(tx, expected.ID); err != nil {
				return err
			}
			changed = true
		}
		if changed {
			return s.bumpDerivedDataRevision(tx)
		}
		return nil
	})
}
