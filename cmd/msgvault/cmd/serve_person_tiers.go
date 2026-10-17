package cmd

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
)

var _ api.PersonTierStore = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) SetPersonTrackingActorContext(ctx context.Context, personID int64, tracked bool, actor string) (*store.PersonTracking, error) {
	return a.store.SetPersonTrackingActorContext(ctx, personID, tracked, actor)
}
func (a *storeAPIAdapter) PlanPersonTiersContext(ctx context.Context, req store.PersonTierPageRequest) (*store.PersonTierPage, error) {
	return a.store.PlanPersonTiersContext(ctx, req)
}
func (a *storeAPIAdapter) ApplyPersonTiersPageContext(ctx context.Context, req store.PersonTierApplyRequest) (*store.PersonTierPage, error) {
	return a.store.ApplyPersonTiersPageContext(ctx, req)
}
func (a *storeAPIAdapter) ExplainPersonTierContext(ctx context.Context, personID int64, req store.PersonTierPageRequest) (*store.PersonTierExplanation, error) {
	return a.store.ExplainPersonTierContext(ctx, personID, req)
}
func (a *storeAPIAdapter) ResetPersonTiersContext(ctx context.Context, actor string) (*store.PersonTierResetResult, error) {
	return a.store.ResetPersonTiersContext(ctx, actor)
}
func (a *storeAPIAdapter) PersonTierHistoryContext(ctx context.Context, personID int64, limit, offset int) ([]store.PersonTierEvent, error) {
	return a.store.PersonTierHistoryContext(ctx, personID, limit, offset)
}
func (a *storeAPIAdapter) PersonTierCandidatesContext(ctx context.Context, afterID int64, limit int, asOf time.Time, cfg persontiers.Config, timezone string, maxDirect int) (store.PersonTierCandidatePage, error) {
	return a.store.PersonTierCandidatesContext(ctx, afterID, limit, asOf, cfg, timezone, maxDirect)
}
func (a *storeAPIAdapter) SetPersonTierOptOutContext(ctx context.Context, personID int64, lane string, optedOut bool, actor, reason string) error {
	return a.store.SetPersonTierOptOutContext(ctx, personID, lane, optedOut, actor, reason)
}
func (a *storeAPIAdapter) ListPersonTierOptOutsContext(ctx context.Context, personID int64) ([]store.PersonTierOptOut, error) {
	return a.store.ListPersonTierOptOutsContext(ctx, personID)
}
