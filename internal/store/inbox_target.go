package store

import (
	"context"

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

// ValidateInboxItemIdentityContext binds the exact archived source and item.
// Receipt recovery may read a proved remote UID before its membership is local;
// the daemon separately binds that UID to authenticated durable MOVE evidence.
func (s *Store) ValidateInboxItemIdentityContext(ctx context.Context, target inboxcontrol.Target) error {
	if err := target.Validate(); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error { return validateInboxArchiveIdentity(ctx, tx, target) })
}
