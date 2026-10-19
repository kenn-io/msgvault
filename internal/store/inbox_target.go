package store

import (
	"context"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// ValidateInboxTargetContext checks the live archive source/item and exact
// IMAP membership. The daemon additionally binds AccountID to the source
// configuration and authenticates that account against the provider.
func (s *Store) ValidateInboxTargetContext(ctx context.Context, target inboxcontrol.Target) error {
	if err := target.Validate(); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error { return validateInboxArchiveTarget(ctx, tx, target) })
}

// ValidateInboxItemIdentityContext checks the retained original item identity
// without a membership lookup. Only a validated durable receipt may use this
// path in the daemon; it does not authorize a caller or grant mutation
// permission.
func (s *Store) ValidateInboxItemIdentityContext(ctx context.Context, target inboxcontrol.Target) error {
	if err := target.Validate(); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		return validateInboxArchiveIdentity(ctx, tx, target)
	})
}
