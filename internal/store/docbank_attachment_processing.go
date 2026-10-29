package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

var docbankAttachmentProcessingRecoverableOutcomes = []string{"consent", "stale_authority", "pdf_password_required"}

// IsDocbankAttachmentProcessingRecoverableOutcome reports failures a backfill may retry once.
func IsDocbankAttachmentProcessingRecoverableOutcome(outcome string) bool {
	return slices.Contains(docbankAttachmentProcessingRecoverableOutcomes, outcome)
}

// DocbankAttachmentProcessing is the saved processing request for a mirrored file.
type DocbankAttachmentProcessing struct {
	ContentHash string
	NodeID      int64
	VersionID   string
	Fingerprint string
	Outcome     string
	JobID       string
	Generation  int64
	Retry       bool
}

// DocbankAttachmentProcessingSummary reports the current profile's local job outcomes.
type DocbankAttachmentProcessingSummary struct {
	Profile   string           `json:"profile"`
	Waiting   int64            `json:"waiting"`
	Requested int64            `json:"requested"`
	Extracted int64            `json:"extracted"`
	Outcomes  map[string]int64 `json:"outcomes"`
}

func (s *Store) SaveDocbankAttachmentProcessingFingerprint(ctx context.Context, dest, profile, fingerprint string) error {
	_, err := s.db.ExecContext(ctx, s.Rebind(`INSERT INTO archive_metadata (key, value) VALUES (?, ?)
 ON CONFLICT (key) DO UPDATE SET value = excluded.value`), "docbank-processing:"+dest+":"+profile, fingerprint)
	return err
}

// DocbankAttachmentProcessingCursor visits unasked files once, after polling due jobs.
// Use a fresh cursor for each pass and each profile fingerprint.
type DocbankAttachmentProcessingCursor struct {
	afterHash   string
	unaskedDone bool
}

func (s *Store) DocbankAttachmentsToProcess(ctx context.Context, dest, fingerprint string, cursor *DocbankAttachmentProcessingCursor, n int) ([]DocbankAttachmentProcessing, error) {
	visible, _ := s.documentSearchVisible(nil)
	processable := documentSearchDelivered + " AND " + visible
	var result []DocbankAttachmentProcessing
	for _, unasked := range []bool{false, true} {
		if len(result) >= n || unasked && cursor.unaskedDone {
			break
		}
		query := `SELECT d.content_hash, d.node_id, d.version_id, '', '', '',
 COALESCE(p.generation, 0), COALESCE(p.retry, FALSE)
 FROM docbank_attachment_deliveries d
 LEFT JOIN docbank_attachment_processing p
 ON p.destination_key = d.destination_key AND p.content_hash = d.content_hash
 WHERE d.destination_key = ? AND ` + processable + ` AND d.content_hash > ?
 AND (p.destination_key IS NULL OR p.fingerprint <> ?)
 ORDER BY d.content_hash LIMIT ?`
		args := []any{dest, cursor.afterHash, fingerprint, n - len(result)}
		if !unasked {
			query = `SELECT d.content_hash, d.node_id, d.version_id, p.fingerprint, p.outcome, p.job_id,
 p.generation, p.retry
 FROM docbank_attachment_processing p JOIN docbank_attachment_deliveries d
 ON d.destination_key = p.destination_key AND d.content_hash = p.content_hash
 WHERE p.destination_key = ? AND p.fingerprint = ? AND p.next_action_at <= ?
 AND ` + processable + `
 ORDER BY p.next_action_at, p.content_hash LIMIT ?`
			args = []any{dest, fingerprint, s.timestampValue(time.Now().UTC()), n - len(result)}
		}
		rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
		if err != nil {
			return nil, fmt.Errorf("read attachment processing requests: %w", err)
		}
		for rows.Next() {
			var d DocbankAttachmentProcessing
			if err := rows.Scan(&d.ContentHash, &d.NodeID, &d.VersionID, &d.Fingerprint, &d.Outcome, &d.JobID, &d.Generation, &d.Retry); err != nil {
				_ = rows.Close()
				return nil, err
			}
			result = append(result, d)
			if unasked {
				cursor.afterHash = d.ContentHash
			}
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return nil, err
		}
		if unasked {
			cursor.unaskedDone = len(result) < n
		}
	}
	return result, nil
}

func (s *Store) RecordDocbankAttachmentProcessing(ctx context.Context, dest string, d DocbankAttachmentProcessing, nextActionAt time.Time) error {
	var next any
	if !nextActionAt.IsZero() {
		next = s.timestampValue(nextActionAt)
	}
	_, err := s.db.ExecContext(ctx, s.Rebind(`INSERT INTO docbank_attachment_processing
 (destination_key, content_hash, fingerprint, outcome, job_id, next_action_at, generation, retry) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (destination_key, content_hash) DO UPDATE SET
 fingerprint = excluded.fingerprint, outcome = excluded.outcome, job_id = excluded.job_id,
 next_action_at = excluded.next_action_at, retry = excluded.retry
 WHERE docbank_attachment_processing.generation = excluded.generation`),
		dest, d.ContentHash, d.Fingerprint, d.Outcome, d.JobID, next, d.Generation, d.Retry)
	if err != nil {
		return fmt.Errorf("record attachment processing result: %w", err)
	}
	return nil
}

func (s *Store) DocbankAttachmentProcessingCounts(ctx context.Context, dest, profile string) (DocbankAttachmentProcessingSummary, error) {
	visible, _ := s.documentSearchVisible(nil)
	processable := documentSearchDelivered + " AND " + visible
	result := DocbankAttachmentProcessingSummary{Outcomes: map[string]int64{}}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT COALESCE(p.outcome, ''), COUNT(*)
 FROM docbank_attachment_deliveries d LEFT JOIN docbank_attachment_processing p
 ON p.destination_key = d.destination_key AND p.content_hash = d.content_hash
 AND p.fingerprint = (SELECT value FROM archive_metadata WHERE key = ?)
 WHERE d.destination_key = ? AND `+processable+` GROUP BY COALESCE(p.outcome, '')`),
		"docbank-processing:"+dest+":"+profile, dest)
	if err != nil {
		return result, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var outcome string
		var count int64
		if err := rows.Scan(&outcome, &count); err != nil {
			return result, err
		}
		switch outcome {
		case "":
			result.Waiting += count
		case "requested":
			result.Requested += count
		case "extracted":
			result.Extracted += count
		default:
			result.Outcomes[outcome] += count
		}
	}
	return result, rows.Err()
}
