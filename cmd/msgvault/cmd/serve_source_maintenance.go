package cmd

import (
	"context"
	"go.kenn.io/msgvault/internal/store"
)

func (a *storeAPIAdapter) UpdateSourceSettingsContext(ctx context.Context, sourceID int64, update store.SourceSettingsUpdate) (store.SourceSettings, error) {
	return a.store.UpdateSourceSettingsContext(ctx, sourceID, update)
}

func (a *storeAPIAdapter) MergeSourcesContext(ctx context.Context, req store.MergeSourcesRequest) (store.SourceMergeResult, error) {
	return a.store.MergeSourcesContext(ctx, req)
}
