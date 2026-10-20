package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/whoknows"
)

var _ whoknows.DataReader = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) TopicAuthorsContext(ctx context.Context, ids []int64) (map[int64]store.TopicAuthor, error) {
	return a.store.TopicAuthorsContext(ctx, ids)
}

func (a *storeAPIAdapter) AskMeAboutValuesContext(ctx context.Context, ids []int64) ([]store.PersonAskMeAbout, error) {
	return a.store.AskMeAboutValuesContext(ctx, ids)
}
