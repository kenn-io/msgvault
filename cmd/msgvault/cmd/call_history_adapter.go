package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/store"
)

var _ api.CallHistoryStore = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) GetOrCreateSourceContext(ctx context.Context, sourceType, identifier string) (*store.Source, error) {
	return a.store.GetOrCreateSourceContext(ctx, sourceType, identifier)
}

func (a *storeAPIAdapter) PutCallObservationContext(ctx context.Context, sourceID int64, o callhistory.Observation) (int64, error) {
	id, err := a.store.PutCallObservationContext(ctx, sourceID, o)
	if err == nil {
		refreshCtx := context.WithoutCancel(a.invocationContext(ctx))
		if refreshErr := rebuildCacheAfterScheduledSync(refreshCtx, callhistory.SourceType); refreshErr != nil && a.logger != nil {
			a.logger.Warn("Call history cache refresh deferred", "error_class", "internal")
		}
	}
	return id, err
}

func (a *storeAPIAdapter) ListCallObservationsContext(ctx context.Context, q store.CallQuery) ([]store.CallObservationView, error) {
	return a.store.ListCallObservationsContext(ctx, q)
}

func (a *storeAPIAdapter) SummarizeCallsContext(ctx context.Context, q store.CallQuery) ([]store.PersonCallSummary, error) {
	return a.store.SummarizeCallsContext(ctx, q)
}
