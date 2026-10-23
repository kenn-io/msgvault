package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/personobs"
	"go.kenn.io/msgvault/internal/store"
)

var _ api.PersonObservationStore = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) ListPersonObservations(ctx context.Context, id int64, f personobs.Filter) (personobs.Page, error) {
	return a.store.ListPersonObservations(ctx, id, f)
}
func (a *storeAPIAdapter) GetPersonObservation(ctx context.Context, id int64) (personobs.Detail, error) {
	return a.store.GetPersonObservation(ctx, id)
}
func (a *storeAPIAdapter) AddOwnerObservation(ctx context.Context, in personobs.OwnerInput) (personobs.Observation, error) {
	return a.store.AddOwnerObservation(ctx, in)
}
func (a *storeAPIAdapter) DecidePersonObservation(ctx context.Context, d personobs.Decision) (personobs.Observation, error) {
	return a.store.DecidePersonObservation(ctx, d)
}
func (a *storeAPIAdapter) ClearObservationSuppression(ctx context.Context, id int64, key, actor string) error {
	return a.store.ClearObservationSuppression(ctx, id, key, actor)
}
func (a *storeAPIAdapter) ListObservationSuppressions(ctx context.Context, id int64) ([]store.ObservationSuppression, error) {
	return a.store.ListObservationSuppressions(ctx, id)
}
