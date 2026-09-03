package store

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	internalmime "go.kenn.io/msgvault/internal/mime"
)

const (
	attachmentRoleRepairMaxMessageBytes = 64 << 20
	attachmentRoleRepairMaxBatchBytes   = 128 << 20
)

var errAttachmentRoleRepairMessageTooLarge = errors.New("attachment role repair message exceeds decompression limit")

// AttachmentRoleRepairProgress describes one committed repair batch. Counts
// cover this call; LastMessageID and Completed are the durable cursor state.
type AttachmentRoleRepairProgress struct {
	LastMessageID      int64
	MessagesScanned    int
	AttachmentsUpdated int
	Completed          bool
}

type attachmentRoleRepairUpdate struct {
	attachmentID int64
	messageID    int64
	contentHash  string
	role         AttachmentRole
	partKey      string
	contentID    string
}

// RepairHistoricalAttachmentRolesBatch repairs at most batchSize historical
// email messages. Only a unique raw-MIME part/hash match can update a row;
// malformed, missing, or ambiguous evidence remains unknown. The updates and
// cursor advance commit atomically, so cancellation is safely resumable.
func (s *Store) RepairHistoricalAttachmentRolesBatch(
	ctx context.Context,
	batchSize int,
) (AttachmentRoleRepairProgress, error) {
	if err := ctx.Err(); err != nil {
		return AttachmentRoleRepairProgress{}, err
	}
	if batchSize <= 0 {
		return AttachmentRoleRepairProgress{}, errors.New("attachment role repair batch size must be positive")
	}

	progress, err := s.attachmentRoleRepairCursor(ctx)
	if err != nil {
		return AttachmentRoleRepairProgress{}, err
	}
	messageIDs, hasMore, err := s.nextAttachmentRoleRepairMessageIDs(
		ctx, progress.LastMessageID, batchSize)
	if err != nil {
		return AttachmentRoleRepairProgress{}, err
	}
	updates := make([]attachmentRoleRepairUpdate, 0)
	lastMessageID := progress.LastMessageID
	messagesScanned := 0
	decompressedBytes := int64(0)
	for _, messageID := range messageIDs {
		if err := ctx.Err(); err != nil {
			return AttachmentRoleRepairProgress{}, err
		}
		remainingBytes := int64(attachmentRoleRepairMaxBatchBytes) - decompressedBytes
		if remainingBytes <= 0 {
			hasMore = true
			break
		}
		compressed, compression, err := s.attachmentRoleRepairMessageRaw(ctx, messageID)
		if err != nil {
			return AttachmentRoleRepairProgress{}, err
		}
		decodeLimit := min(int64(attachmentRoleRepairMaxMessageBytes), remainingBytes)
		raw, bytesRead, decodeErr := decodeMessageRawBounded(
			compressed, compression, decodeLimit, errAttachmentRoleRepairMessageTooLarge)
		if errors.Is(decodeErr, errAttachmentRoleRepairMessageTooLarge) &&
			decodeLimit < attachmentRoleRepairMaxMessageBytes {
			hasMore = true
			break
		}
		decompressedBytes += bytesRead
		messagesScanned++
		lastMessageID = messageID
		// Corrupt or individually oversized MIME is not authoritative evidence.
		// Advancing the cursor keeps one bad record from blocking later repairs.
		if decodeErr != nil {
			continue
		}
		messageUpdates, err := s.prepareAttachmentRoleRepair(messageID, raw)
		if err != nil {
			return AttachmentRoleRepairProgress{}, err
		}
		updates = append(updates, messageUpdates...)
	}
	if s.attachmentRoleRepairPreparedHook != nil {
		s.attachmentRoleRepairPreparedHook()
	}

	completed := !hasMore && messagesScanned == len(messageIDs)
	updated := 0
	err = s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		for _, update := range updates {
			merged, mergeErr := mergeDuplicateAttachmentRoleRepair(ctx, tx, update)
			if mergeErr != nil {
				return fmt.Errorf("merge attachment %d role repair duplicate: %w", update.attachmentID, mergeErr)
			}
			if merged {
				updated++
				if _, err := tx.ExecContext(ctx, `
				UPDATE messages
				SET has_attachments = (SELECT COUNT(*) FROM attachments WHERE message_id = ?) > 0,
				    attachment_count = (SELECT COUNT(*) FROM attachments WHERE message_id = ?)
				WHERE id = ?
			`, update.messageID, update.messageID, update.messageID); err != nil {
					return fmt.Errorf("refresh message %d attachment stats: %w", update.messageID, err)
				}
				continue
			}
			result, err := tx.ExecContext(ctx, `
				UPDATE attachments
				SET attachment_role = ?, role_source = ?, source_part_key = ?, content_id = ?
				WHERE id = ?
				  AND attachment_role = 'unknown'
				  AND source_part_key IS NULL
				  AND content_hash = ?
				  AND NOT EXISTS (
					SELECT 1 FROM attachments keyed
					WHERE keyed.message_id = ? AND keyed.source_part_key = ?
				  )
			`, string(update.role), string(AttachmentRoleSourceRawMIMERepair),
				update.partKey, nullIfEmpty(update.contentID), update.attachmentID,
				update.contentHash, update.messageID, update.partKey)
			if err != nil {
				return fmt.Errorf("repair attachment %d role: %w", update.attachmentID, err)
			}
			if count, err := result.RowsAffected(); err == nil {
				updated += int(count)
			}
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO attachment_role_repair_progress
				(singleton, last_message_id, completed, updated_at)
			VALUES (1, ?, ?, %s)
			ON CONFLICT(singleton) DO UPDATE SET
				last_message_id = excluded.last_message_id,
				completed = excluded.completed,
				updated_at = excluded.updated_at
		`, s.dialect.Now()), lastMessageID, boolInt(completed))
		return err
	})
	if err != nil {
		return AttachmentRoleRepairProgress{}, err
	}
	return AttachmentRoleRepairProgress{
		LastMessageID:      lastMessageID,
		MessagesScanned:    messagesScanned,
		AttachmentsUpdated: updated,
		Completed:          completed,
	}, nil
}

// mergeDuplicateAttachmentRoleRepair removes a legacy hash-identified row
// when a resync has already created the same source occurrence. The keyed row
// is authoritative and already carries the current storage/reference data;
// only an exact content-hash match is safe to collapse.
func mergeDuplicateAttachmentRoleRepair(
	ctx context.Context,
	tx *loggedTx,
	update attachmentRoleRepairUpdate,
) (bool, error) {
	var keyedID int64
	var keyedHash string
	err := tx.QueryRowContext(ctx, `
		SELECT id, COALESCE(content_hash, '')
		FROM attachments
		WHERE message_id = ? AND source_part_key = ?
	`, update.messageID, update.partKey).Scan(&keyedID, &keyedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if keyedHash == "" || keyedHash != update.contentHash {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, `
		DELETE FROM attachments
		WHERE id = ?
		  AND message_id = ?
		  AND attachment_role = 'unknown'
		  AND source_part_key IS NULL
		  AND content_hash = ?
	`, update.attachmentID, update.messageID, update.contentHash)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

func (s *Store) attachmentRoleRepairCursor(ctx context.Context) (AttachmentRoleRepairProgress, error) {
	var progress AttachmentRoleRepairProgress
	var completed int
	err := s.db.QueryRowContext(ctx, `
		SELECT last_message_id, completed
		FROM attachment_role_repair_progress WHERE singleton = 1
	`).Scan(&progress.LastMessageID, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return progress, nil
	}
	if err != nil {
		return AttachmentRoleRepairProgress{}, fmt.Errorf("read attachment role repair cursor: %w", err)
	}
	progress.Completed = completed != 0
	return progress, nil
}

func (s *Store) nextAttachmentRoleRepairMessageIDs(
	ctx context.Context,
	afterMessageID int64,
	batchSize int,
) ([]int64, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id
		FROM messages m
		JOIN message_raw mr ON mr.message_id = m.id AND mr.raw_format = 'mime'
		WHERE m.id > ?
		  AND EXISTS (
			SELECT 1 FROM attachments a
			WHERE a.message_id = m.id AND a.attachment_role = 'unknown'
		  )
		ORDER BY m.id
		LIMIT ?
	`, afterMessageID, batchSize+1)
	if err != nil {
		return nil, false, fmt.Errorf("list attachment role repair messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	messageIDs := make([]int64, 0, batchSize+1)
	for rows.Next() {
		var messageID int64
		if err := rows.Scan(&messageID); err != nil {
			return nil, false, err
		}
		messageIDs = append(messageIDs, messageID)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(messageIDs) > batchSize
	if hasMore {
		messageIDs = messageIDs[:batchSize]
	}
	return messageIDs, hasMore, nil
}

func (s *Store) attachmentRoleRepairMessageRaw(
	ctx context.Context,
	messageID int64,
) ([]byte, sql.NullString, error) {
	var compressed []byte
	var compression sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT raw_data, compression
		FROM message_raw
		WHERE message_id = ? AND raw_format = 'mime'
	`, messageID).Scan(&compressed, &compression); err != nil {
		return nil, sql.NullString{}, fmt.Errorf("read message %d for attachment role repair: %w", messageID, err)
	}
	return compressed, compression, nil
}

// decodeMessageRawBounded decompresses at most maxBytes of a stored raw
// record. Decompression is capped with an io.LimitReader of maxBytes+1, so a
// stream larger than the cap is detected without materializing it, and the
// caller-supplied tooLargeErr is returned instead. bytesRead reports how many
// decompressed bytes were consumed before the cap or stream end.
func decodeMessageRawBounded(
	compressed []byte,
	compression sql.NullString,
	maxBytes int64,
	tooLargeErr error,
) ([]byte, int64, error) {
	if maxBytes <= 0 {
		return nil, 0, tooLargeErr
	}
	if !compression.Valid || compression.String != "zlib" {
		if int64(len(compressed)) > maxBytes {
			return nil, maxBytes + 1, tooLargeErr
		}
		return compressed, int64(len(compressed)), nil
	}
	reader, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, 0, fmt.Errorf("zlib reader: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	closeErr := reader.Close()
	bytesRead := int64(len(data))
	if bytesRead > maxBytes {
		return nil, bytesRead, tooLargeErr
	}
	if readErr != nil {
		return nil, bytesRead, readErr
	}
	if closeErr != nil {
		return nil, bytesRead, fmt.Errorf("close zlib reader: %w", closeErr)
	}
	return data, bytesRead, nil
}

func (s *Store) prepareAttachmentRoleRepair(messageID int64, raw []byte) ([]attachmentRoleRepairUpdate, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	parsed, err := internalmime.Parse(raw)
	if err != nil {
		return nil, nil //nolint:nilerr // corrupt MIME is ineligible evidence, not a repair failure.
	}

	type historicalAttachment struct {
		id          int64
		contentHash string
	}
	rows, err := s.db.Query(`
		SELECT id, COALESCE(content_hash, '')
		FROM attachments
		WHERE message_id = ? AND attachment_role = 'unknown'
	`, messageID)
	if err != nil {
		return nil, err
	}
	var historical []historicalAttachment
	for rows.Next() {
		var attachment historicalAttachment
		if err := rows.Scan(&attachment.id, &attachment.contentHash); err != nil {
			_ = rows.Close()
			return nil, err
		}
		historical = append(historical, attachment)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Enmime can expose the same physical part through both Attachments and
	// Inlines. PartKey is the source identity, so collapse that duplicate while
	// preserving the stricter inline evidence. A key with conflicting bytes is
	// corrupt/ambiguous and is excluded entirely.
	partsByKey := make(map[string]internalmime.Attachment, len(parsed.Attachments))
	conflictedKeys := make(map[string]struct{})
	for _, part := range parsed.Attachments {
		if part.PartKey == "" {
			continue
		}
		previous, exists := partsByKey[part.PartKey]
		if !exists {
			partsByKey[part.PartKey] = part
			continue
		}
		if previous.ContentHash != part.ContentHash {
			conflictedKeys[part.PartKey] = struct{}{}
			continue
		}
		if part.IsInline || part.Disposition == "inline" || part.ContentID != "" {
			previous.IsInline = true
			previous.Disposition = "inline"
			if previous.ContentID == "" {
				previous.ContentID = part.ContentID
			}
			partsByKey[part.PartKey] = previous
		}
	}
	partsByHash := make(map[string][]internalmime.Attachment, len(partsByKey))
	for partKey, part := range partsByKey {
		if _, conflicted := conflictedKeys[partKey]; conflicted {
			continue
		}
		partsByHash[part.ContentHash] = append(partsByHash[part.ContentHash], part)
	}
	rowsByHash := make(map[string]int, len(historical))
	for _, attachment := range historical {
		rowsByHash[attachment.contentHash]++
	}

	updates := make([]attachmentRoleRepairUpdate, 0, len(historical))
	for _, attachment := range historical {
		parts := partsByHash[attachment.contentHash]
		if attachment.contentHash == "" || rowsByHash[attachment.contentHash] != 1 || len(parts) != 1 {
			continue
		}
		part := parts[0]
		role, _ := AttachmentRoleFromMIME(part.Disposition, part.IsInline, part.ContentID)
		if role == AttachmentRoleUnknown || part.PartKey == "" {
			continue
		}
		updates = append(updates, attachmentRoleRepairUpdate{
			attachmentID: attachment.id,
			messageID:    messageID,
			contentHash:  attachment.contentHash,
			role:         role,
			partKey:      part.PartKey,
			contentID:    part.ContentID,
		})
	}
	return updates, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
