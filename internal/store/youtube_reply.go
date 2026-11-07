package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
)

const youtubeReplyReceiptPrefix = "youtube_reply:v1:"
const removedYouTubeReplyReceipt = `{"state":"removed"}`
const discardedYouTubeReplyReceipt = `{"state":"discarded"}`

type youtubeReplyEnvelope struct {
	SourceID int64  `json:"source_id"`
	Receipt  string `json:"receipt"`
}

func youtubeReplyReceiptKey(key string) (string, error) {
	decoded, err := hex.DecodeString(key)
	if err != nil || len(decoded) != 32 {
		return "", errors.New("invalid YouTube reply receipt key")
	}
	return youtubeReplyReceiptPrefix + hex.EncodeToString(decoded), nil
}

func encodeYouTubeReplyEnvelope(sourceID int64, receipt string) (string, error) {
	value, err := json.Marshal(youtubeReplyEnvelope{SourceID: sourceID, Receipt: receipt})
	if err != nil {
		return "", errors.New("encode YouTube reply envelope")
	}
	return string(value), nil
}

// GetYouTubeReplyReceiptContext returns the receipt inside its source-bound
// envelope, an opaque removed tombstone, or empty if no operation exists.
func (s *Store) GetYouTubeReplyReceiptContext(ctx context.Context, key string) (string, error) {
	key, err := youtubeReplyReceiptKey(key)
	if err != nil {
		return "", err
	}
	var value string
	err = s.db.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read YouTube reply receipt: %w", err)
	}
	var envelope youtubeReplyEnvelope
	if json.Unmarshal([]byte(value), &envelope) != nil || envelope.Receipt == "" {
		return "", errors.New("saved reply is invalid")
	}
	return envelope.Receipt, nil
}

// CompareAndSwapYouTubeReplyReceiptContext grants one state transition. It
// shares source removal's identity lock and verifies the source after locking,
// so stale callers cannot recreate private data after removal has committed.
func (s *Store) CompareAndSwapYouTubeReplyReceiptContext(ctx context.Context, sourceID int64, key, old, next string) (bool, error) {
	key, err := youtubeReplyReceiptKey(key)
	if err != nil {
		return false, err
	}
	if sourceID <= 0 || next == "" {
		return false, errors.New("reply receipt needs an existing source and nonempty value")
	}
	nextEnvelope, err := encodeYouTubeReplyEnvelope(sourceID, next)
	if err != nil {
		return false, err
	}
	oldEnvelope, err := encodeYouTubeReplyEnvelope(sourceID, old)
	if err != nil {
		return false, err
	}
	var won bool
	err = s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockIdentityMutationTxContext(ctx, tx) }, func(tx *loggedTx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
 SELECT 1 FROM sources
 WHERE id = ? AND source_type = 'youtube'
   AND NOT EXISTS (
     SELECT 1 FROM source_settings settings
     WHERE settings.source_id = sources.id
       AND settings.merged_into_source_id IS NOT NULL
   )
)`, sourceID).Scan(&exists); err != nil {
			return fmt.Errorf("check reply receipt source: %w", err)
		}
		if !exists {
			return ErrSourceNotFound
		}
		var result sql.Result
		var writeErr error
		if old == "" {
			result, writeErr = tx.ExecContext(ctx, `INSERT INTO archive_metadata (key,value) VALUES (?,?) ON CONFLICT(key) DO NOTHING`, key, nextEnvelope)
		} else {
			result, writeErr = tx.ExecContext(ctx, `UPDATE archive_metadata SET value = ? WHERE key = ? AND value = ?`, nextEnvelope, key, oldEnvelope)
		}
		if writeErr != nil {
			return fmt.Errorf("write YouTube reply receipt: %w", writeErr)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check YouTube reply transition: %w", err)
		}
		won = count == 1
		return nil
	})
	return won && err == nil, err
}

func (s *Store) prepareYouTubeReplyReceiptsForSourceMergeContext(ctx context.Context, tx *loggedTx, sourceID int64, discardPrepared bool) (bool, error) {
	rows, err := tx.QueryContext(ctx, s.dialect.Rebind(`SELECT key,value FROM archive_metadata
WHERE substr(key,1,?) = ?`), len(youtubeReplyReceiptPrefix), youtubeReplyReceiptPrefix)
	if err != nil {
		return false, fmt.Errorf("list YouTube reply receipts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type receiptUpdate struct {
		key      string
		oldValue string
		newValue string
	}
	var updates []receiptUpdate
	hasUnresolved := false
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return false, fmt.Errorf("read YouTube reply receipt: %w", err)
		}
		var envelope youtubeReplyEnvelope
		if err := json.Unmarshal([]byte(value), &envelope); err != nil || envelope.Receipt == "" {
			return false, errors.New("invalid saved reply; cannot merge sources")
		}
		if envelope.SourceID != sourceID {
			continue
		}
		var receipt struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal([]byte(envelope.Receipt), &receipt); err != nil || receipt.State == "" {
			return false, errors.New("invalid YouTube reply receipt prevents safe source merge")
		}
		switch receipt.State {
		case "prepared":
			if discardPrepared {
				newValue, err := encodeYouTubeReplyEnvelope(sourceID, discardedYouTubeReplyReceipt)
				if err != nil {
					return false, err
				}
				updates = append(updates, receiptUpdate{key: key, oldValue: value, newValue: newValue})
			}
		case "sending", "accepted":
			hasUnresolved = true
		case "archived", "rejected", "discarded", "removed":
		default:
			return false, fmt.Errorf("unknown YouTube reply receipt state %q prevents safe source merge", receipt.State)
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate YouTube reply receipts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("close YouTube reply receipts: %w", err)
	}
	if hasUnresolved {
		return true, nil
	}
	for _, update := range updates {
		result, err := tx.ExecContext(ctx, s.dialect.Rebind(`UPDATE archive_metadata SET value = ? WHERE key = ? AND value = ?`), update.newValue, update.key, update.oldValue)
		if err != nil {
			return false, fmt.Errorf("discard prepared YouTube reply before source merge: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("check prepared YouTube reply discard: %w", err)
		}
		if count != 1 {
			return false, errors.New("prepared YouTube reply changed during source merge")
		}
	}
	return hasUnresolved, nil
}

// Called inside either source-removal transaction, already holding the same
// identity-mutation lock as receipt creation/completion. Keep only the opaque
// global fingerprint: deleting it could reopen an uncertain send on reimport.
func redactYouTubeReplyReceiptsContext[Rows rowsScanner](ctx context.Context, s *Store, sourceID int64, query func(context.Context, string, ...any) (Rows, error), exec sourceIdentityExec) error {
	rows, err := query(ctx, s.dialect.Rebind(`SELECT key,value FROM archive_metadata WHERE substr(key,1,?) = ?`), len(youtubeReplyReceiptPrefix), youtubeReplyReceiptPrefix)
	if err != nil {
		return fmt.Errorf("list source reply receipts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return fmt.Errorf("read source reply receipt: %w", err)
		}
		var envelope youtubeReplyEnvelope
		if json.Unmarshal([]byte(value), &envelope) != nil || envelope.Receipt == "" {
			return errors.New("invalid saved reply; cannot remove source")
		}
		if envelope.SourceID == sourceID {
			keys = append(keys, key)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate source reply receipts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close source reply receipts: %w", err)
	}
	tombstone, err := encodeYouTubeReplyEnvelope(0, removedYouTubeReplyReceipt)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := exec(ctx, s.dialect.Rebind(`UPDATE archive_metadata SET value = ? WHERE key = ?`), tombstone, key); err != nil {
			return fmt.Errorf("redact source reply receipt: %w", err)
		}
	}
	return nil
}
