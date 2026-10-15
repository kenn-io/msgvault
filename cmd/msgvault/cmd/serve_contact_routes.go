package cmd

import (
	"context"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

var _ api.ContactRouteStore = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) FindContactCandidatesContext(ctx context.Context, q store.ContactCandidateQuery) (*store.ContactCandidatePage, error) {
	return a.store.FindContactCandidatesContext(ctx, q)
}
func (a *storeAPIAdapter) GetPersonMessagingRoutesContext(ctx context.Context, q store.PersonMessagingRouteQuery) (*store.PersonMessagingRoutesPage, error) {
	return a.store.GetPersonMessagingRoutesContext(ctx, q)
}
