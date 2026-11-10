package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	msgmime "go.kenn.io/msgvault/internal/mime"
)

// DuplicateMergePreservesHistoryContext checks content that deduplication does
// not transfer. Archive-only copies must remain visible if that content is unique.
func (s *Store) DuplicateMergePreservesHistoryContext(ctx context.Context, survivorID int64, duplicateIDs []int64) (bool, error) {
	return duplicateMergePreservesHistory(ctx, s.db, survivorID, duplicateIDs)
}

func duplicateMergePreservesHistory(ctx context.Context, q contextStatementQuerier, survivorID int64, duplicateIDs []int64) (bool, error) {
	var retainedMIME map[duplicateMIMEAttachment]int
	retainedMIMERead := false
	for _, id := range duplicateIDs {
		var archiveOnly bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM source_merge_archive_only_messages WHERE message_id = ?
		)`, id).Scan(&archiveOnly); err != nil {
			return false, fmt.Errorf("check duplicate %d archive status: %w", id, err)
		}
		if !archiveOnly {
			continue
		}
		var historicalText, historicalHTML, survivorText, survivorHTML sql.NullString
		err := q.QueryRowContext(ctx, `SELECT body_text, body_html FROM message_bodies WHERE message_id = ?`, id).
			Scan(&historicalText, &historicalHTML)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("read historical duplicate %d body: %w", id, err)
		}
		err = q.QueryRowContext(ctx, `SELECT body_text, body_html FROM message_bodies WHERE message_id = ?`, survivorID).
			Scan(&survivorText, &survivorHTML)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("read survivor %d body: %w", survivorID, err)
		}
		if historicalText.String != "" && historicalText.String != survivorText.String ||
			historicalHTML.String != "" && historicalHTML.String != survivorHTML.String {
			return false, nil
		}
		var missingAttachment bool
		// Match physical occurrences, not just distinct content. Stored parts
		// also need enough stored matches, even when other copies lack bytes.
		err = q.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM (
				SELECT content_hash, COALESCE(filename, '') AS filename,
				       COALESCE(mime_type, '') AS mime_type, attachment_role,
				       COALESCE(content_id, '') AS content_id, COUNT(*) AS occurrences,
				       COUNT(CASE WHEN storage_path <> '' THEN 1 END) AS stored_occurrences
				FROM attachments WHERE message_id = ?
				GROUP BY content_hash, COALESCE(filename, ''), COALESCE(mime_type, ''),
				         attachment_role, COALESCE(content_id, '')
			) historical
			WHERE
				COALESCE(historical.content_hash, '') = '' OR NOT EXISTS (
					SELECT COUNT(*) FROM attachments survivor
					WHERE survivor.message_id = ?
					  AND survivor.content_hash = historical.content_hash
					  AND COALESCE(survivor.filename, '') = COALESCE(historical.filename, '')
					  AND COALESCE(survivor.mime_type, '') = COALESCE(historical.mime_type, '')
					  AND survivor.attachment_role = historical.attachment_role
					  AND COALESCE(survivor.content_id, '') = COALESCE(historical.content_id, '')
					HAVING COUNT(*) >= historical.occurrences
					   AND COUNT(CASE WHEN survivor.storage_path <> '' THEN 1 END) >= historical.stored_occurrences
				)
		)`, id, survivorID).Scan(&missingAttachment)
		if err != nil {
			return false, fmt.Errorf("compare historical duplicate %d attachments: %w", id, err)
		}
		if missingAttachment {
			return false, nil
		}
		historicalMIME, err := duplicateMIMEAttachments(ctx, q, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if errors.Is(err, ErrInvalidMessageRaw) {
			return false, nil // Unreadable MIME cannot prove preservation.
		}
		if err != nil {
			return false, err
		}
		if len(historicalMIME) == 0 {
			continue
		}
		if !retainedMIMERead {
			// MergeDuplicates fills missing survivor raw data from the first
			// duplicate with raw data. Check that same retained copy.
			for _, rawID := range append([]int64{survivorID}, duplicateIDs...) {
				retainedMIME, err = duplicateMIMEAttachments(ctx, q, rawID)
				if !errors.Is(err, sql.ErrNoRows) {
					break
				}
			}
			if errors.Is(err, ErrInvalidMessageRaw) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			retainedMIMERead = true
		}
		for part, occurrences := range historicalMIME {
			if retainedMIME[part] >= occurrences {
				continue
			}
			// Extracted bytes can preserve a part even if the survivor's raw
			// MIME omits it. Do not count hash-only rows as retained bytes.
			var stored int
			err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments
				WHERE message_id = ? AND content_hash = ? AND COALESCE(filename, '') = ?
				  AND COALESCE(mime_type, '') = ? AND attachment_role = ?
				  AND COALESCE(content_id, '') = ? AND storage_path <> ''`,
				survivorID, part.hash, part.filename, part.mimeType, part.role, part.contentID).Scan(&stored)
			if err != nil {
				return false, fmt.Errorf("compare historical duplicate %d MIME attachments: %w", id, err)
			}
			if stored < occurrences {
				return false, nil
			}
		}
	}
	return true, nil
}

// Attachment identity excludes source-local part keys but retains occurrence counts.
type duplicateMIMEAttachment struct {
	hash, filename, mimeType, contentID string
	role                                AttachmentRole
}

func duplicateMIMEAttachments(ctx context.Context, q contextStatementQuerier, messageID int64) (map[duplicateMIMEAttachment]int, error) {
	var compressed []byte
	var compression sql.NullString
	var format string
	if err := q.QueryRowContext(ctx, `SELECT raw_data, compression, raw_format FROM message_raw
		WHERE message_id = ?`, messageID).Scan(&compressed, &compression, &format); err != nil {
		return nil, fmt.Errorf("read duplicate %d raw data: %w", messageID, err)
	}
	if format != "mime" {
		return map[duplicateMIMEAttachment]int{}, nil
	}
	raw, err := decodeMessageRaw(compressed, compression)
	if err != nil {
		return nil, err
	}
	parsed, err := msgmime.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: parse duplicate %d MIME: %w", ErrInvalidMessageRaw, messageID, err)
	}
	counts := make(map[duplicateMIMEAttachment]int)
	for _, part := range parsed.Attachments {
		role, _ := AttachmentRoleFromMIME(part.Disposition, part.IsInline, part.ContentID)
		counts[duplicateMIMEAttachment{part.ContentHash, part.Filename, part.ContentType, part.ContentID, role}]++
	}
	return counts, nil
}
