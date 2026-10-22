package store

import (
	"context"
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

func (s *Store) SaveDocbankAttachmentProcessingFingerprint(ctx context.Context, dest, fingerprint string) error {
	_, err := s.db.ExecContext(ctx, s.Rebind(`INSERT INTO archive_metadata (key, value) VALUES (?, ?)
 ON CONFLICT (key) DO UPDATE SET value = excluded.value`), "docbank-processing:"+dest, fingerprint)
	return err
}

func (s *Store) DocbankAttachmentsToProcess(ctx context.Context, dest, fingerprint string, n int) ([]DocbankAttachmentProcessing, error) {
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT d.content_hash, d.node_id, d.version_id,
 COALESCE(p.fingerprint, ''), COALESCE(p.outcome, ''), COALESCE(p.job_id, '')
 FROM docbank_attachment_deliveries d LEFT JOIN docbank_attachment_processing p
 ON p.destination_key = d.destination_key AND p.content_hash = d.content_hash
 AND p.version_id = d.version_id AND p.fingerprint = ?
 WHERE d.destination_key = ? AND d.state = 'delivered' AND d.version_id <> ''
 AND (p.content_hash IS NULL OR p.next_action_at <= ?)
 ORDER BY CASE WHEN p.content_hash IS NULL THEN 0 ELSE 1 END, p.next_action_at, d.content_hash LIMIT ?`),
		fingerprint, dest, s.timestampValue(time.Now().UTC()), n)
	if err != nil {
		return nil, fmt.Errorf("read attachment processing requests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []DocbankAttachmentProcessing
	for rows.Next() {
		var d DocbankAttachmentProcessing
		if err := rows.Scan(&d.ContentHash, &d.NodeID, &d.VersionID, &d.Fingerprint, &d.Outcome, &d.JobID); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

func (s *Store) RecordDocbankAttachmentProcessing(ctx context.Context, dest string, d DocbankAttachmentProcessing, nextActionAt time.Time) error {
	var next any
	if !nextActionAt.IsZero() {
		next = s.timestampValue(nextActionAt)
	}
	_, err := s.db.ExecContext(ctx, s.Rebind(`INSERT INTO docbank_attachment_processing
 (destination_key, content_hash, version_id, fingerprint, outcome, job_id, next_action_at) VALUES (?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (destination_key, content_hash) DO UPDATE SET version_id = excluded.version_id,
 fingerprint = excluded.fingerprint, outcome = excluded.outcome, job_id = excluded.job_id, next_action_at = excluded.next_action_at`),
		dest, d.ContentHash, d.VersionID, d.Fingerprint, d.Outcome, d.JobID, next)
	return err
}

func (s *Store) DocbankAttachmentProcessingCounts(ctx context.Context, dest string) (DocbankAttachmentProcessingSummary, error) {
	result := DocbankAttachmentProcessingSummary{Outcomes: map[string]int64{}}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT COALESCE(p.outcome, ''), COUNT(*)
 FROM docbank_attachment_deliveries d LEFT JOIN docbank_attachment_processing p
 ON p.destination_key = d.destination_key AND p.content_hash = d.content_hash AND p.version_id = d.version_id
 AND p.fingerprint = (SELECT value FROM archive_metadata WHERE key = ?)
 WHERE d.destination_key = ? AND d.state = 'delivered' AND d.version_id <> '' GROUP BY COALESCE(p.outcome, '')`),
		"docbank-processing:"+dest, dest)
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
