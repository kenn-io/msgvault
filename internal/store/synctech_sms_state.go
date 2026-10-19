package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

const (
	synctechSMSNativeStateBackfillMarker    = "synctech_sms.native_send_state_backfill_v1"
	synctechSMSNativeStateBackfillComplete  = "complete"
	SynctechSMSNativeStateBackfillBatchSize = 128
)

// SynctechSMSStateBackfillCandidate carries one compressed raw record. Callers
// inspect it synchronously so expanded attachment payloads are never retained
// across the batch.
type SynctechSMSStateBackfillCandidate struct {
	MessageID     int64
	MessageType   string
	CompressedRaw []byte
	Compression   sql.NullString
}

type SynctechSMSNativeStateBackfillProgress struct {
	LastMessageID int64
	Processed     int
	Unsent        int
	Complete      bool
}

// SynctechSMSNativeStateBackfillInspector reports whether the native record is
// sent. A false result corrects the legacy TRUE default.
type SynctechSMSNativeStateBackfillInspector func(candidate SynctechSMSStateBackfillCandidate) (sent bool, err error)

// SynctechSMSNativeStateBackfillProgressContext returns the last committed
// message ID and whether every eligible record has been inspected. A missing
// marker starts before the first message so ready-queue filtering holds all
// unverified imports until their batch is processed.
func (s *Store) SynctechSMSNativeStateBackfillProgressContext(ctx context.Context) (afterMessageID int64, complete bool, err error) {
	var marker string
	err = s.db.QueryRowContext(ctx, s.Rebind(`SELECT value FROM archive_metadata WHERE key = ?`), synctechSMSNativeStateBackfillMarker).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read Synctech SMS send-state backfill marker: %w", err)
	}
	if marker == synctechSMSNativeStateBackfillComplete {
		return 0, true, nil
	}
	afterMessageID, err = parseSynctechSMSNativeStateBackfillCursor(marker)
	if err != nil {
		return 0, false, fmt.Errorf("read Synctech SMS send-state backfill cursor: %w", err)
	}
	return afterMessageID, false, nil
}

// ProcessSynctechSMSNativeStateBackfillBatchContext inspects at most batchSize
// records after the durable cursor. Each message correction and the cursor
// advance commit together, so cancellation resumes after the last committed
// batch. The inspector sees one compressed raw record at a time.
func (s *Store) ProcessSynctechSMSNativeStateBackfillBatchContext(
	ctx context.Context,
	batchSize int,
	inspect SynctechSMSNativeStateBackfillInspector,
) (progress SynctechSMSNativeStateBackfillProgress, err error) {
	if err := ctx.Err(); err != nil {
		return progress, err
	}
	if batchSize < 1 {
		return progress, fmt.Errorf("%w: Synctech SMS backfill batch size must be positive", ErrInvalidLoop)
	}
	if inspect == nil {
		return progress, fmt.Errorf("%w: Synctech SMS backfill inspector is required", ErrInvalidLoop)
	}

	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES(?,'-1')
			ON CONFLICT(key) DO NOTHING`, synctechSMSNativeStateBackfillMarker); err != nil {
			return fmt.Errorf("initialize Synctech SMS send-state backfill cursor: %w", err)
		}
		cursorQuery := `SELECT value FROM archive_metadata WHERE key=?`
		if s.IsPostgreSQL() {
			cursorQuery += ` FOR UPDATE`
		}
		var marker string
		markerErr := tx.QueryRowContext(ctx, cursorQuery, synctechSMSNativeStateBackfillMarker).Scan(&marker)
		if markerErr != nil {
			return fmt.Errorf("read Synctech SMS send-state backfill cursor: %w", markerErr)
		}
		if marker == synctechSMSNativeStateBackfillComplete {
			progress.Complete = true
			return nil
		}
		cursor, err := parseSynctechSMSNativeStateBackfillCursor(marker)
		if err != nil {
			return fmt.Errorf("parse Synctech SMS send-state backfill cursor: %w", err)
		}

		rows, err := tx.QueryContext(ctx, `SELECT m.id,m.message_type
			FROM messages m JOIN sources src ON src.id=m.source_id
			WHERE m.id>? AND src.source_type='synctech_sms' AND m.message_type IN ('sms','mms')
			AND m.source_is_from_me=TRUE AND COALESCE(m.is_sent,TRUE)=TRUE
			AND EXISTS(SELECT 1 FROM message_raw r WHERE r.message_id=m.id AND r.raw_format='synctech_sms_xml_json')
			ORDER BY m.id LIMIT ?`, cursor, batchSize+1)
		if err != nil {
			return fmt.Errorf("query Synctech SMS send-state backfill batch: %w", err)
		}
		type candidateID struct {
			messageID   int64
			messageType string
		}
		ids := make([]candidateID, 0, batchSize+1)
		for rows.Next() {
			var id candidateID
			if err := rows.Scan(&id.messageID, &id.messageType); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan Synctech SMS send-state candidate id: %w", err)
			}
			ids = append(ids, id)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil {
			return fmt.Errorf("iterate Synctech SMS send-state candidate ids: %w", rowsErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close Synctech SMS send-state candidate ids: %w", closeErr)
		}
		hasMore := len(ids) > batchSize
		if hasMore {
			ids = ids[:batchSize]
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			candidate := SynctechSMSStateBackfillCandidate{MessageID: id.messageID, MessageType: id.messageType}
			err := tx.QueryRowContext(ctx, `SELECT raw_data,compression FROM message_raw
				WHERE message_id=? AND raw_format='synctech_sms_xml_json'`, id.messageID).Scan(&candidate.CompressedRaw, &candidate.Compression)
			if err != nil {
				return fmt.Errorf("load Synctech SMS send-state record for message %d: %w", id.messageID, err)
			}
			sent, err := inspect(candidate)
			if err != nil {
				return fmt.Errorf("inspect Synctech SMS send state for message %d: %w", id.messageID, err)
			}
			if !sent {
				if _, err := tx.ExecContext(ctx, `UPDATE messages SET is_sent=FALSE
					WHERE id=? AND source_is_from_me=TRUE AND COALESCE(is_sent,TRUE)=TRUE
					AND message_type IN ('sms','mms')
					AND source_id IN (SELECT id FROM sources WHERE source_type='synctech_sms')`, id.messageID); err != nil {
					return fmt.Errorf("persist Synctech SMS native unsent state for message %d: %w", id.messageID, err)
				}
				progress.Unsent++
			}
			progress.Processed++
			progress.LastMessageID = id.messageID
			candidate.CompressedRaw = nil
		}

		progress.Complete = !hasMore
		marker = strconv.FormatInt(cursor, 10)
		if len(ids) > 0 {
			marker = strconv.FormatInt(ids[len(ids)-1].messageID, 10)
			progress.LastMessageID = ids[len(ids)-1].messageID
		}
		if progress.Complete {
			marker = synctechSMSNativeStateBackfillComplete
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, synctechSMSNativeStateBackfillMarker, marker); err != nil {
			return fmt.Errorf("persist Synctech SMS send-state backfill cursor: %w", err)
		}
		return nil
	})
	if err != nil {
		return SynctechSMSNativeStateBackfillProgress{}, err
	}
	return progress, nil
}

func parseSynctechSMSNativeStateBackfillCursor(marker string) (int64, error) {
	cursor, err := strconv.ParseInt(marker, 10, 64)
	if err != nil || cursor < -1 {
		return 0, fmt.Errorf("invalid cursor %q", marker)
	}
	return cursor, nil
}
