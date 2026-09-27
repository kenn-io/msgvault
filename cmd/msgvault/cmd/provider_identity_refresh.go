package cmd

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"
)

var fastmailIdentityInventoryFactory provideridentity.Factory = provideridentity.NewFastmailInventory

func newMessageSyncer(client gmail.API, st *store.Store, opts *msgsync.Options, state *invocation) *msgsync.Syncer {
	if opts == nil {
		opts = msgsync.DefaultOptions()
	}
	configured := *opts
	configured.RemoteImages = configuredRemoteImageFetcher(state.cfg)
	return withAutomaticProviderIdentityRefresh(msgsync.New(client, st, &configured), st, state)
}

func withAutomaticProviderIdentityRefresh(syncer *msgsync.Syncer, st *store.Store, state *invocation) *msgsync.Syncer {
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
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			_, _, err := refresh(
				ctx,
				state.cfg,
				st,
				source.ID,
				fastmailIdentityInventoryFactory,
			)
			return err
		},
	)
}
