package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
)

func (a *storeAPIAdapter) resolveBeeperInboxProvider(ctx context.Context, request inboxcontrol.Request, source *store.Source, binding inboxcontrol.SourceIdentity) (inboxcontrol.Provider, error) {
	if request.Operation.IsMutation() {
		return nil, inboxcontrol.ErrUnavailable
	}
	factory := a.beeperInboxClientFactory
	if factory == nil {
		factory = func(context.Context, *store.Source) (*beeper.Client, error) {
			if a.config == nil {
				return nil, inboxcontrol.ErrUnavailable
			}
			token, err := beeper.LoadToken(a.config.TokensDir())
			if err != nil {
				return nil, inboxcontrol.ErrUnavailable
			}
			return beeperClient(a.config, token), nil
		}
	}
	client, err := factory(ctx, source)
	if err != nil || client == nil {
		return nil, inboxcontrol.ErrUnavailable
	}
	return &beeperInboxObservationProvider{InboxProvider: beeper.NewInboxProvider(client, binding), adapter: a}, nil
}

// This is the integration-owned metadata producer, under the existing source
// execution/removal fence. It does not introduce an Events writer or a crawl.
type beeperInboxObservationProvider struct {
	*beeper.InboxProvider
	adapter *storeAPIAdapter
}

func (p *beeperInboxObservationProvider) Observe(ctx context.Context, request inboxcontrol.Request) (inboxcontrol.State, error) {
	if request.Target == nil {
		return inboxcontrol.State{}, inboxcontrol.ErrDenied
	}
	execution, err := p.adapter.store.AcquireInboxExecutionContext(ctx, request.Target.SourceID)
	if err != nil {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	defer func() {
		if err := execution.Release(); err != nil && p.adapter.logger != nil {
			p.adapter.logger.Error("release Beeper Inbox observation source", "error", err)
		}
	}()
	if _, _, err := p.adapter.inboxBinding(ctx, request); err != nil {
		return inboxcontrol.State{}, err
	}
	state, err := p.InboxProvider.Observe(ctx, request)
	if err != nil {
		return inboxcontrol.State{}, err
	}
	if _, err := p.adapter.store.ObserveInboxState(ctx, state); err != nil {
		return inboxcontrol.State{}, inboxcontrol.ErrUnavailable
	}
	return state, nil
}
