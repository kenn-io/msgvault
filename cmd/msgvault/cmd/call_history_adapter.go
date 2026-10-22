package cmd

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/store"
)

const callHistoryCacheRefreshQuietPeriod = 2 * time.Second

var _ api.CallHistoryStore = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) GetOrCreateSourceContext(ctx context.Context, sourceType, identifier string) (*store.Source, error) {
	return a.store.GetOrCreateSourceContext(ctx, sourceType, identifier)
}

func (a *storeAPIAdapter) PutCallObservationContext(ctx context.Context, sourceID int64, o callhistory.Observation) (int64, error) {
	id, err := a.store.PutCallObservationContext(ctx, sourceID, o)
	if err != nil {
		return id, err
	}
	a.refreshCallHistoryCache(ctx)
	return id, nil
}

func (a *storeAPIAdapter) refreshCallHistoryCache(ctx context.Context) {
	if a.callHistoryCacheRefresh == nil {
		return
	}
	if err := a.callHistoryCacheRefresh(ctx, callhistory.SourceType); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		if a.logger != nil {
			a.logger.Error("call history analytics cache refresh failed", "error", err)
		}
	}
}

func (a *storeAPIAdapter) ListCallObservationsContext(ctx context.Context, q store.CallQuery) ([]store.CallObservationView, error) {
	return a.store.ListCallObservationsContext(ctx, q)
}

func (a *storeAPIAdapter) SummarizeCallsContext(ctx context.Context, q store.CallQuery) ([]store.PersonCallSummary, error) {
	return a.store.SummarizeCallsContext(ctx, q)
}
