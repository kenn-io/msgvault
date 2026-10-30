package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

var _ api.LineStatusStore = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) LineInboxStatusContext(ctx context.Context, sourceID int64) (store.LineInboxStatus, error) {
	return a.store.LineInboxStatusContext(ctx, sourceID)
}
