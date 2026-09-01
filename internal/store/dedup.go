package store

import (
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

// ErrRFC822IDBackfillPlanChanged means the archive no longer matches the
// exact RFC822 Message-ID derivation plan the user confirmed.
var ErrRFC822IDBackfillPlanChanged = errors.New(
	"RFC822 Message-ID derivation plan changed; rerun deduplicate to review the current plan",
)

// rfc822CanonicalIndexName is the canonical Message-ID/source covering index
// used by duplicate discovery. SQLite's production query names it explicitly:
// without that choice the cost-based planner prefers idx_messages_source for a
// scoped query and rebuilds a temporary GROUP BY B-tree, defeating the
// expression index this path exists to use.
const rfc822CanonicalIndexName = "idx_messages_rfc822_message_id_canonical"

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

type rfc822IDBackfillItem struct {
	MessageID       int64
	SourceID        int64
	RFC822MessageID string
	RawInputSHA256  [sha256.Size]byte
}

type RFC822IDBackfillPlan struct {
	Candidates int64
	Ready      int64
	Failed     int64
	digest     string
}

func (p RFC822IDBackfillPlan) Digest() string {
	return p.digest
}

// rfc822IDBackfillDigest incrementally binds the exact executable derivation
// stream without retaining it. Items are added in message-ID order by both
// PlanRFC822IDBackfill and ApplyRFC822IDBackfill. Finalization binds the item
// count separately so empty, truncated, and extended streams cannot compare
// equal even though the count is not known when streaming begins.
type rfc822IDBackfillDigest struct {
	items hash.Hash
	count int64
}

func newRFC822IDBackfillDigest() *rfc822IDBackfillDigest {
	return &rfc822IDBackfillDigest{items: sha256.New()}
}

func (d *rfc822IDBackfillDigest) Add(item rfc822IDBackfillItem) {
	writeRFC822IDBackfillInt64(d.items, item.MessageID)
	writeRFC822IDBackfillInt64(d.items, item.SourceID)
	writeRFC822IDBackfillBytes(d.items, []byte(item.RFC822MessageID))
	writeRFC822IDBackfillBytes(d.items, item.RawInputSHA256[:])
	d.count++
}

func (d *rfc822IDBackfillDigest) Sum() string {
	const version = "msgvault-rfc822-id-backfill-plan-v2"
	digest := sha256.New()
	writeRFC822IDBackfillBytes(digest, []byte(version))
	writeRFC822IDBackfillInt64(digest, d.count)
	writeRFC822IDBackfillBytes(digest, d.items.Sum(nil))
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

// findDuplicatesByRFC822IDQuery builds the exact SQL statement and bind
// arguments FindDuplicatesByRFC822ID executes. It exists so the query-plan
// regression tests can EXPLAIN the production statement rather than a copy
// that would silently drift from it — the GROUP BY expression must match the
// idx_messages_rfc822_message_id_canonical expression index byte for byte.
func (s *Store) findDuplicatesByRFC822IDQuery(sourceIDs []int64) (string, []any) {
	canonicalID := s.dialect.RFC822CanonicalIDExpr("rfc822_message_id")
	from := "messages"
	if len(sourceIDs) > 0 && !s.IsPostgreSQL() {
		// SQLite otherwise estimates the equality lookup through
		// idx_messages_source as cheaper, then sorts every scoped row into a
		// temporary GROUP BY B-tree. The canonical/source index is ordered for
		// grouping and covers the source filter, so select it for the exact
		// production shape. PostgreSQL has no INDEXED BY syntax and retains its
		// cost-based choice.
		from += " INDEXED BY " + rfc822CanonicalIndexName
	}
	query := `
		SELECT ` + canonicalID + `, COUNT(*) AS cnt
		FROM ` + from + `
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
	return query, args
}

func (s *Store) FindDuplicatesByRFC822ID(sourceIDs ...int64) ([]DuplicateGroupKey, error) {
	query, args := s.findDuplicatesByRFC822IDQuery(sourceIDs)

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

const rfc822IDBackfillBatchSize = 1000

func (s *Store) rfc822IDBackfillBatch() int {
	if s.rfc822IDBackfillBatchSizeOverride > 0 {
		return s.rfc822IDBackfillBatchSizeOverride
	}
	return rfc822IDBackfillBatchSize
}

type rfc822IDBackfillBatch struct {
	items      []rfc822IDBackfillItem
	candidates int64
	failed     int64
	lastID     int64
}

func (s *Store) rfc822IDBackfillBatchQuery(
	sourceIDs []int64, lastID int64, lockRows bool,
) (string, []any) {
	scopeClause, scopeArgs := rfc822IDBackfillSourceScope(sourceIDs)
	query := `SELECT m.id, m.source_id, mr.raw_data, mr.raw_format, mr.compression
		FROM messages m
		JOIN message_raw mr ON mr.message_id = m.id
		WHERE (m.rfc822_message_id IS NULL OR m.rfc822_message_id = '')
		  AND mr.raw_format = 'mime'
		  AND ` + LiveMessagesWhere("m", true) + `
		  AND m.id > ?` + scopeClause + `
		ORDER BY m.id
		LIMIT ?`
	if lockRows {
		query += s.dialect.SelectForUpdate()
	}
	args := append([]any{lastID}, scopeArgs...)
	args = append(args, s.rfc822IDBackfillBatch())
	return s.dialect.Rebind(query), args
}

// readRFC822IDBackfillBatch derives at most one bounded plan or apply page and
// closes its rows before returning. rawData is retained only for the row
// currently being parsed; the returned slice contains compact derivation
// metadata, never MIME payloads.
func readRFC822IDBackfillBatch(rows rowsScanner) (rfc822IDBackfillBatch, error) {
	batch := rfc822IDBackfillBatch{}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			item        rfc822IDBackfillItem
			rawData     []byte
			rawFormat   string
			compression sql.NullString
		)
		if err := rows.Scan(
			&item.MessageID, &item.SourceID, &rawData, &rawFormat, &compression,
		); err != nil {
			return rfc822IDBackfillBatch{}, fmt.Errorf("scan RFC822 ID backfill candidate: %w", err)
		}
		batch.candidates++
		batch.lastID = item.MessageID
		item.RawInputSHA256 = rfc822IDBackfillRawFingerprint(rawData, rawFormat, compression)
		var ok bool
		item.RFC822MessageID, ok = deriveRFC822MessageID(rawData, compression)
		if !ok {
			batch.failed++
			continue
		}
		batch.items = append(batch.items, item)
	}
	if err := rows.Err(); err != nil {
		return rfc822IDBackfillBatch{}, fmt.Errorf("iterate RFC822 ID backfill candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return rfc822IDBackfillBatch{}, fmt.Errorf("close RFC822 ID backfill candidates: %w", err)
	}
	return batch, nil
}

func (s *Store) PlanRFC822IDBackfill(
	ctx context.Context, sourceIDs []int64,
) (RFC822IDBackfillPlan, error) {
	lastID := int64(0)
	plan := RFC822IDBackfillPlan{}
	digest := newRFC822IDBackfillDigest()

	for {
		query, args := s.rfc822IDBackfillBatchQuery(sourceIDs, lastID, false)
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return RFC822IDBackfillPlan{}, fmt.Errorf("fetch RFC822 ID backfill batch: %w", err)
		}
		batch, err := readRFC822IDBackfillBatch(rows)
		if err != nil {
			return RFC822IDBackfillPlan{}, err
		}
		plan.Candidates += batch.candidates
		plan.Failed += batch.failed
		for _, item := range batch.items {
			digest.Add(item)
			plan.Ready++
		}
		if batch.candidates == 0 {
			break
		}
		lastID = batch.lastID
	}

	plan.digest = digest.Sum()
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
	if plan.Ready == 0 {
		return 0, nil
	}
	if err := validateRFC822IDBackfillPlan(plan); err != nil {
		return 0, err
	}

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
		retErr = finishRFC822IDBackfillTransaction(ctx, conn, committed, retErr)
	}()

	applied, digest, err := s.applyRFC822IDBackfillRows(ctx, conn, sourceIDs)
	if err != nil {
		return 0, err
	}
	if err := validateAppliedRFC822IDBackfill(plan, digest); err != nil {
		return 0, err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, fmt.Errorf("commit RFC822 ID backfill transaction: %w", err)
	}
	committed = true
	updated = applied
	reportRFC822IDBackfillProgress(progress, plan.Candidates)
	return updated, nil
}

func finishRFC822IDBackfillTransaction(
	ctx context.Context, conn *sql.Conn, committed bool, retErr error,
) error {
	if committed {
		return retErr
	}
	rollbackCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), manualTransactionCleanupTimeout)
	defer cancel()
	if _, err := conn.ExecContext(rollbackCtx, "ROLLBACK"); err != nil && retErr == nil {
		return fmt.Errorf("rollback RFC822 ID backfill transaction: %w", err)
	}
	return retErr
}

func reportRFC822IDBackfillProgress(progress func(done, total int64), candidates int64) {
	if progress != nil {
		progress(candidates, candidates)
	}
}

func validateRFC822IDBackfillPlan(plan RFC822IDBackfillPlan) error {
	if plan.Ready < 0 || plan.Ready > plan.Candidates || plan.Digest() == "" {
		return errors.New("invalid RFC822 ID backfill plan")
	}
	return nil
}

func (s *Store) applyRFC822IDBackfillRows(
	ctx context.Context, conn *sql.Conn, sourceIDs []int64,
) (int64, *rfc822IDBackfillDigest, error) {
	applied := int64(0)
	lastID := int64(0)
	digest := newRFC822IDBackfillDigest()
	for {
		query, args := s.rfc822IDBackfillBatchQuery(sourceIDs, lastID, true)
		//nolint:rowserrcheck // The bounded-page reader owns rows and checks Err.
		rows, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			return 0, digest, fmt.Errorf("fetch RFC822 ID backfill apply batch: %w", err)
		}
		batch, err := readRFC822IDBackfillBatch(rows)
		if err != nil {
			return 0, digest, err
		}
		batchApplied, err := s.applyRFC822IDBackfillBatch(ctx, conn, batch, digest)
		if err != nil {
			return 0, digest, err
		}
		applied += batchApplied
		if batch.candidates == 0 {
			return applied, digest, nil
		}
		lastID = batch.lastID
	}
}

func (s *Store) applyRFC822IDBackfillBatch(
	ctx context.Context,
	conn *sql.Conn,
	batch rfc822IDBackfillBatch,
	digest *rfc822IDBackfillDigest,
) (int64, error) {
	for _, item := range batch.items {
		digest.Add(item)
		if err := s.updateRFC822IDBackfillItem(ctx, conn, item); err != nil {
			return 0, err
		}
	}
	return int64(len(batch.items)), nil
}

func (s *Store) updateRFC822IDBackfillItem(
	ctx context.Context, conn *sql.Conn, item rfc822IDBackfillItem,
) error {
	result, err := conn.ExecContext(ctx, s.dialect.Rebind(
		`UPDATE messages SET rfc822_message_id = ?
		 WHERE id = ? AND source_id = ?
		   AND (rfc822_message_id IS NULL OR rfc822_message_id = '')
		   AND `+LiveMessagesWhere("", true)),
		item.RFC822MessageID, item.MessageID, item.SourceID)
	if err != nil {
		return fmt.Errorf("update RFC822 ID backfill message %d: %w", item.MessageID, err)
	}
	matched, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for RFC822 ID backfill message %d: %w", item.MessageID, err)
	}
	if matched != 1 {
		return fmt.Errorf(
			"%w: message %d no longer matches the confirmed candidate",
			ErrRFC822IDBackfillPlanChanged, item.MessageID,
		)
	}
	return nil
}

func validateAppliedRFC822IDBackfill(
	plan RFC822IDBackfillPlan, digest *rfc822IDBackfillDigest,
) error {
	if digest.count == plan.Ready && digest.Sum() == plan.Digest() {
		return nil
	}
	return fmt.Errorf(
		"%w: confirmed %d derivations but found %d",
		ErrRFC822IDBackfillPlanChanged, plan.Ready, digest.count,
	)
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
