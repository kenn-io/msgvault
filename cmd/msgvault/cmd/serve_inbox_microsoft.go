package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/msmail"
	"go.kenn.io/msgvault/internal/store"
)

func (a *storeAPIAdapter) resolveMicrosoftInboxProvider(ctx context.Context, source *store.Source, binding inboxcontrol.SourceIdentity, request inboxcontrol.Request) (inboxcontrol.Provider, error) {
	switch request.Operation {
	case inboxcontrol.OpGetCapabilities, inboxcontrol.OpGetState, inboxcontrol.OpTags:
	default:
		return nil, inboxcontrol.ErrUnavailable
	}
	write := request.Operation.IsMutation()
	capability := a.microsoftInboxWriteCapability(ctx, source)
	if write && capability != inboxcontrol.CapabilitySupported {
		if capability == inboxcontrol.CapabilityPermissionRequired {
			return nil, inboxcontrol.ErrDenied
		}
		return nil, inboxcontrol.ErrUnavailable
	}
	factory := a.microsoftTagClientFactory
	if factory == nil {
		factory = func(ctx context.Context, source *store.Source, write bool) (*msmail.Client, error) {
			state := invocationFromContext(ctx)
			manager := newGraphMailManager(state)
			if write {
				manager = newGraphMailWriteManager(state)
			}
			token, err := manager.TokenSource(ctx, source.Identifier)
			if err != nil {
				return nil, err
			}
			return msmail.NewClient(msmail.GraphBaseURL, token, msmailQPS), nil
		}
	}
	client, err := factory(ctx, source, write)
	if err != nil || client == nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	return msmail.NewInboxProvider(client, binding).WithWriteCapability(capability), nil
}

func (a *storeAPIAdapter) microsoftInboxWriteCapability(ctx context.Context, source *store.Source) inboxcontrol.CapabilityStatus {
	granted, err := newGraphMailWriteManager(invocationFromContext(ctx)).HasScopes(source.Identifier)
	if err != nil {
		return inboxcontrol.CapabilityUnavailable
	}
	if !granted {
		return inboxcontrol.CapabilityPermissionRequired
	}
	return inboxcontrol.CapabilitySupported
}
