package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/store"
)

// ReconcileCardDAVParticipantBindingsContext keeps the daemon's owner-only
// CardDAV operation on the same Store as synchronization and person identities.
func (a *storeAPIAdapter) ReconcileCardDAVParticipantBindingsContext(ctx context.Context, options store.CardDAVParticipantBindingOptions) (*store.CardDAVParticipantBindingResult, error) {
	return a.store.ReconcileCardDAVParticipantBindingsContext(ctx, options)
}
