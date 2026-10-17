package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/mime"
)

// LoopSignalsMaxRawBytes caps both stored input and zlib expansion. Backfill
// accepts only zlib MIME; unsupported/encrypted/missing raw leaves evidence
// absent and is counted without exposing personal content.
const LoopSignalsMaxRawBytes = 8 << 20
const loopSignalsStartedKey = "loops_signals_backfill_started"

var errLoopSignalsTooLarge = errors.New("loop signals raw exceeds decode limit")

type LoopSignalsBackfillResult struct {
	Scanned, Parsed, Missing, Encrypted, Unsupported, TooLarge, Invalid int
	MaxStoredBytesRead, MaxDecodedBytesRead                             int64
}

// BackfillLoopSignalsContext commits a bounded newest-ID-first slice, its
// invalidations, and cursor together. The metadata marker distinguishes an
// initial cursor zero from a previously consumed legal message ID zero.
func (s *Store) BackfillLoopSignalsContext(ctx context.Context, limit int, now time.Time) (LoopSignalsBackfillResult, error) {
	if limit < 1 {
		return LoopSignalsBackfillResult{}, fmt.Errorf("%w: backfill limit must be positive", ErrInvalidLoop)
	}
	result := LoopSignalsBackfillResult{}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := lockLoopState(ctx, tx); err != nil {
			return err
		}
		state, err := readLoopState(ctx, tx)
		if err != nil {
			return err
		}
		var marker string
		err = tx.QueryRowContext(ctx, "SELECT value FROM archive_metadata WHERE key=?", loopSignalsStartedKey).Scan(&marker)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		version := strconv.Itoa(mime.AutomationParserVersion)
		started := marker == version
		if started && state.SignalsBackfillComplete {
			return nil
		}
		query := `SELECT m.id FROM messages m WHERE NOT EXISTS(SELECT 1 FROM message_automation_signals a WHERE a.message_id=m.id AND a.parser_version>=?)`
		args := []any{mime.AutomationParserVersion}
		if started {
			query += " AND m.id<?"
			args = append(args, state.SignalsBackfillLastMessageID)
		}
		query += " ORDER BY m.id DESC LIMIT ?"
		args = append(args, limit+1)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		ids := []int64{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		complete := len(ids) <= limit
		if !complete {
			ids = ids[:limit]
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.lockMeetingEvidenceWith(ctx, tx, id); err != nil {
				return err
			}
			if err := s.backfillLoopSignal(ctx, tx, id, now, &result); err != nil {
				return err
			}
			result.Scanned++
		}
		last := state.SignalsBackfillLastMessageID
		if len(ids) > 0 {
			last = ids[len(ids)-1]
		}
		// Write the started marker after evidence, so any commit failure rolls both
		// back. last_run_at is deliberately irrelevant: earlier phases can fail.
		_, err = tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, loopSignalsStartedKey, version)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE loop_derivation_state SET signals_backfill_last_message_id=?,signals_backfill_complete=?,updated_at=? WHERE singleton=1`, last, complete, s.dialect.TimestampParam(now))
		return err
	})
	if err != nil {
		return LoopSignalsBackfillResult{}, fmt.Errorf("backfill loop automation signals: %w", err)
	}
	return result, nil
}
func (s *Store) backfillLoopSignal(ctx context.Context, tx *loggedTx, id int64, now time.Time, result *LoopSignalsBackfillResult) error {
	var raw []byte
	var compression, format sql.NullString
	var encrypted, rawLength int64
	// substr is evaluated by both native engines before the BLOB crosses into
	// Go; encrypted and unsupported records do not read even a raw prefix.
	err := tx.QueryRowContext(ctx, `SELECT CASE WHEN raw_format='mime' AND COALESCE(encryption_version,0)=0 AND compression='zlib' THEN substr(raw_data,1,?) ELSE NULL END,length(raw_data),raw_format,compression,COALESCE(encryption_version,0) FROM message_raw WHERE message_id=?`, LoopSignalsMaxRawBytes+1, id).Scan(&raw, &rawLength, &format, &compression, &encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		result.Missing++
		return nil
	}
	if err != nil {
		return err
	}
	result.MaxStoredBytesRead = max(result.MaxStoredBytesRead, int64(len(raw)))
	if encrypted > 0 {
		result.Encrypted++
		return nil
	}
	if format.String != "mime" || compression.String != "zlib" {
		result.Unsupported++
		return nil
	}
	if rawLength > LoopSignalsMaxRawBytes {
		result.TooLarge++
		return nil
	}
	decoded, read, err := decodeMessageRawBounded(raw, compression, LoopSignalsMaxRawBytes, errLoopSignalsTooLarge)
	result.MaxDecodedBytesRead = max(result.MaxDecodedBytesRead, read)
	if errors.Is(err, errLoopSignalsTooLarge) {
		result.TooLarge++
		return nil
	}
	if err != nil {
		result.Invalid++
		return nil
	}
	parsed, err := mime.Parse(decoded)
	if err != nil {
		result.Invalid++
		return nil
	}
	if err := persistMessageAutomationSignalsWith(boundQuerier{ctx: ctx, q: tx}, id, AutomationSignalsFromMIME(parsed.AutomationSignals())); err != nil {
		return err
	}
	var conversation sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT conversation_id FROM messages WHERE id=?", id).Scan(&conversation); err != nil {
		return err
	}
	if conversation.Valid {
		if err := s.enqueueLoopTx(ctx, tx, []int64{conversation.Int64}, "signals", "", now); err != nil {
			return err
		}
	}
	result.Parsed++
	return nil
}
