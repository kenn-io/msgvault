package store

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// An attachment row records archived bytes only when it carries their hash.
// content_hash names the blob; storage_path on a hashless row is the source
// locator or placeholder an importer keeps for media it has not archived
// (a provider URL, a pending or excluded marker, a source key). Backup,
// packing, and export select archived bytes by hash, so two states would
// claim bytes the archive cannot name:
//
//   - attachment_state 'stored' without a content hash;
//   - a thumbnail_path without a thumbnail hash.
//
// The SQLite guard triggers. Subset copy drops them while it bulk-copies
// rows from older archives, and the post-copy upgrade reinstalls them.
var attachmentBytesHashTriggers = []string{
	"trg_attachments_archived_bytes_hash_insert",
	"trg_attachments_archived_bytes_hash_update",
}

// archivedBytesHashPredicate is the rule both backends enforce, over the
// columns of row (empty for a table constraint, "NEW." in a trigger).
func archivedBytesHashPredicate(row string) string {
	return fmt.Sprintf(`(%[1]sattachment_state IS NULL OR %[1]sattachment_state <> 'stored' OR COALESCE(%[1]scontent_hash, '') <> '')
		AND (COALESCE(%[1]sthumbnail_path, '') = '' OR COALESCE(%[1]sthumbnail_hash, '') <> '')`, row)
}

// enforceAttachmentBytesHash repairs rows that break the rule and installs
// the database guard. Legacy Slack and Discord duplicate-byte aliases kept
// a hashless row at the content-addressed "<aa>/<hash>" path, which names the
// hash, so those rows get it back. Any other 'stored' row without a hash
// becomes a failed download, so retrying importers fetch it again
// instead of reporting it archived. A thumbnail path without a hash has no
// producer to repair it from, so its path is cleared.
func (s *Store) enforceAttachmentBytesHash(ctx context.Context, tx *loggedTx) error {
	if err := recoverAttachmentPathHashes(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE attachments
		SET attachment_state = ?, attachment_skip_reason = ?
		WHERE attachment_state = ? AND COALESCE(content_hash, '') = ''
	`, attachmentpolicy.StateFailed, attachmentpolicy.SkipFetchFailure, attachmentpolicy.StateStored); err != nil {
		return fmt.Errorf("mark hashless stored attachments failed: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE attachments
		SET thumbnail_path = NULL, thumbnail_hash = NULL
		WHERE COALESCE(thumbnail_path, '') <> '' AND COALESCE(thumbnail_hash, '') = ''
	`); err != nil {
		return fmt.Errorf("clear hashless attachment thumbnails: %w", err)
	}

	var ddl []string
	if s.IsPostgreSQL() {
		ddl = []string{
			`ALTER TABLE attachments DROP CONSTRAINT IF EXISTS attachments_archived_bytes_hash`,
			`ALTER TABLE attachments ADD CONSTRAINT attachments_archived_bytes_hash CHECK (` +
				archivedBytesHashPredicate("") + `)`,
		}
	} else {
		// SQLite cannot add a CHECK constraint without rebuilding the table.
		for _, name := range attachmentBytesHashTriggers {
			ddl = append(ddl, `DROP TRIGGER IF EXISTS `+name)
		}
		ddl = append(ddl,
			`CREATE TRIGGER trg_attachments_archived_bytes_hash_insert
			BEFORE INSERT ON attachments
			WHEN NOT (`+archivedBytesHashPredicate("NEW.")+`)
			BEGIN
				SELECT RAISE(ABORT, 'attachments: stored bytes and thumbnails require a content hash');
			END`,
			`CREATE TRIGGER trg_attachments_archived_bytes_hash_update
			BEFORE UPDATE OF attachment_state, content_hash, thumbnail_path, thumbnail_hash ON attachments
			WHEN NOT (`+archivedBytesHashPredicate("NEW.")+`)
			BEGIN
				SELECT RAISE(ABORT, 'attachments: stored bytes and thumbnails require a content hash');
			END`,
		)
	}
	for _, stmt := range ddl {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("install attachment bytes hash guard: %w", err)
		}
	}
	return nil
}

// recoverAttachmentPathHashes records the hash a hashless row's
// content-addressed storage path names. The per-message hash index admits one
// unkeyed row per hash, which is why aliases were left hashless. A recovered
// alias without a source-part key takes its source attachment ID, the same
// upgrade replaceMessageAttachmentsWhereTx applies. When that ID is missing or
// taken and an unkeyed row already holds the hash, it takes a key derived from
// its row. A replacing import removes a row with a stale key just as it
// removes an unkeyed one.
func recoverAttachmentPathHashes(ctx context.Context, tx *loggedTx) error {
	type candidate struct {
		id, messageID                 int64
		hash, partKey, sourceAttachID string
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, message_id, storage_path, COALESCE(source_part_key, ''), COALESCE(source_attachment_id, '')
		FROM attachments
		WHERE COALESCE(content_hash, '') = '' AND LENGTH(storage_path) = 67
		  AND COALESCE(attachment_state, '') IN ('', ?)
		ORDER BY id
	`, attachmentpolicy.StateStored)
	if err != nil {
		return fmt.Errorf("list hashless content-addressed attachments: %w", err)
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var storagePath string
		if err := rows.Scan(&c.id, &c.messageID, &storagePath, &c.partKey, &c.sourceAttachID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan hashless content-addressed attachment: %w", err)
		}
		var ok bool
		if c.hash, ok = casPathHash(storagePath); ok {
			candidates = append(candidates, c)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("list hashless content-addressed attachments: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close hashless content-addressed attachments: %w", err)
	}

	for _, c := range candidates {
		if c.partKey == "" && c.sourceAttachID != "" {
			var taken bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
				SELECT 1 FROM attachments WHERE message_id = ? AND source_part_key = ?
			)`, c.messageID, c.sourceAttachID).Scan(&taken); err != nil {
				return fmt.Errorf("check attachment %d source-part key: %w", c.id, err)
			}
			if !taken {
				c.partKey = c.sourceAttachID
			}
		}
		if c.partKey == "" {
			var taken bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
				SELECT 1 FROM attachments
				WHERE message_id = ? AND content_hash = ? AND source_part_key IS NULL
			)`, c.messageID, c.hash).Scan(&taken); err != nil {
				return fmt.Errorf("check attachment %d content hash: %w", c.id, err)
			}
			if taken {
				c.partKey = fmt.Sprintf("legacy-alias:%d", c.id)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE attachments SET content_hash = ?, source_part_key = NULLIF(?, '') WHERE id = ?
		`, c.hash, c.partKey, c.id); err != nil {
			return fmt.Errorf("recover attachment %d content hash: %w", c.id, err)
		}
	}
	return nil
}
