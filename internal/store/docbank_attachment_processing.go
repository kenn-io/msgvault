package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DocbankAttachmentProcessing is the saved processing request for a mirrored file.
type DocbankAttachmentProcessing struct {
	ContentHash string
	NodeID      int64
	VersionID   string
	Fingerprint string
	Outcome     string
	JobID       string
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
	var result []DocbankAttachmentProcessing
	for _, unasked := range []bool{false, true} {
		if len(result) >= n || unasked && cursor.unaskedDone {
			break
		}
		query := `SELECT d.content_hash, d.node_id, d.version_id, '', '', ''
 FROM docbank_attachment_deliveries d
 WHERE d.destination_key = ? AND d.state = 'delivered' AND d.version_id <> '' AND d.content_hash > ?
 AND NOT EXISTS (SELECT 1 FROM docbank_attachment_processing p
 WHERE p.destination_key = d.destination_key AND p.content_hash = d.content_hash
 AND p.fingerprint = ?)
 ORDER BY d.content_hash LIMIT ?`
		args := []any{dest, cursor.afterHash, fingerprint, n - len(result)}
		if !unasked {
			query = `SELECT d.content_hash, d.node_id, d.version_id, p.fingerprint, p.outcome, p.job_id
 FROM docbank_attachment_processing p JOIN docbank_attachment_deliveries d
 ON d.destination_key = p.destination_key AND d.content_hash = p.content_hash
 WHERE p.destination_key = ? AND p.fingerprint = ? AND p.next_action_at <= ?
 AND d.state = 'delivered' AND d.version_id <> ''
 ORDER BY p.next_action_at, p.content_hash LIMIT ?`
			args = []any{dest, fingerprint, s.timestampValue(time.Now().UTC()), n - len(result)}
		}
		rows, err := s.db.QueryContext(ctx, s.Rebind(query), args...)
		if err != nil {
			return nil, fmt.Errorf("read attachment processing requests: %w", err)
		}
		for rows.Next() {
			var d DocbankAttachmentProcessing
			if err := rows.Scan(&d.ContentHash, &d.NodeID, &d.VersionID, &d.Fingerprint, &d.Outcome, &d.JobID); err != nil {
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
 (destination_key, content_hash, fingerprint, outcome, job_id, next_action_at) VALUES (?, ?, ?, ?, ?, ?)
 ON CONFLICT (destination_key, content_hash) DO UPDATE SET
 fingerprint = excluded.fingerprint, outcome = excluded.outcome, job_id = excluded.job_id, next_action_at = excluded.next_action_at`),
		dest, d.ContentHash, d.Fingerprint, d.Outcome, d.JobID, next)
	return err
}

func (s *Store) DocbankAttachmentProcessingCounts(ctx context.Context, dest, profile string) (DocbankAttachmentProcessingSummary, error) {
	result := DocbankAttachmentProcessingSummary{Outcomes: map[string]int64{}}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT COALESCE(p.outcome, ''), COUNT(*)
 FROM docbank_attachment_deliveries d LEFT JOIN docbank_attachment_processing p
 ON p.destination_key = d.destination_key AND p.content_hash = d.content_hash
 AND p.fingerprint = (SELECT value FROM archive_metadata WHERE key = ?)
 WHERE d.destination_key = ? AND d.state = 'delivered' AND d.version_id <> '' GROUP BY COALESCE(p.outcome, '')`),
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
