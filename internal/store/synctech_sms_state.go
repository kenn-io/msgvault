package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

const synctechSMSNativeStateBackfillMarker = "synctech_sms.native_send_state_backfill_v1"
const synctechSMSNativeStateBackfillComplete = "complete"

func (s *Store) SynctechSMSNativeStateBackfillProgressContext(ctx context.Context) (afterMessageID int64, complete bool, err error) {
	var value string
	err = s.db.QueryRowContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key = ?`), synctechSMSNativeStateBackfillMarker).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read Synctech SMS send-state backfill marker: %w", err)
	}
	if value == synctechSMSNativeStateBackfillComplete {
		return 0, true, nil
	}
	afterMessageID, err = strconv.ParseInt(value, 10, 64)
	if err != nil || afterMessageID < 0 {
		return 0, false, fmt.Errorf("invalid Synctech SMS send-state backfill cursor %q", value)
	}
	return afterMessageID, false, nil
}

// SynctechSMSNativeStateBackfillBatchContext streams one bounded page of
// owner-authored records through inspect. It releases each decompressed raw
// record before reading the next, so MMS part payloads never accumulate across
// the batch. The caller returns false from inspect's sent result for drafts or
// pending sends that need correction.
func (s *Store) SynctechSMSNativeStateBackfillBatchContext(
	ctx context.Context,
	afterMessageID int64,
	limit int,
	inspect func(messageType string, raw []byte) (sent, valid bool, err error),
) (unsentMessageIDs []int64, lastMessageID int64, processed int, err error) {
	if afterMessageID < 0 || limit < 1 || inspect == nil {
		return nil, afterMessageID, 0, fmt.Errorf("Synctech SMS send-state backfill requires a nonnegative cursor, positive limit, and inspector")
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT m.id, m.message_type, r.raw_data, r.compression
		FROM messages m JOIN sources src ON src.id = m.source_id
		JOIN message_raw r ON r.message_id = m.id
		WHERE src.source_type = 'synctech_sms' AND m.message_type IN ('sms','mms')
		AND m.source_is_from_me = TRUE AND COALESCE(m.is_sent, TRUE) = TRUE
		AND m.id > ?
		AND r.raw_format = 'synctech_sms_xml_json'
		ORDER BY m.id LIMIT ?`), afterMessageID, limit)
	if err != nil {
		return nil, afterMessageID, 0, fmt.Errorf("query Synctech SMS send-state backfill batch: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, afterMessageID, processed, err
		}
		var messageID int64
		var messageType string
		var compressed []byte
		var compression sql.NullString
		if err := rows.Scan(&messageID, &messageType, &compressed, &compression); err != nil {
			return nil, afterMessageID, processed, fmt.Errorf("scan Synctech SMS send-state candidate: %w", err)
		}
		raw, err := decodeMessageRaw(compressed, compression)
		if err != nil {
			return nil, afterMessageID, processed, fmt.Errorf("decode Synctech SMS raw record for message %d: %w", messageID, err)
		}
		sent, valid, err := inspect(messageType, raw)
		raw = nil
		compressed = nil
		if err != nil {
			return nil, afterMessageID, processed, fmt.Errorf("inspect Synctech SMS send state for message %d: %w", messageID, err)
		}
		if !valid {
			return nil, afterMessageID, processed, fmt.Errorf("stored Synctech message %d has no native send state", messageID)
		}
		if !sent {
			unsentMessageIDs = append(unsentMessageIDs, messageID)
		}
		lastMessageID = messageID
		processed++
	}
	if err := rows.Err(); err != nil {
		return nil, afterMessageID, processed, fmt.Errorf("iterate Synctech SMS send-state batch: %w", err)
	}
	return unsentMessageIDs, lastMessageID, processed, nil
}

// FinishSynctechSMSNativeStateBackfillBatch corrects old unsent owner messages
// and advances the durable cursor in one transaction. The is_sent trigger
// queues changed conversations before the deriver reads its pending work.
func (s *Store) FinishSynctechSMSNativeStateBackfillBatch(ctx context.Context, unsentMessageIDs []int64, afterMessageID int64, complete bool) error {
	if afterMessageID < 0 {
		return fmt.Errorf("Synctech SMS send-state backfill cursor must be nonnegative")
	}
	progress := strconv.FormatInt(afterMessageID, 10)
	if complete {
		progress = synctechSMSNativeStateBackfillComplete
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var current string
		err := tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key = ?`, synctechSMSNativeStateBackfillMarker).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read Synctech SMS send-state backfill cursor: %w", err)
		}
		if err == nil && current == synctechSMSNativeStateBackfillComplete {
			return nil
		}
		if err == nil {
			if currentID, parseErr := strconv.ParseInt(current, 10, 64); parseErr != nil || currentID < 0 {
				return fmt.Errorf("invalid Synctech SMS send-state backfill cursor %q", current)
			}
		}
		if err := execInChunksContext(ctx, tx, unsentMessageIDs, nil, `
			UPDATE messages SET is_sent = FALSE
			WHERE id IN (%s) AND COALESCE(is_sent, TRUE) = TRUE AND source_is_from_me = TRUE
			AND message_type IN ('sms','mms')
			AND source_id IN (SELECT id FROM sources WHERE source_type = 'synctech_sms')`); err != nil {
			return fmt.Errorf("persist Synctech SMS native unsent state: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = CASE
				WHEN archive_metadata.value = 'complete' THEN archive_metadata.value
				WHEN CAST(archive_metadata.value AS BIGINT) > CAST(? AS BIGINT) THEN archive_metadata.value
				ELSE excluded.value END`, synctechSMSNativeStateBackfillMarker, progress, strconv.FormatInt(afterMessageID, 10)); err != nil {
			return fmt.Errorf("record Synctech SMS send-state backfill progress: %w", err)
		}
		return nil
	})
}
