package store

import (
	"context"
	"errors"
	"fmt"
)

// AdoptMessageSourceIDContext atomically rekeys a message to a new composite
// source ID and reconciles its labels in one transaction. The rekey is
// guarded by the expected old source ID under the sync-source fence, so a
// lost race, a failing label step, or cancellation leaves the previous key,
// labels, and snapshot untouched and the adoption retryable — the old key is
// never consumed before every step of the adoption has committed.
//
// reconcileLabels false (deferred authoritative reconciliation) performs the
// guarded rekey alone and leaves labels unchanged: no label step is split
// into a second transaction. The later label finalizer stays an independent
// operation — atomicity here covers the rekey itself, not the rekey plus that
// finalizer. replaceLabels follows ReconcileMessageLabels semantics and only
// applies when reconcileLabels is true.
func (s *Store) AdoptMessageSourceIDContext(
	ctx context.Context,
	messageID int64,
	expectedSourceMessageID, newSourceMessageID string,
	reconcileLabels bool,
	labelIDs []int64,
	replaceLabels bool,
) (bool, error) {
	if newSourceMessageID == "" || expectedSourceMessageID == "" ||
		expectedSourceMessageID == newSourceMessageID {
		return false, errors.New(
			"adoption requires a changing non-empty source message ID")
	}
	var changed bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE messages SET source_message_id = ?
			WHERE id = ? AND source_message_id = ?
		`, newSourceMessageID, messageID, expectedSourceMessageID)
		if err != nil {
			return fmt.Errorf("rekey adopted message source ID: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("inspect adopted message rekey: %w", err)
		}
		if affected != 1 {
			return fmt.Errorf(
				"adoption identity guard mismatch: message %d no longer has source ID %q",
				messageID, expectedSourceMessageID)
		}
		changed = true
		if !reconcileLabels {
			return nil
		}
		labelsChanged, err := s.reconcileMessageLabelsTxContext(
			ctx, tx, messageID, labelIDs, replaceLabels)
		if err != nil {
			return fmt.Errorf("reconcile adopted message labels: %w", err)
		}
		changed = changed || labelsChanged
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}
