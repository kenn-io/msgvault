package cmd

import (
	"context"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
	xapi "go.kenn.io/msgvault/internal/x"
	"time"
)

func (a *storeAPIAdapter) UpdateSourceSettingsContext(ctx context.Context, sourceID int64, update store.SourceSettingsUpdate) (store.SourceSettings, error) {
	return a.store.UpdateSourceSettingsContext(ctx, sourceID, update)
}

func (a *storeAPIAdapter) ReadXSourceStatusContext(ctx context.Context, sourceID int64, now time.Time) (*api.XSourceStatus, error) {
	source, err := a.store.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	policy, err := a.config.XAccount(source.Identifier)
	if err != nil {
		return nil, err
	}
	retention, err := xapi.ReadRefreshStatus(ctx, a.store, sourceID, now, policy.RefreshSweepInterval)
	if err != nil {
		return nil, err
	}
	state, err := a.store.LoadXControlContext(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	status := &api.XSourceStatus{Generation: state.Generation, MonthlyBudgetMicros: policy.MonthlyBudgetMicros, Month: now.UTC().Format("2006-01"), Retention: retention, ModelError: state.ModelError}
	switch state.CredentialError {
	case "", "invalid_grant", "unauthorized":
		status.CredentialError = state.CredentialError
	default:
		status.CredentialError = "credentials_disabled"
	}
	if state.Own != nil {
		status.OwnComplete = state.Own.Complete
		status.HistoryGapPossible = state.Own.HistoryGapPossible
	}
	if state.Mentions != nil {
		status.MentionsComplete = state.Mentions.Complete
		status.HistoryGapPossible = status.HistoryGapPossible || state.Mentions.HistoryGapPossible
	}
	for _, bucket := range state.SpendBuckets {
		if bucket.Month == status.Month {
			status.EstimatedMicros = bucket.SpentMicros
			status.ReservedMicros = bucket.ReservedMicros
		}
	}
	return status, nil
}

var _ api.XSourceStatusReader = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) MergeSourcesContext(ctx context.Context, req store.MergeSourcesRequest) (store.SourceMergeResult, error) {
	return a.store.MergeSourcesContext(ctx, req)
}
