package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

var _ inboxcontrol.Ledger = (*Store)(nil)

// PrepareInboxReceipt claims a principal/source/key atomically. The winner is
// the only caller that may dispatch. A replay returns the original receipt;
// changed intent fails even if the earlier receipt is terminal.
func (s *Store) PrepareInboxReceipt(ctx context.Context, receipt inboxcontrol.Receipt) (*inboxcontrol.Receipt, bool, error) {
	if err := receipt.ValidatePrepared(); err != nil {
		return nil, false, err
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return nil, false, fmt.Errorf("encode inbox receipt: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO inbox_operation_receipts
		(id, principal_id, source_id, idempotency_key_hash, intent_hash, receipt_json, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'prepared', ?)
		ON CONFLICT (principal_id, source_id, idempotency_key_hash) DO NOTHING`,
		receipt.ID, receipt.PrincipalID, receipt.SourceID, inboxIdempotencyKeyHash(receipt.IdempotencyKey), receipt.IntentHash, string(encoded), receipt.CreatedAt.UnixNano())
	if err != nil {
		return nil, false, fmt.Errorf("claim inbox receipt: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("inspect inbox receipt claim: %w", err)
	}
	got, err := s.LookupInboxReceipt(ctx, receipt.PrincipalID, receipt.SourceID, receipt.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	if got == nil || got.IntentHash != receipt.IntentHash {
		return nil, false, inboxcontrol.ErrConflict
	}
	return got, rows == 1, nil
}

// LookupInboxReceipt is storage-only. The controller supplies a principal
// derived from authentication and checks current source/action permissions.
func (s *Store) LookupInboxReceipt(ctx context.Context, principal string, sourceID int64, key string) (*inboxcontrol.Receipt, error) {
	return scanInboxReceipt(s.db.QueryRowContext(ctx, `SELECT receipt_json, status, dispatched_at, finished_at, after_json, failure_code
		FROM inbox_operation_receipts WHERE principal_id = ? AND source_id = ? AND idempotency_key_hash = ?`, principal, sourceID, inboxIdempotencyKeyHash(key)))
}

// PostgreSQL text cannot store NUL. Hash the lookup key while JSON retains its
// original UTF-8 bytes, preserving the request contract on both databases.
func inboxIdempotencyKeyHash(key string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
}

func (s *Store) GetInboxReceipt(ctx context.Context, id string) (*inboxcontrol.Receipt, error) {
	return scanInboxReceipt(s.db.QueryRowContext(ctx, `SELECT receipt_json, status, dispatched_at, finished_at, after_json, failure_code
		FROM inbox_operation_receipts WHERE id = ?`, id))
}

func scanInboxReceipt(row *sql.Row) (*inboxcontrol.Receipt, error) {
	var encoded, status, failure string
	var dispatched, finished sql.NullInt64
	var after sql.NullString
	if err := row.Scan(&encoded, &status, &dispatched, &finished, &after, &failure); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil //nolint:nilnil // Absence permits the ledger to claim a new operation.
		}
		return nil, fmt.Errorf("read inbox receipt: %w", err)
	}
	var receipt inboxcontrol.Receipt
	if err := json.Unmarshal([]byte(encoded), &receipt); err != nil {
		return nil, fmt.Errorf("decode inbox receipt: %w", err)
	}
	receipt.Status = inboxcontrol.Status(status)
	receipt.FailureCode = failure
	if dispatched.Valid {
		value := time.Unix(0, dispatched.Int64).UTC()
		receipt.DispatchedAt = &value
	}
	if finished.Valid {
		value := time.Unix(0, finished.Int64).UTC()
		receipt.FinishedAt = &value
	}
	if after.Valid {
		receipt.After = new(inboxcontrol.State)
		if err := json.Unmarshal([]byte(after.String), receipt.After); err != nil {
			return nil, fmt.Errorf("decode inbox receipt observation: %w", err)
		}
	}
	return &receipt, nil
}

// MarkInboxDispatching commits evidence before any provider I/O. It never
// accepts dispatching or terminal receipts, so uncertain writes cannot replay.
func (s *Store) MarkInboxDispatching(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE inbox_operation_receipts SET status = 'dispatching', dispatched_at = ? WHERE id = ? AND status = 'prepared'`, time.Now().UTC().UnixNano(), id)
	return inboxReceiptTransition(result, err)
}

func inboxReceiptTransition(result sql.Result, err error) error {
	if err != nil {
		return fmt.Errorf("persist inbox receipt transition: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect inbox receipt transition: %w", err)
	}
	if rows != 1 {
		return inboxcontrol.ErrConflict
	}
	return nil
}

// RecordInboxReceiptMapping persists an authoritative IMAP MOVE destination
// while the receipt is dispatching, before a provider readback can block.
func (s *Store) RecordInboxReceiptMapping(ctx context.Context, id string, mapping inboxcontrol.State) error {
	if _, err := inboxcontrol.SemanticFingerprint(mapping); err != nil {
		return err
	}
	if mapping.Source != (inboxcontrol.SourceIdentity{}) || mapping.ObservedAt.IsZero() || mapping.Inbox != nil || mapping.Read != nil || mapping.MarkedUnread != nil || mapping.ProvisionedFolder != nil || len(mapping.Folders) != 0 || len(mapping.Tags) != 0 || len(mapping.Flags) != 0 || mapping.Location != "" || mapping.Revision != "" || mapping.LastMessageID != "" {
		return fmt.Errorf("%w: receipt mapping must contain only an item target", inboxcontrol.ErrInvalid)
	}
	receipt, err := s.GetInboxReceipt(ctx, id)
	if err != nil {
		return err
	}
	if receipt == nil || receipt.Status != inboxcontrol.StatusDispatching {
		return inboxcontrol.ErrConflict
	}
	before := receipt.Before.Target
	if before.SourceType != sourceTypeIMAP || (receipt.Intent.Operation != inboxcontrol.OpMove && receipt.Intent.Operation != inboxcontrol.OpArchive && receipt.Intent.Operation != inboxcontrol.OpUnarchive) {
		return fmt.Errorf("%w: receipt does not describe an IMAP move", inboxcontrol.ErrInvalid)
	}
	target := mapping.Target
	target.Mailbox, target.UIDValidity, target.UID = before.Mailbox, before.UIDValidity, before.UID
	if target != before {
		return fmt.Errorf("%w: receipt mapping differs from the original item", inboxcontrol.ErrInvalid)
	}
	encoded, err := json.Marshal(mapping)
	if err != nil {
		return fmt.Errorf("encode inbox receipt mapping: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE inbox_operation_receipts SET after_json = ? WHERE id = ? AND status = 'dispatching' AND after_json IS NULL`, string(encoded), id)
	return inboxReceiptTransition(result, err)
}

// FinishInboxReceipt preserves immutable dispatch evidence. Reconciliation may
// resolve uncertain/partial outcomes, but cannot overwrite a verified receipt.
func (s *Store) FinishInboxReceipt(ctx context.Context, id string, status inboxcontrol.Status, after *inboxcontrol.State, code string) error {
	switch status {
	case inboxcontrol.StatusVerified, inboxcontrol.StatusPartial, inboxcontrol.StatusUnknown, inboxcontrol.StatusReconcileOnly, inboxcontrol.StatusFailed:
	default:
		return fmt.Errorf("%w: invalid final receipt status", inboxcontrol.ErrInvalid)
	}
	if status == inboxcontrol.StatusVerified && after == nil {
		return fmt.Errorf("%w: verified receipt requires observation", inboxcontrol.ErrInvalid)
	}
	var encoded any
	if after != nil {
		if _, err := inboxcontrol.SemanticFingerprint(*after); err != nil {
			return err
		}
		receipt, err := s.GetInboxReceipt(ctx, id)
		if err != nil {
			return err
		}
		if receipt == nil {
			return inboxcontrol.ErrConflict
		}
		target := after.Target
		before := receipt.Before.Target
		if before.SourceType == sourceTypeIMAP && (receipt.Intent.Operation == inboxcontrol.OpMove || receipt.Intent.Operation == inboxcontrol.OpArchive || receipt.Intent.Operation == inboxcontrol.OpUnarchive) {
			target.Mailbox, target.UIDValidity, target.UID = before.Mailbox, before.UIDValidity, before.UID
		}
		if target != before || after.Source != receipt.Before.Source || after.ObservedAt.IsZero() {
			return fmt.Errorf("%w: final observation differs from receipt identity", inboxcontrol.ErrInvalid)
		}
		data, err := json.Marshal(after)
		if err != nil {
			return fmt.Errorf("encode inbox final observation: %w", err)
		}
		encoded = string(data)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE inbox_operation_receipts SET status = ?, finished_at = ?, after_json = ?, failure_code = ?
		WHERE id = ? AND (status IN ('dispatching', 'unknown', 'partial', 'reconcile-only') OR (status = 'prepared' AND ? = 'failed'))`,
		status, time.Now().UTC().UnixNano(), encoded, code, id, status)
	return inboxReceiptTransition(result, err)
}

// RecoverInboxReceipts runs at startup and recovers only sources whose
// execution lease can be acquired. Active workers keep their receipts intact.
// No uncertain provider operation is retried or erased by recovery.
func (s *Store) RecoverInboxReceipts(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT source_id FROM inbox_operation_receipts WHERE status IN ('prepared', 'dispatching') GROUP BY source_id`)
	if err != nil {
		return fmt.Errorf("list interrupted inbox sources: %w", err)
	}
	var sourceIDs []int64
	for rows.Next() {
		var sourceID int64
		if err := rows.Scan(&sourceID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read interrupted inbox source: %w", err)
		}
		sourceIDs = append(sourceIDs, sourceID)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return fmt.Errorf("read interrupted inbox sources: %w", err)
	}
	for _, sourceID := range sourceIDs {
		lock, err := s.acquireSyncExecutionLock(ctx, sourceID)
		if errors.Is(err, ErrSyncAlreadyActive) {
			continue
		}
		if err != nil {
			return fmt.Errorf("acquire inbox recovery ownership: %w", err)
		}
		// Receipts outlive source deletion. Keep execution ownership while
		// recovering them even when there is no source row left to lock.
		recoveryErr := s.recoverAbandonedSyncSource(ctx, sourceID)
		if recoveryErr == nil || errors.Is(recoveryErr, sql.ErrNoRows) {
			recoveryErr = s.recoverInboxSourceReceipts(ctx, sourceID)
		}
		err = errors.Join(recoveryErr, s.abandonSyncExecutionLock(sourceID, lock))
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) recoverInboxSourceReceipts(ctx context.Context, sourceID int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		for _, transition := range []struct{ from, to, code string }{
			{"prepared", "failed", "interrupted-before-dispatch"},
			{"dispatching", "unknown", "interrupted-after-dispatch"},
		} {
			if _, err := tx.ExecContext(ctx, `UPDATE inbox_operation_receipts SET status = ?, finished_at = ?, failure_code = ? WHERE source_id = ? AND status = ?`, transition.to, time.Now().UTC().UnixNano(), transition.code, sourceID, transition.from); err != nil {
				return fmt.Errorf("recover inbox receipts: %w", err)
			}
		}
		return nil
	})
}
