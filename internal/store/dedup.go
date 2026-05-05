package store

import (
	"bytes"
	"compress/zlib"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wesm/msgvault/internal/mime"
)

// DuplicateGroupKey identifies a group of messages sharing the same
// RFC822 Message-ID. Lightweight return type for the store layer.
type DuplicateGroupKey struct {
	RFC822MessageID string
	Count           int
}

// DuplicateMessageRow holds metadata needed to select the survivor in a
// duplicate group. Lightweight return type for the store layer.
type DuplicateMessageRow struct {
	ID               int64
	SourceID         int64
	SourceType       string
	SourceIdentifier string
	SourceMessageID  string
	Subject          string
	SentAt           time.Time
	ArchivedAt       time.Time
	HasRawMIME       bool
	LabelCount       int
	IsFromMe         bool
	HasSentLabel     bool // true if the message has the Gmail SENT label
	// Raw From: address with original case preserved. The dedup engine
	// normalizes via NormalizeIdentifierForCompare for identity-match
	// sent detection, which is case-insensitive for email shapes and
	// case-sensitive for synthetic identifiers (Matrix MXIDs, chat
	// handles).
	FromEmail string
}

// MergeResult holds the counts from a MergeDuplicates operation.
type MergeResult struct {
	LabelsTransferred int
	RawMIMEBackfilled int
}

// ContentHashCandidate holds message metadata for raw-MIME hash scans.
type ContentHashCandidate struct {
	ID               int64
	SourceID         int64
	SourceType       string
	SourceIdentifier string
	SourceMessageID  string
	Subject          string
	SentAt           time.Time
	ArchivedAt       time.Time
	LabelCount       int
	IsFromMe         bool
	HasSentLabel     bool
	FromEmail        string
}

func (s *Store) FindDuplicatesByRFC822ID(sourceIDs ...int64) ([]DuplicateGroupKey, error) {
	query := `
		SELECT rfc822_message_id, COUNT(*) AS cnt
		FROM messages
		WHERE rfc822_message_id IS NOT NULL
		  AND rfc822_message_id != ''
		  AND ` + LiveMessagesWhere("", true)
	var args []any
	if len(sourceIDs) > 0 {
		placeholders := make([]string, len(sourceIDs))
		for i, id := range sourceIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND source_id IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += `
		GROUP BY rfc822_message_id
		HAVING cnt > 1`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("find duplicates by rfc822 id: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var groups []DuplicateGroupKey
	for rows.Next() {
		var g DuplicateGroupKey
		if err := rows.Scan(&g.RFC822MessageID, &g.Count); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (s *Store) GetDuplicateGroupMessages(
	rfc822ID string, sourceIDs ...int64,
) ([]DuplicateMessageRow, error) {
	query := `
		SELECT m.id, m.source_id, s.source_type, s.identifier,
		       m.source_message_id,
		       COALESCE(m.subject, ''), m.sent_at, m.archived_at,
		       (CASE WHEN mr.message_id IS NOT NULL THEN 1 ELSE 0 END) AS has_raw,
		       (SELECT COUNT(*) FROM message_labels ml
		          WHERE ml.message_id = m.id) AS label_count,
		       COALESCE(m.is_from_me, 0) AS is_from_me,
		       CAST(EXISTS (
		           SELECT 1 FROM message_labels ml2
		           JOIN labels l ON l.id = ml2.label_id
		           WHERE ml2.message_id = m.id
		             AND (l.source_label_id = 'SENT' OR UPPER(l.name) = 'SENT')
		       ) AS INTEGER) AS has_sent_label,
		       COALESCE((
		           SELECT p_from.email_address
		           FROM message_recipients mr_from
		           JOIN participants p_from
		             ON p_from.id = mr_from.participant_id
		           WHERE mr_from.message_id = m.id
		             AND mr_from.recipient_type = 'from'
		           LIMIT 1
		       ), '') AS from_email
		FROM messages m
		JOIN sources s ON s.id = m.source_id
		LEFT JOIN message_raw mr ON mr.message_id = m.id
		WHERE m.rfc822_message_id = ? AND ` + LiveMessagesWhere("m", true)
	args := []any{rfc822ID}
	if len(sourceIDs) > 0 {
		placeholders := make([]string, len(sourceIDs))
		for i, id := range sourceIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND m.source_id IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY m.id"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get duplicate group messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var msgs []DuplicateMessageRow
	for rows.Next() {
		var dm DuplicateMessageRow
		var sentAt, archivedAt sql.NullTime
		var hasRaw, isFromMe, hasSent int
		if err := rows.Scan(
			&dm.ID, &dm.SourceID, &dm.SourceType, &dm.SourceIdentifier,
			&dm.SourceMessageID, &dm.Subject, &sentAt, &archivedAt,
			&hasRaw, &dm.LabelCount, &isFromMe, &hasSent,
			&dm.FromEmail,
		); err != nil {
			return nil, err
		}
		if sentAt.Valid {
			dm.SentAt = sentAt.Time
		}
		if archivedAt.Valid {
			dm.ArchivedAt = archivedAt.Time
		}
		dm.HasRawMIME = hasRaw == 1
		dm.IsFromMe = isFromMe == 1
		dm.HasSentLabel = hasSent == 1
		msgs = append(msgs, dm)
	}
	return msgs, rows.Err()
}

func (s *Store) MergeDuplicates(
	survivorID int64, duplicateIDs []int64, batchID string,
) (*MergeResult, error) {
	if len(duplicateIDs) == 0 {
		return &MergeResult{}, nil
	}

	result := &MergeResult{}
	unionLabelsSQL := s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO message_labels (message_id, label_id)
			SELECT ?, label_id FROM message_labels WHERE message_id = ?`)
	backfillRawSQL := s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO message_raw
			  (message_id, raw_data, raw_format, compression)
			SELECT ?, raw_data, raw_format, compression
			FROM message_raw WHERE message_id = ?`)
	softDeleteSQL := fmt.Sprintf(`UPDATE messages
			SET deleted_at = %s, delete_batch_id = ?
			WHERE id = ?`, s.dialect.Now())

	err := s.withTx(func(tx *loggedTx) error {
		for _, dupID := range duplicateIDs {
			res, err := tx.Exec(unionLabelsSQL, survivorID, dupID)
			if err != nil {
				return fmt.Errorf("union labels from %d: %w", dupID, err)
			}
			affected, _ := res.RowsAffected()
			result.LabelsTransferred += int(affected)
		}

		var survivorHasRaw int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM message_raw WHERE message_id = ?`,
			survivorID,
		).Scan(&survivorHasRaw); err != nil {
			return fmt.Errorf("check survivor raw MIME: %w", err)
		}
		if survivorHasRaw == 0 {
			for _, dupID := range duplicateIDs {
				res, err := tx.Exec(backfillRawSQL, survivorID, dupID)
				if err != nil {
					return fmt.Errorf("backfill raw MIME from %d: %w", dupID, err)
				}
				affected, _ := res.RowsAffected()
				if affected > 0 {
					result.RawMIMEBackfilled += int(affected)
					break
				}
			}
		}

		for _, dupID := range duplicateIDs {
			if _, err := tx.Exec(softDeleteSQL, batchID, dupID); err != nil {
				return fmt.Errorf("soft-delete duplicate %d: %w", dupID, err)
			}
		}
		return nil
	})
	return result, err
}

func (s *Store) GetAllRawMIMECandidates(
	sourceIDs ...int64,
) ([]ContentHashCandidate, error) {
	query := `
		SELECT m.id, m.source_id, s.source_type, s.identifier,
		       m.source_message_id,
		       COALESCE(m.subject, ''), m.sent_at, m.archived_at,
		       (SELECT COUNT(*) FROM message_labels ml
		          WHERE ml.message_id = m.id) AS label_count,
		       COALESCE(m.is_from_me, 0) AS is_from_me,
		       CAST(EXISTS (
		           SELECT 1 FROM message_labels ml2
		           JOIN labels l ON l.id = ml2.label_id
		           WHERE ml2.message_id = m.id
		             AND (l.source_label_id = 'SENT' OR UPPER(l.name) = 'SENT')
		       ) AS INTEGER) AS has_sent_label,
		       COALESCE((
		           SELECT p_from.email_address
		           FROM message_recipients mr_from
		           JOIN participants p_from
		             ON p_from.id = mr_from.participant_id
		           WHERE mr_from.message_id = m.id
		             AND mr_from.recipient_type = 'from'
		           LIMIT 1
		       ), '') AS from_email
		FROM messages m
		JOIN sources s ON s.id = m.source_id
		JOIN message_raw mr ON mr.message_id = m.id
		WHERE ` + LiveMessagesWhere("m", true)
	var args []any
	if len(sourceIDs) > 0 {
		placeholders := make([]string, len(sourceIDs))
		for i, id := range sourceIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND m.source_id IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY m.id"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get all raw MIME candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []ContentHashCandidate
	for rows.Next() {
		var c ContentHashCandidate
		var sentAt, archivedAt sql.NullTime
		var isFromMe, hasSent int
		if err := rows.Scan(
			&c.ID, &c.SourceID, &c.SourceType, &c.SourceIdentifier,
			&c.SourceMessageID, &c.Subject, &sentAt, &archivedAt,
			&c.LabelCount, &isFromMe, &hasSent, &c.FromEmail,
		); err != nil {
			return nil, err
		}
		if sentAt.Valid {
			c.SentAt = sentAt.Time
		}
		if archivedAt.Valid {
			c.ArchivedAt = archivedAt.Time
		}
		c.IsFromMe = isFromMe == 1
		c.HasSentLabel = hasSent == 1
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

func (s *Store) StreamMessageRaw(
	messageIDs []int64,
	fn func(messageID int64, rawData []byte, compression string),
) error {
	const chunkSize = 500
	for start := 0; start < len(messageIDs); start += chunkSize {
		end := min(start+chunkSize, len(messageIDs))
		chunk := messageIDs[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}

		query := "SELECT message_id, raw_data, compression FROM message_raw WHERE message_id IN (" +
			strings.Join(placeholders, ",") + ")"
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return fmt.Errorf("stream message raw: %w", err)
		}

		for rows.Next() {
			var msgID int64
			var rawData []byte
			var compression sql.NullString
			if err := rows.Scan(&msgID, &rawData, &compression); err != nil {
				_ = rows.Close()
				return err
			}
			comp := ""
			if compression.Valid {
				comp = compression.String
			}
			fn(msgID, rawData, comp)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
	}
	return nil
}

// UndoDedup restores soft-deleted duplicates from a dedup batch by
// clearing deleted_at and delete_batch_id. Merge side effects (labels
// copied to survivors, raw MIME backfilled onto survivors) are not
// reversed — those changes are additive enrichment that leaves
// survivors strictly better off.
func (s *Store) UndoDedup(batchID string) (int64, error) {
	result, err := s.db.Exec(`
		UPDATE messages
		SET deleted_at = NULL, delete_batch_id = NULL
		WHERE delete_batch_id = ?
	`, batchID)
	if err != nil {
		return 0, fmt.Errorf("undo dedup: %w", err)
	}
	return result.RowsAffected()
}

// DeleteDedupedBatch permanently deletes all hidden rows associated with a
// dedup batch. Only deletes rows where deleted_at IS NOT NULL AND
// delete_batch_id = batchID. Returns the number of rows deleted.
//
// This is irreversible. Caller is responsible for backups.
// Attachments cascade-delete from the metadata row; on-disk blobs are
// content-addressed and survive until separate cleanup.
func (s *Store) DeleteDedupedBatch(batchID string) (int64, error) {
	result, err := s.db.Exec(`
		DELETE FROM messages
		WHERE delete_batch_id = ? AND deleted_at IS NOT NULL
	`, batchID)
	if err != nil {
		return 0, fmt.Errorf("delete dedup batch %q: %w", batchID, err)
	}
	return result.RowsAffected()
}

// DeleteAllDeduped permanently deletes every dedup-hidden row regardless of
// batch. Returns the number of rows deleted and the number of distinct
// batches affected. Distinct-batch counting only considers rows that have a
// batch ID; the delete itself is not gated on batch ID, so any future code
// path that soft-hides a row without a batch is also captured.
//
// This is irreversible. Caller is responsible for backups.
// Attachments cascade-delete from the metadata row; on-disk blobs are
// content-addressed and survive until separate cleanup.
//
// FUTURE REFINEMENT (out of scope for #304): the predicate
// `deleted_at IS NOT NULL` is currently safe because `deleted_at` is
// only ever set by the dedup soft-hide path. If a future feature adds
// any other soft-delete semantics that share this column (e.g. a
// "trash" view, a per-message hide), this delete will silently destroy
// those rows alongside dedup-hidden ones. The right shape if/when that
// happens is either rename the column to make the contract explicit
// (e.g. `dedup_hidden_at`) or gate the delete on a positive marker
// (`delete_batch_id IS NOT NULL OR dedup_origin = 1`-style). Flagged
// here so the future change-author notices before introducing the
// ambiguity. Iter15 review surfaced this as future-proofing.
func (s *Store) DeleteAllDeduped() (deleted int64, distinctBatches int64, err error) {
	committed := false
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("delete all dedup-hidden: begin tx: %w", err)
	}
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err = tx.QueryRow(`
		SELECT COUNT(DISTINCT delete_batch_id)
		FROM messages
		WHERE deleted_at IS NOT NULL AND delete_batch_id IS NOT NULL
	`).Scan(&distinctBatches); err != nil {
		return 0, 0, fmt.Errorf("delete all dedup-hidden: count batches: %w", err)
	}

	result, err := tx.Exec(`
		DELETE FROM messages
		WHERE deleted_at IS NOT NULL
	`)
	if err != nil {
		return 0, 0, fmt.Errorf("delete all dedup-hidden: delete: %w", err)
	}
	deleted, err = result.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("delete all dedup-hidden: rows affected: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("delete all dedup-hidden: commit: %w", err)
	}
	committed = true
	return deleted, distinctBatches, nil
}

func (s *Store) CountActiveMessages(sourceIDs ...int64) (int64, error) {
	query := "SELECT COUNT(*) FROM messages WHERE " + LiveMessagesWhere("", true)
	var args []any
	if len(sourceIDs) > 0 {
		placeholders := make([]string, len(sourceIDs))
		for i, id := range sourceIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " AND source_id IN (" + strings.Join(placeholders, ",") + ")"
	}
	var count int64
	err := s.db.QueryRow(query, args...).Scan(&count)
	return count, err
}

func (s *Store) CountMessagesWithoutRFC822ID(sourceIDs ...int64) (int64, error) {
	q := `SELECT COUNT(*) FROM messages m
		JOIN message_raw mr ON mr.message_id = m.id
		WHERE (m.rfc822_message_id IS NULL OR m.rfc822_message_id = '')
		  AND ` + LiveMessagesWhere("m", true)
	var args []any
	if len(sourceIDs) > 0 {
		placeholders := make([]string, len(sourceIDs))
		for i, id := range sourceIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		q += " AND m.source_id IN (" + strings.Join(placeholders, ",") + ")"
	}
	var count int64
	err := s.db.QueryRow(q, args...).Scan(&count)
	return count, err
}

func (s *Store) BackfillRFC822IDs(
	sourceIDs []int64,
	progress func(done, total int64),
) (updated int64, failed int64, err error) {
	scopeClause := ""
	var scopeArgs []any
	if len(sourceIDs) > 0 {
		placeholders := make([]string, len(sourceIDs))
		for i, id := range sourceIDs {
			placeholders[i] = "?"
			scopeArgs = append(scopeArgs, id)
		}
		scopeClause = " AND m.source_id IN (" + strings.Join(placeholders, ",") + ")"
	}

	var total int64
	countQ := `SELECT COUNT(*) FROM messages m
		JOIN message_raw mr ON mr.message_id = m.id
		WHERE (m.rfc822_message_id IS NULL OR m.rfc822_message_id = '')
		  AND ` + LiveMessagesWhere("m", true) + scopeClause
	err = s.db.QueryRow(countQ, scopeArgs...).Scan(&total)
	if err != nil {
		return 0, 0, fmt.Errorf("count backfill candidates: %w", err)
	}
	if total == 0 {
		return 0, 0, nil
	}

	const batchSize = 1000
	lastID := int64(0)

	for {
		batchQ := `SELECT m.id FROM messages m
			JOIN message_raw mr ON mr.message_id = m.id
			WHERE (m.rfc822_message_id IS NULL OR m.rfc822_message_id = '')
			  AND ` + LiveMessagesWhere("m", true) + `
			  AND m.id > ?` + scopeClause + `
			ORDER BY m.id
			LIMIT ?`
		batchArgs := append([]any{lastID}, scopeArgs...)
		batchArgs = append(batchArgs, batchSize)
		rows, err := s.db.Query(batchQ, batchArgs...)
		if err != nil {
			return updated, failed, fmt.Errorf("fetch backfill batch: %w", err)
		}

		var batchIDs []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return updated, failed, err
			}
			batchIDs = append(batchIDs, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return updated, failed, err
		}
		if len(batchIDs) == 0 {
			break
		}

		updates := make([]struct {
			id           int64
			normalizedID string
		}, 0, len(batchIDs))
		// Failed rows are not updated and are not retried in this run:
		// because lastID advances past every batch element below, the
		// next batch query (m.id > lastID) skips them. The selection
		// filter (rfc822_message_id IS NULL OR '') will pick them up
		// again on the next BackfillRFC822IDs invocation.
		seen := make(map[int64]bool, len(batchIDs))
		streamErr := s.StreamMessageRaw(batchIDs, func(id int64, rawData []byte, compression string) {
			seen[id] = true
			raw := rawData
			if compression == "zlib" {
				r, err := zlib.NewReader(bytes.NewReader(rawData))
				if err != nil {
					failed++
					return
				}
				decompressed, err := io.ReadAll(r)
				_ = r.Close()
				if err != nil {
					failed++
					return
				}
				raw = decompressed
			}
			parsed, err := mime.Parse(raw)
			if err != nil || parsed.MessageID == "" {
				failed++
				return
			}
			normalizedID := strings.TrimSpace(parsed.MessageID)
			normalizedID = strings.Trim(normalizedID, "<>")
			if normalizedID == "" {
				failed++
				return
			}
			updates = append(updates, struct {
				id           int64
				normalizedID string
			}{
				id:           id,
				normalizedID: normalizedID,
			})
		})
		if streamErr != nil {
			return updated, failed, fmt.Errorf("stream raw for backfill batch: %w", streamErr)
		}
		// Rows whose message_raw row went missing between the batch
		// SELECT and the stream are counted as failed so totals reconcile.
		for _, id := range batchIDs {
			if !seen[id] {
				failed++
			}
		}

		var batchUpdated int64
		err = s.withTx(func(tx *loggedTx) error {
			for _, update := range updates {
				if _, err := tx.Exec(
					"UPDATE messages SET rfc822_message_id = ? WHERE id = ?",
					update.normalizedID, update.id,
				); err != nil {
					return fmt.Errorf("update message %d: %w", update.id, err)
				}
				batchUpdated++
			}
			return nil
		})
		if err != nil {
			return updated, failed, fmt.Errorf(
				"apply backfill batch ending at %d: %w",
				batchIDs[len(batchIDs)-1], err,
			)
		}
		updated += batchUpdated

		lastID = batchIDs[len(batchIDs)-1]
		if progress != nil {
			progress(updated+failed, total)
		}
	}
	return updated, failed, nil
}
