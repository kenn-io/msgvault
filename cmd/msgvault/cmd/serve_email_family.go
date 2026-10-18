package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

var _ api.EmailFamilyStore = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) EmailFamilyContext(ctx context.Context, messageID int64, options store.EmailFamilyOptions) (*store.EmailFamily, error) {
	return a.store.EmailFamilyContext(ctx, messageID, options)
}
