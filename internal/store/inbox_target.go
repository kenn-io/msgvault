package store

import (
	"context"
	"fmt"

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

// ValidateActiveInboxMessageTargetContext binds an active archived message to
// its exact current provider membership.
func (s *Store) ValidateActiveInboxMessageTargetContext(ctx context.Context, target inboxcontrol.Target) error {
	if err := target.Validate(); err != nil {
		return err
	}
	if target.Scope != inboxcontrol.ScopeMessage {
		return inboxcontrol.ErrInvalid
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := validateInboxArchiveTarget(ctx, tx, target); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
  WHERE id=? AND source_id=? AND source_message_id=? AND deleted_at IS NULL AND deleted_from_source_at IS NULL`,
			target.ItemID, target.SourceID, target.ProviderID).Scan(&count); err != nil {
			return fmt.Errorf("validate active Inbox message identity: %w", err)
		}
		if count != 1 {
			return inboxcontrol.ErrDenied
		}
		return nil
	})
}

// ValidateInboxItemIdentityContext checks the live original item without a
// membership lookup. Only a validated durable MOVE receipt may use this path
// in the daemon; it does not authorize a caller or grant mutation permission.
func (s *Store) ValidateInboxItemIdentityContext(ctx context.Context, target inboxcontrol.Target) error {
	if err := target.Validate(); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := validateInboxArchiveIdentity(ctx, tx, target); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE id=? AND source_id=? AND source_message_id=? AND deleted_at IS NULL`, target.ItemID, target.SourceID, target.ProviderID).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return inboxcontrol.ErrDenied
		}
		return nil
	})
}
