package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const synctechSMSNativeStateBackfillMarker = "synctech_sms.native_send_state_backfill_v1"

// SynctechSMSStateBackfillCandidate is a stored owner-authored SMS/MMS message
// whose legacy is_sent default may disagree with its archived native record.
type SynctechSMSStateBackfillCandidate struct {
	MessageID   int64
	MessageType string
	Raw         []byte
}

func (s *Store) SynctechSMSNativeStateBackfillNeededContext(ctx context.Context) (bool, error) {
	var complete string
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key = ?`), synctechSMSNativeStateBackfillMarker).Scan(&complete)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read Synctech SMS send-state backfill marker: %w", err)
	}
	return false, nil
}

// SynctechSMSNativeStateBackfillCandidatesContext returns owner-authored text
// records that still have the old default TRUE send state. Native raw records
// are the authority for distinguishing sent messages from drafts and pending
// sends.
func (s *Store) SynctechSMSNativeStateBackfillCandidatesContext(ctx context.Context) ([]SynctechSMSStateBackfillCandidate, error) {
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT m.id, m.message_type, r.raw_data, r.compression
		FROM messages m JOIN sources src ON src.id = m.source_id
		JOIN message_raw r ON r.message_id = m.id
		WHERE src.source_type = 'synctech_sms' AND m.message_type IN ('sms','mms')
		AND m.source_is_from_me = TRUE AND COALESCE(m.is_sent, TRUE) = TRUE
		AND r.raw_format = 'synctech_sms_xml_json'
		ORDER BY m.id`))
	if err != nil {
		return nil, fmt.Errorf("query Synctech SMS send-state backfill candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var candidates []SynctechSMSStateBackfillCandidate
	for rows.Next() {
		var candidate SynctechSMSStateBackfillCandidate
		var compressed []byte
		var compression sql.NullString
		if err := rows.Scan(&candidate.MessageID, &candidate.MessageType, &compressed, &compression); err != nil {
			return nil, fmt.Errorf("scan Synctech SMS send-state candidate: %w", err)
		}
		candidate.Raw, err = decodeMessageRaw(compressed, compression)
		if err != nil {
			return nil, fmt.Errorf("decode Synctech SMS raw record for message %d: %w", candidate.MessageID, err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Synctech SMS send-state candidates: %w", err)
	}
	return candidates, nil
}

// FinishSynctechSMSNativeStateBackfill corrects old unsent owner messages and
// records completion in the same transaction. The is_sent trigger queues every
// changed conversation before the deriver reads its pending work.
func (s *Store) FinishSynctechSMSNativeStateBackfill(ctx context.Context, unsentMessageIDs []int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var complete string
		err := tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key = ?`, synctechSMSNativeStateBackfillMarker).Scan(&complete)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock Synctech SMS send-state backfill marker: %w", err)
		}
		if err := execInChunksContext(ctx, tx, unsentMessageIDs, nil, `
			UPDATE messages SET is_sent = FALSE
			WHERE id IN (%s) AND COALESCE(is_sent, TRUE) = TRUE AND source_is_from_me = TRUE
			AND message_type IN ('sms','mms')
			AND source_id IN (SELECT id FROM sources WHERE source_type = 'synctech_sms')`); err != nil {
			return fmt.Errorf("persist Synctech SMS native unsent state: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key, value) VALUES (?, 'complete')
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, synctechSMSNativeStateBackfillMarker); err != nil {
			return fmt.Errorf("record Synctech SMS send-state backfill completion: %w", err)
		}
		return nil
	})
}
