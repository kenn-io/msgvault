package cmd

import (
	"context"
	"errors"
	"log/slog"

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
	if err != nil {
		return id, err
	}
	if a.draftCacheRefresh != nil {
		refreshCtx := context.WithoutCancel(a.invocationContext(ctx))
		if err := a.draftCacheRefresh(refreshCtx, callhistory.SourceType); err != nil && !errors.Is(err, context.Canceled) {
			logger := a.logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.ErrorContext(refreshCtx, "call analytics cache refresh failed",
				"source", callhistory.SourceType,
				"source_id", sourceID,
				"message_id", id,
				"error", err,
			)
		}
	}
	return id, nil
}

func (a *storeAPIAdapter) ListCallObservationsContext(ctx context.Context, q store.CallQuery) ([]store.CallObservationView, error) {
	return a.store.ListCallObservationsContext(ctx, q)
}

func (a *storeAPIAdapter) SummarizeCallsContext(ctx context.Context, q store.CallQuery) ([]store.PersonCallSummary, error) {
	return a.store.SummarizeCallsContext(ctx, q)
}
