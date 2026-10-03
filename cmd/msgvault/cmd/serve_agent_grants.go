package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/store"
)

func (a *storeAPIAdapter) GetMessageSourceContext(ctx context.Context, id int64) (*store.Source, error) {
	return a.store.GetMessageSourceContext(ctx, id)
}
func (a *storeAPIAdapter) AgentAttachmentSourceIDsContext(ctx context.Context, id int64, hash string) ([]int64, error) {
	return a.store.AgentAttachmentSourceIDsContext(ctx, id, hash)
}
