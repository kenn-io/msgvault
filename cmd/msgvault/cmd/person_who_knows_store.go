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

func (a *storeAPIAdapter) OwnerAddressedContext(ctx context.Context, ids []int64) (map[int64]bool, error) {
	return a.store.OwnerAddressedContext(ctx, ids)
}

func (a *storeAPIAdapter) AskMeAboutValuesContext(ctx context.Context) ([]store.PersonAskMeAbout, bool, error) {
	return a.store.AskMeAboutValuesContext(ctx)
}

func (a *storeAPIAdapter) GetMessageRawsContext(ctx context.Context, ids []int64) (map[int64][]byte, error) {
	return a.store.GetMessageRawsContext(ctx, ids)
}
