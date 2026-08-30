package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/mime"
)

// DuplicateGroupKey identifies a group of messages sharing the same
// RFC822 Message-ID. Lightweight return type for the store layer.
type DuplicateGroupKey struct {
	RFC822MessageID string
	Count           int
}

// ErrRFC822StorageFormCollision means two requested duplicate-group keys
// expand to the same stored Message-ID representation.
var ErrRFC822StorageFormCollision = errors.New("RFC822 Message-ID storage form collision")

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
	PayloadBytes     int64
	AttachmentCount  int
	HasAttachments   bool
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
	PayloadBytes     int64
	AttachmentCount  int
	HasAttachments   bool
	LabelCount       int
	IsFromMe         bool
	HasSentLabel     bool
	FromEmail        string
}

type DedupedBatchCount struct {
	ID    string
	Count int64
}

type RFC822IDBackfillItem struct {
	MessageID       int64
	SourceID        int64
	RFC822MessageID string
	RawInputSHA256  [sha256.Size]byte
}

type RFC822IDBackfillPlan struct {
	Candidates int64
	Items      []RFC822IDBackfillItem
	Failed     int64
}

func (p RFC822IDBackfillPlan) Digest() string {
	items := append([]RFC822IDBackfillItem(nil), p.Items...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].MessageID != items[j].MessageID {
			return items[i].MessageID < items[j].MessageID
		}
		if items[i].SourceID != items[j].SourceID {
			return items[i].SourceID < items[j].SourceID
		}
		if items[i].RFC822MessageID != items[j].RFC822MessageID {
			return items[i].RFC822MessageID < items[j].RFC822MessageID
		}
		return bytes.Compare(items[i].RawInputSHA256[:], items[j].RawInputSHA256[:]) < 0
	})

	digest := sha256.New()
	writeRFC822IDBackfillInt64(digest, int64(len(items)))
	for _, item := range items {
		writeRFC822IDBackfillInt64(digest, item.MessageID)
		writeRFC822IDBackfillInt64(digest, item.SourceID)
		writeRFC822IDBackfillBytes(digest, []byte(item.RFC822MessageID))
		writeRFC822IDBackfillBytes(digest, item.RawInputSHA256[:])
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeRFC822IDBackfillInt64(dst hash.Hash, value int64) {
	_ = binary.Write(dst, binary.BigEndian, value)
}

func writeRFC822IDBackfillBytes(dst hash.Hash, value []byte) {
	writeRFC822IDBackfillInt64(dst, int64(len(value)))
	_, _ = dst.Write(value)
}

func rfc822IDBackfillRawFingerprint(
	rawData []byte, rawFormat string, compression sql.NullString,
) [sha256.Size]byte {
	digest := sha256.New()
	writeRFC822IDBackfillBytes(digest, rawData)
	writeRFC822IDBackfillBytes(digest, []byte(rawFormat))
	if compression.Valid {
		_, _ = digest.Write([]byte{1})
		writeRFC822IDBackfillBytes(digest, []byte(compression.String))
	} else {
		_, _ = digest.Write([]byte{0})
	}
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], digest.Sum(nil))
	return fingerprint
}

func rfc822MessageIDStorageForms(id string) []string {
	if id == "" {
		return nil
	}

	// Mirror FindDuplicatesByRFC822ID's dialect expression exactly. SQLite
	// groups BLOB bytes so embedded NUL remains ordinary data; PostgreSQL TEXT
	// rejects NUL. Both unwrap <id> only when id's edge bytes are not brackets
	// or an ASCII space. Trimming or UTF-8 repair here would make fetch disagree
	// with discovery.
	forms := []string{id}
	first, last := id[0], id[len(id)-1]
	if first != '<' && first != '>' && first != ' ' &&
		last != '<' && last != '>' && last != ' ' {
		forms = append(forms, "<"+id+">")
	}
	return forms
}

func (s *Store) FindDuplicatesByRFC822ID(sourceIDs ...int64) ([]DuplicateGroupKey, error) {
	canonicalID := s.dialect.RFC822CanonicalIDExpr("rfc822_message_id")
	query := `
		SELECT ` + canonicalID + `, COUNT(*) AS cnt
		FROM messages
		WHERE rfc822_message_id IS NOT NULL
		  AND rfc822_message_id != ''
		  AND ` + canonicalID + ` != ''
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
		GROUP BY ` + canonicalID + `
		HAVING COUNT(*) > 1`

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

// duplicateGroupMessageColumns is the SELECT column list shared by
// GetDuplicateGroupMessages and GetDuplicateGroupMessagesBatch: the message
// metadata, the two correlated subqueries (label count, from address), and
// the EXISTS clause used to detect the Gmail SENT label. Both methods build
// their SELECT clause from this constant so the two queries can't drift
// apart; GetDuplicateGroupMessagesBatch prepends m.rfc822_message_id (needed
// to key its result map) since it has no per-call rfc822ID to bind.
const duplicateGroupMessageColumns = `m.id, m.source_id, s.source_type, s.identifier,
		       m.source_message_id,
		       COALESCE(m.subject, ''), m.sent_at, m.archived_at,
		       (CASE WHEN mr.message_id IS NOT NULL THEN 1 ELSE 0 END) AS has_raw,
		       COALESCE(m.size_estimate, 0) AS payload_bytes,
		       COALESCE(m.attachment_count, 0) AS attachment_count,
		       CASE WHEN COALESCE(m.has_attachments, FALSE) THEN 1 ELSE 0 END AS has_attachments,
		       (SELECT COUNT(*) FROM message_labels ml
		          WHERE ml.message_id = m.id) AS label_count,
		       CASE WHEN COALESCE(m.is_from_me, FALSE) THEN 1 ELSE 0 END AS is_from_me,
		       CASE WHEN EXISTS (
		           SELECT 1 FROM message_labels ml2
		           JOIN labels l ON l.id = ml2.label_id
		           WHERE ml2.message_id = m.id
		             AND (l.source_label_id = 'SENT' OR UPPER(l.name) = 'SENT')
		       ) THEN 1 ELSE 0 END AS has_sent_label,
		       COALESCE((
		           SELECT p_from.email_address
		           FROM message_recipients mr_from
		           JOIN participants p_from
		             ON p_from.id = mr_from.participant_id
		           WHERE mr_from.message_id = m.id
		             AND mr_from.recipient_type = 'from'
		           LIMIT 1
		       ), '') AS from_email`

// GetDuplicateGroupMessages fetches every message row for a single RFC822
// duplicate group in one query. It is retained as the reference
// implementation that the equivalence test in dedup_test.go checks
// GetDuplicateGroupMessagesBatch against, and it is still exercised by its
// own pre-existing direct tests, even though Engine.Scan now calls
// GetDuplicateGroupMessagesBatch instead.
func (s *Store) GetDuplicateGroupMessages(
	rfc822ID string, sourceIDs ...int64,
) ([]DuplicateMessageRow, error) {
	storageForms := rfc822MessageIDStorageForms(rfc822ID)
	if len(storageForms) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(storageForms))
	args := make([]any, 0, len(storageForms)+len(sourceIDs))
	for i, form := range storageForms {
		placeholders[i] = "?"
		args = append(args, form)
	}
	query := `
		SELECT ` + duplicateGroupMessageColumns + `
		FROM messages m
		JOIN sources s ON s.id = m.source_id
		LEFT JOIN message_raw mr ON mr.message_id = m.id
		WHERE m.rfc822_message_id IN (` + strings.Join(placeholders, ",") + `)
		  AND ` + LiveMessagesWhere("m", true)
	if len(sourceIDs) > 0 {
		placeholders = make([]string, len(sourceIDs))
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
		var hasRaw, hasAttachments, isFromMe, hasSent int
		if err := rows.Scan(
			&dm.ID, &dm.SourceID, &dm.SourceType, &dm.SourceIdentifier,
			&dm.SourceMessageID, &dm.Subject, &sentAt, &archivedAt,
			&hasRaw, &dm.PayloadBytes, &dm.AttachmentCount, &hasAttachments,
			&dm.LabelCount, &isFromMe, &hasSent,
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
		dm.HasAttachments = hasAttachments == 1
		dm.IsFromMe = isFromMe == 1
		dm.HasSentLabel = hasSent == 1
		msgs = append(msgs, dm)
	}
	return msgs, rows.Err()
}

// GetDuplicateGroupMessagesBatch is the batched form of
// GetDuplicateGroupMessages: it fetches every message row for many RFC822
// duplicate groups in a handful of chunked queries instead of one query per
// group (see kenn-io/msgvault#510 — 22,025 groups meant 22,025 unindexed
// queries). Returns a map keyed by RFC822 message ID; each value preserves
// the same per-group id-ascending order as GetDuplicateGroupMessages.
func (s *Store) GetDuplicateGroupMessagesBatch(
	rfc822IDs []string, sourceIDs ...int64,
) (map[string][]DuplicateMessageRow, error) {
	return s.GetDuplicateGroupMessagesBatchContext(
		context.Background(), rfc822IDs, sourceIDs...,
	)
}

// GetDuplicateGroupMessagesBatchContext is the request-aware form of
// GetDuplicateGroupMessagesBatch.
func (s *Store) GetDuplicateGroupMessagesBatchContext(
	ctx context.Context, rfc822IDs []string, sourceIDs ...int64,
) (map[string][]DuplicateMessageRow, error) {
	result := make(map[string][]DuplicateMessageRow)
	if len(rfc822IDs) == 0 {
		return result, nil
	}
	storageForms := make([]string, 0, 2*len(rfc822IDs))
	seenForms := make(map[string]struct{}, 2*len(rfc822IDs))
	groupByStorageForm := make(map[string]string, 2*len(rfc822IDs))
	for _, rfc822ID := range rfc822IDs {
		for _, form := range rfc822MessageIDStorageForms(rfc822ID) {
			if existingGroup, exists := groupByStorageForm[form]; exists && existingGroup != rfc822ID {
				return nil, fmt.Errorf(
					"%w: storage form %q maps to multiple duplicate groups %q and %q",
					ErrRFC822StorageFormCollision, form, existingGroup, rfc822ID,
				)
			}
			groupByStorageForm[form] = rfc822ID
			if _, exists := seenForms[form]; exists {
				continue
			}
			seenForms[form] = struct{}{}
			storageForms = append(storageForms, form)
		}
	}
	if len(storageForms) == 0 {
		return result, nil
	}

	const selectCols = "\n\t\tm.rfc822_message_id, " + duplicateGroupMessageColumns

	// queryInChunks binds prefixArgs BEFORE the chunked %s placeholder, so
	// the source_id filter (prefixArgs) must appear textually before the
	// rfc822_message_id IN (%s) clause below — the reverse of
	// GetDuplicateGroupMessages's clause order, which binds rfc822ID first
	// via a plain "=" and has no ordering constraint to satisfy.
	var prefixArgs []any
	sourceFilter := ""
	if len(sourceIDs) > 0 {
		placeholders := make([]string, len(sourceIDs))
		for i, id := range sourceIDs {
			placeholders[i] = "?"
			prefixArgs = append(prefixArgs, id)
		}
		sourceFilter = "m.source_id IN (" + strings.Join(placeholders, ",") + ") AND "
	}

	queryTemplate := `
		SELECT` + selectCols + `
		FROM messages m
		JOIN sources s ON s.id = m.source_id
		LEFT JOIN message_raw mr ON mr.message_id = m.id
		WHERE ` + sourceFilter + `m.rfc822_message_id IN (%s)
		  AND ` + LiveMessagesWhere("m", true) + `
		ORDER BY m.id`

	err := queryInChunksContext(ctx, s.db, storageForms, prefixArgs, queryTemplate,
		func(rows *loggedRows) error {
			var dm DuplicateMessageRow
			var rfc822ID string
			var sentAt, archivedAt sql.NullTime
			var hasRaw, hasAttachments, isFromMe, hasSent int
			if err := rows.Scan(
				&rfc822ID, &dm.ID, &dm.SourceID, &dm.SourceType, &dm.SourceIdentifier,
				&dm.SourceMessageID, &dm.Subject, &sentAt, &archivedAt,
				&hasRaw, &dm.PayloadBytes, &dm.AttachmentCount, &hasAttachments,
				&dm.LabelCount, &isFromMe, &hasSent,
				&dm.FromEmail,
			); err != nil {
				return err
			}
			if sentAt.Valid {
				dm.SentAt = sentAt.Time
			}
			if archivedAt.Valid {
				dm.ArchivedAt = archivedAt.Time
			}
			dm.HasRawMIME = hasRaw == 1
			dm.HasAttachments = hasAttachments == 1
			dm.IsFromMe = isFromMe == 1
			dm.HasSentLabel = hasSent == 1
			groupID, ok := groupByStorageForm[rfc822ID]
			if !ok {
				return fmt.Errorf("unexpected RFC822 Message-ID storage form %q", rfc822ID)
			}
			result[groupID] = append(result[groupID], dm)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("get duplicate group messages batch: %w", err)
	}
	for groupID := range result {
		sort.Slice(result[groupID], func(i, j int) bool {
			return result[groupID][i].ID < result[groupID][j].ID
		})
	}
	return result, nil
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
		       COALESCE(m.size_estimate, 0) AS payload_bytes,
		       COALESCE(m.attachment_count, 0) AS attachment_count,
		       CASE WHEN COALESCE(m.has_attachments, FALSE) THEN 1 ELSE 0 END AS has_attachments,
		       (SELECT COUNT(*) FROM message_labels ml
		          WHERE ml.message_id = m.id) AS label_count,
		       CASE WHEN COALESCE(m.is_from_me, FALSE) THEN 1 ELSE 0 END AS is_from_me,
		       CASE WHEN EXISTS (
		           SELECT 1 FROM message_labels ml2
		           JOIN labels l ON l.id = ml2.label_id
		           WHERE ml2.message_id = m.id
		             AND (l.source_label_id = 'SENT' OR UPPER(l.name) = 'SENT')
		       ) THEN 1 ELSE 0 END AS has_sent_label,
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
		var hasAttachments, isFromMe, hasSent int
		if err := rows.Scan(
			&c.ID, &c.SourceID, &c.SourceType, &c.SourceIdentifier,
			&c.SourceMessageID, &c.Subject, &sentAt, &archivedAt,
			&c.PayloadBytes, &c.AttachmentCount, &hasAttachments,
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
		c.HasAttachments = hasAttachments == 1
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
	return s.DeleteDedupedBatchContext(context.Background(), batchID)
}

// DeleteDedupedBatchContext is the request-aware form of DeleteDedupedBatch.
func (s *Store) DeleteDedupedBatchContext(
	ctx context.Context,
	batchID string,
) (int64, error) {
	return s.DeleteDedupedBatchesContext(ctx, []string{batchID})
}

// DeleteDedupedBatches permanently deletes all hidden rows associated with
// the selected dedup batches in one transaction.
func (s *Store) DeleteDedupedBatches(batchIDs []string) (int64, error) {
	return s.DeleteDedupedBatchesContext(context.Background(), batchIDs)
}

// DeleteDedupedBatchesContext is the request-aware form of
// DeleteDedupedBatches. The selected batches commit or roll back as one unit,
// so cancellation cannot leave only a prefix deleted.
func (s *Store) DeleteDedupedBatchesContext(
	ctx context.Context,
	batchIDs []string,
) (int64, error) {
	if len(batchIDs) == 0 {
		return 0, nil
	}

	// runMaintenance disables the pool-wide 30s statement_timeout for this
	// tx: the cascade DELETE is unbounded and exceeds 30s on a large archive
	// (finding S1). No-op timeout reset on SQLite.
	var deleted int64
	err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		for _, batchID := range batchIDs {
			result, err := tx.ExecContext(ctx, `
				DELETE FROM messages
				WHERE delete_batch_id = ? AND deleted_at IS NOT NULL
			`, batchID)
			if err != nil {
				return fmt.Errorf("delete dedup batch %q: %w", batchID, err)
			}
			affected, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("count deleted dedup batch %q: %w", batchID, err)
			}
			deleted += affected
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

func (s *Store) CountDedupedBatches(batchIDs []string) ([]DedupedBatchCount, int64, error) {
	return s.CountDedupedBatchesContext(context.Background(), batchIDs)
}

// CountDedupedBatchesContext is the request-aware form of
// CountDedupedBatches.
func (s *Store) CountDedupedBatchesContext(
	ctx context.Context,
	batchIDs []string,
) ([]DedupedBatchCount, int64, error) {
	stats := make([]DedupedBatchCount, 0, len(batchIDs))
	var total int64
	for _, id := range batchIDs {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		var count int64
		err := s.db.QueryRowContext(ctx,
			s.Rebind("SELECT COUNT(*) FROM messages WHERE delete_batch_id = ? AND deleted_at IS NOT NULL"),
			id,
		).Scan(&count)
		if err != nil {
			return nil, 0, fmt.Errorf("count rows for batch %q: %w", id, err)
		}
		total += count
		stats = append(stats, DedupedBatchCount{ID: id, Count: count})
	}
	return stats, total, nil
}

func (s *Store) CountAllDeduped() (total int64, distinctBatches int64, err error) {
	return s.CountAllDedupedContext(context.Background())
}

// CountAllDedupedContext is the request-aware form of CountAllDeduped.
func (s *Store) CountAllDedupedContext(
	ctx context.Context,
) (total int64, distinctBatches int64, err error) {
	if err := s.db.QueryRowContext(ctx,
		s.Rebind("SELECT COUNT(*) FROM messages WHERE deleted_at IS NOT NULL AND delete_batch_id IS NOT NULL"),
	).Scan(&total); err != nil {
		return 0, 0, fmt.Errorf("count hidden messages: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		s.Rebind("SELECT COUNT(DISTINCT delete_batch_id) FROM messages WHERE deleted_at IS NOT NULL AND delete_batch_id IS NOT NULL"),
	).Scan(&distinctBatches); err != nil {
		return 0, 0, fmt.Errorf("count distinct batches: %w", err)
	}
	return total, distinctBatches, nil
}

// DeleteAllDeduped permanently deletes every dedup-hidden row regardless of
// batch. Returns the number of rows deleted and the number of distinct
// batches affected.
//
// The delete is gated on the positive marker `delete_batch_id IS NOT NULL`
// in addition to `deleted_at IS NOT NULL` so that the contract is "permanently
// remove rows the dedup pipeline soft-hid." If a future feature ever adds
// another soft-delete semantics that writes deleted_at without a batch ID
// (e.g. a "trash" view, a per-message user hide), this command will leave
// those rows alone — they are not dedup-hidden and have no business being
// purged by the local dedup hard-delete rung.
//
// This is irreversible. Caller is responsible for backups.
// Attachments cascade-delete from the metadata row; on-disk blobs are
// content-addressed and survive until separate cleanup.
func (s *Store) DeleteAllDeduped() (deleted int64, distinctBatches int64, err error) {
	return s.DeleteAllDedupedContext(context.Background())
}

// DeleteAllDedupedContext is the request-aware form of DeleteAllDeduped.
func (s *Store) DeleteAllDedupedContext(
	ctx context.Context,
) (deleted int64, distinctBatches int64, err error) {
	// runMaintenance wraps the count + cascade DELETE in one transaction with
	// the pool-wide 30s statement_timeout disabled: the unbounded cascade
	// DELETE exceeds 30s on a large archive (finding S1). The count and the
	// delete share the tx so they observe the same snapshot, as before.
	// No-op timeout reset on SQLite.
	err = s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT delete_batch_id)
			FROM messages
			WHERE deleted_at IS NOT NULL AND delete_batch_id IS NOT NULL
		`).Scan(&distinctBatches); err != nil {
			return fmt.Errorf("delete all dedup-hidden: count batches: %w", err)
		}

		result, err := tx.ExecContext(ctx, `
			DELETE FROM messages
			WHERE deleted_at IS NOT NULL AND delete_batch_id IS NOT NULL
		`)
		if err != nil {
			return fmt.Errorf("delete all dedup-hidden: delete: %w", err)
		}
		deleted, err = result.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete all dedup-hidden: rows affected: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return deleted, distinctBatches, nil
}

func (s *Store) CountActiveMessages(sourceIDs ...int64) (int64, error) {
	return s.CountActiveMessagesContext(context.Background(), sourceIDs...)
}

// CountActiveMessagesContext is the request-aware form of
// CountActiveMessages.
func (s *Store) CountActiveMessagesContext(ctx context.Context, sourceIDs ...int64) (int64, error) {
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
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

// CountSourceDeletedMessages returns the count of archived messages that were
// deleted from their source account (retained in the archive). It is the exact
// complement of CountActiveMessages within the non-dedup-hidden population, so
// active + source-deleted is the canonical archived total.
func (s *Store) CountSourceDeletedMessages(sourceIDs ...int64) (int64, error) {
	return s.CountSourceDeletedMessagesContext(context.Background(), sourceIDs...)
}

// CountSourceDeletedMessagesContext is the request-aware form of
// CountSourceDeletedMessages.
func (s *Store) CountSourceDeletedMessagesContext(
	ctx context.Context,
	sourceIDs ...int64,
) (int64, error) {
	query := "SELECT COUNT(*) FROM messages WHERE " + SourceDeletedMessagesWhere("")
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
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&count)
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

func (s *Store) PlanRFC822IDBackfill(
	ctx context.Context, sourceIDs []int64,
) (RFC822IDBackfillPlan, error) {
	scopeClause, scopeArgs := rfc822IDBackfillSourceScope(sourceIDs)
	const batchSize = 1000
	lastID := int64(0)
	plan := RFC822IDBackfillPlan{}

	for {
		query := `SELECT m.id, m.source_id, mr.raw_data, mr.raw_format, mr.compression
			FROM messages m
			JOIN message_raw mr ON mr.message_id = m.id
			WHERE (m.rfc822_message_id IS NULL OR m.rfc822_message_id = '')
			  AND mr.raw_format = 'mime'
			  AND ` + LiveMessagesWhere("m", true) + `
			  AND m.id > ?` + scopeClause + `
			ORDER BY m.id
			LIMIT ?`
		args := append([]any{lastID}, scopeArgs...)
		args = append(args, batchSize)
		rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
		if err != nil {
			return RFC822IDBackfillPlan{}, fmt.Errorf("fetch RFC822 ID backfill batch: %w", err)
		}

		batchCount := 0
		for rows.Next() {
			var (
				item        RFC822IDBackfillItem
				rawData     []byte
				rawFormat   string
				compression sql.NullString
			)
			if err := rows.Scan(
				&item.MessageID, &item.SourceID, &rawData, &rawFormat, &compression,
			); err != nil {
				_ = rows.Close()
				return RFC822IDBackfillPlan{}, fmt.Errorf("scan RFC822 ID backfill candidate: %w", err)
			}
			batchCount++
			plan.Candidates++
			lastID = item.MessageID
			item.RawInputSHA256 = rfc822IDBackfillRawFingerprint(rawData, rawFormat, compression)

			raw, err := decodeMessageRaw(rawData, compression)
			if err != nil {
				plan.Failed++
				continue
			}
			parsed, err := mime.Parse(raw)
			if err != nil {
				plan.Failed++
				continue
			}
			item.RFC822MessageID = mime.NormalizeMessageID(parsed.MessageID)
			if item.RFC822MessageID == "" {
				plan.Failed++
				continue
			}
			plan.Items = append(plan.Items, item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return RFC822IDBackfillPlan{}, fmt.Errorf("iterate RFC822 ID backfill candidates: %w", err)
		}
		if err := rows.Close(); err != nil {
			return RFC822IDBackfillPlan{}, fmt.Errorf("close RFC822 ID backfill candidates: %w", err)
		}
		if batchCount == 0 {
			break
		}
	}

	sort.Slice(plan.Items, func(i, j int) bool {
		return plan.Items[i].MessageID < plan.Items[j].MessageID
	})
	return plan, nil
}

func rfc822IDBackfillSourceScope(sourceIDs []int64) (string, []any) {
	if len(sourceIDs) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(sourceIDs))
	args := make([]any, len(sourceIDs))
	for i, sourceID := range sourceIDs {
		placeholders[i] = "?"
		args[i] = sourceID
	}
	return " AND m.source_id IN (" + strings.Join(placeholders, ",") + ")", args
}

func (s *Store) ApplyRFC822IDBackfill(
	ctx context.Context,
	sourceIDs []int64,
	plan RFC822IDBackfillPlan,
	progress func(done, total int64),
) (updated int64, retErr error) {
	if plan.Candidates == 0 && len(plan.Items) == 0 {
		return 0, nil
	}
	items := append([]RFC822IDBackfillItem(nil), plan.Items...)
	sort.Slice(items, func(i, j int) bool {
		return items[i].MessageID < items[j].MessageID
	})
	scopeClause, scopeArgs := rfc822IDBackfillSourceScope(sourceIDs)

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire RFC822 ID backfill connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, s.dialect.BeginWriteSQL()); err != nil {
		return 0, fmt.Errorf("begin RFC822 ID backfill transaction: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), manualTransactionCleanupTimeout)
		defer cancel()
		if _, rollbackErr := conn.ExecContext(rollbackCtx, "ROLLBACK"); rollbackErr != nil && retErr == nil {
			retErr = fmt.Errorf("rollback RFC822 ID backfill transaction: %w", rollbackErr)
		}
	}()

	applied := int64(0)
	for _, item := range items {
		query := `SELECT m.id, m.source_id, m.rfc822_message_id,
			       mr.raw_data, mr.raw_format, mr.compression
			FROM messages m
			JOIN message_raw mr ON mr.message_id = m.id
			WHERE m.id = ?
			  AND ` + LiveMessagesWhere("m", true) + scopeClause +
			s.dialect.SelectForUpdate()
		args := append([]any{item.MessageID}, scopeArgs...)
		var (
			messageID      int64
			sourceID       int64
			storedRFC822ID sql.NullString
			rawData        []byte
			rawFormat      string
			compression    sql.NullString
		)
		err := conn.QueryRowContext(ctx, s.dialect.Rebind(query), args...).Scan(
			&messageID, &sourceID, &storedRFC822ID, &rawData, &rawFormat, &compression,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("lock RFC822 ID backfill message %d: %w", item.MessageID, err)
		}
		derivedID, ok := deriveRFC822MessageID(rawData, compression)
		if messageID != item.MessageID || sourceID != item.SourceID ||
			(storedRFC822ID.Valid && storedRFC822ID.String != "") ||
			!ok || derivedID != item.RFC822MessageID ||
			rfc822IDBackfillRawFingerprint(rawData, rawFormat, compression) != item.RawInputSHA256 {
			return 0, nil
		}

		result, err := conn.ExecContext(ctx, s.dialect.Rebind(
			`UPDATE messages SET rfc822_message_id = ?
			 WHERE id = ? AND source_id = ?
			   AND (rfc822_message_id IS NULL OR rfc822_message_id = '')
			   AND `+LiveMessagesWhere("", true)),
			item.RFC822MessageID, item.MessageID, item.SourceID)
		if err != nil {
			return 0, fmt.Errorf("update RFC822 ID backfill message %d: %w", item.MessageID, err)
		}
		matched, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("rows affected for RFC822 ID backfill message %d: %w", item.MessageID, err)
		}
		if matched != 1 {
			return 0, nil
		}
		applied++
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, fmt.Errorf("commit RFC822 ID backfill transaction: %w", err)
	}
	committed = true
	updated = applied
	if progress != nil {
		progress(plan.Candidates, plan.Candidates)
	}
	return updated, nil
}

func deriveRFC822MessageID(rawData []byte, compression sql.NullString) (string, bool) {
	raw, err := decodeMessageRaw(rawData, compression)
	if err != nil {
		return "", false
	}
	parsed, err := mime.Parse(raw)
	if err != nil {
		return "", false
	}
	normalizedID := mime.NormalizeMessageID(parsed.MessageID)
	return normalizedID, normalizedID != ""
}

func (s *Store) BackfillRFC822IDs(
	sourceIDs []int64,
	progress func(done, total int64),
) (updated int64, failed int64, err error) {
	plan, err := s.PlanRFC822IDBackfill(context.Background(), sourceIDs)
	if err != nil {
		return 0, 0, err
	}
	updated, err = s.ApplyRFC822IDBackfill(context.Background(), sourceIDs, plan, progress)
	return updated, plan.Failed, err
}
