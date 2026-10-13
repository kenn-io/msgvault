package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

var (
	_ api.InboxCandidateReader = (*storeAPIAdapter)(nil)
	_ api.InboxContextReader   = (*storeAPIAdapter)(nil)
)

func (a *storeAPIAdapter) InboxCandidates(ctx context.Context, source inboxcontrol.SourceIdentity, scope inboxcontrol.Scope, limit int, cursor string) (*inboxcontrol.CandidatePage, error) {
	return a.store.InboxCandidates(a.invocationContext(ctx), source, scope, limit, cursor)
}

func (a *storeAPIAdapter) InboxContext(ctx context.Context, request inboxcontrol.ContextRequest) (*inboxcontrol.Context, error) {
	return a.store.InboxContext(a.invocationContext(ctx), request)
}
