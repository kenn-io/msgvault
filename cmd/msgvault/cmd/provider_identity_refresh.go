package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"
)

var fastmailIdentityInventoryFactory provideridentity.Factory = provideridentity.NewFastmailInventory

func newMessageSyncer(client gmail.API, st *store.Store, opts *msgsync.Options) *msgsync.Syncer {
	return withAutomaticProviderIdentityRefresh(msgsync.New(client, st, opts), st)
}

func withAutomaticProviderIdentityRefresh(syncer *msgsync.Syncer, st *store.Store) *msgsync.Syncer {
	return syncer.WithSuccessfulSyncHook(
		"provider identity refresh",
		func(ctx context.Context, source *store.Source, mailboxChanged bool) error {
			// A run that saw mailbox change always refreshes; a no-op run
			// refreshes only when one is owed, so frequent scheduled no-op
			// syncs do not each cost a provider round trip.
			refresh := provideridentity.AutoRefresh
			if !mailboxChanged {
				refresh = provideridentity.AutoRefreshIfDue
			}
			_, _, err := refresh(
				ctx,
				cfg,
				st,
				source.ID,
				fastmailIdentityInventoryFactory,
			)
			return err
		},
	)
}
