package store

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// ValidateInboxTargetContext checks archive source/item/membership metadata
// only. The daemon additionally binds AccountID to the source configuration and
// authenticates that account against the provider before dispatch.
func (s *Store) ValidateInboxTargetContext(ctx context.Context, target inboxcontrol.Target) error {
	if err := target.Validate(); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error { return validateInboxArchiveTarget(ctx, tx, target) })
}

// ValidateInboxItemIdentityContext checks the live original item. A validated
// durable MOVE receipt may also follow that item after sync rekeys its provider
// ID to the mapped destination copy. This does not authorize a caller or grant
// mutation permission.
func (s *Store) ValidateInboxItemIdentityContext(ctx context.Context, target inboxcontrol.Target) error {
	if err := target.Validate(); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := validateInboxArchiveSourceIdentity(ctx, tx, target); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE id=? AND source_id=? AND source_message_id=? AND deleted_at IS NULL`, target.ItemID, target.SourceID, target.ProviderID).Scan(&count); err != nil {
			return err
		}
		if count == 1 {
			return nil
		}
		original, hasOriginal := inboxcontrol.ReceiptAuthorityTarget(ctx)
		mapped, hasMapping := inboxcontrol.ReceiptMoveTarget(ctx)
		_, hasReceipt := inboxcontrol.ReceiptReconcileID(ctx)
		if target.Scope != inboxcontrol.ScopeMessage || target.SourceType != sourceTypeIMAP || !hasOriginal || !hasMapping || !hasReceipt || (target != original && target != mapped) {
			return inboxcontrol.ErrDenied
		}
		if err := validateInboxMappedMoveIdentityTx(ctx, tx, original, mapped); err != nil {
			if errors.Is(err, inboxcontrol.ErrConflict) {
				return inboxcontrol.ErrDenied
			}
			return err
		}
		return nil
	})
}
