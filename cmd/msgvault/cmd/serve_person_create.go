package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/store"
)

func (a *storeAPIAdapter) CreateStandalonePersonContext(
	ctx context.Context, input store.PersonCreateInput,
) (*store.Person, error) {
	return a.store.CreateStandalonePersonContext(ctx, input)
}
