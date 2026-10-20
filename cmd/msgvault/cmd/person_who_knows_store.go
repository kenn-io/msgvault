package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/whoknows"
)

var _ whoknows.DataReader = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) OwnerFamilyContext(ctx context.Context) ([]int64, error) {
	return a.store.OwnerFamilyContext(ctx)
}

func (a *storeAPIAdapter) TopicAuthorsContext(ctx context.Context, ids, owner []int64) (map[int64]store.TopicAuthor, error) {
	return a.store.TopicAuthorsContext(ctx, ids, owner)
}

func (a *storeAPIAdapter) AskMeAboutValuesContext(ctx context.Context, owner []int64) ([]store.PersonAskMeAbout, bool, error) {
	return a.store.AskMeAboutValuesContext(ctx, owner)
}

func (a *storeAPIAdapter) CoverageCountsScoped(ctx context.Context, activeGen int64, messageTypes []string, sourceIDs []int64) (live, stamped, blank, missing int64, err error) {
	return a.store.CoverageCountsScoped(ctx, activeGen, messageTypes, sourceIDs)
}
