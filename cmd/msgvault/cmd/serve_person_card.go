package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

var _ api.PersonCardStore = (*storeAPIAdapter)(nil)
var _ api.PersonCardStore = (*store.Store)(nil)

func (a *storeAPIAdapter) ResolvePersonByEmailContext(ctx context.Context, email string) ([]int64, error) {
	return a.store.ResolvePersonByEmailContext(ctx, email)
}
func (a *storeAPIAdapter) ListPersonSourcesContext(ctx context.Context, id int64, limit int) ([]store.PersonSource, error) {
	return a.store.ListPersonSourcesContext(ctx, id, limit)
}
func (a *storeAPIAdapter) EmailParticipantContext(ctx context.Context, email string) (int64, error) {
	return a.store.EmailParticipantContext(ctx, email)
}
func (a *storeAPIAdapter) ListPersonContactPointsContext(ctx context.Context, id int64, current bool) ([]store.PersonContactPoint, error) {
	return a.store.ListPersonContactPointsContext(ctx, id, current)
}
func (a *storeAPIAdapter) ListPersonCategoriesContext(ctx context.Context, id int64, current bool) ([]store.PersonCategory, error) {
	return a.store.ListPersonCategoriesContext(ctx, id, current)
}
func (a *storeAPIAdapter) GetIdentitiesForScope(ids []int64) (map[string]struct{}, error) {
	return a.store.GetIdentitiesForScope(ids)
}
