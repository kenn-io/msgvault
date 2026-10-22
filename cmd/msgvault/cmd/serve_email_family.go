package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

var _ api.EmailFamilyStore = (*storeAPIAdapter)(nil)
var _ api.EmailFamilyDecisionStore = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) EmailFamilyContext(ctx context.Context, messageID int64, options store.EmailFamilyOptions) (*store.EmailFamily, error) {
	return a.store.EmailFamilyContext(ctx, messageID, options)
}

func (a *storeAPIAdapter) GetEmailFamilyDecisionContext(ctx context.Context, decisionID int64, options store.EmailFamilyOptions) (*store.EmailFamilyDecisionResult, error) {
	return a.store.GetEmailFamilyDecisionContext(ctx, decisionID, options)
}

func (a *storeAPIAdapter) PreviewEmailFamilyDecisionContext(ctx context.Context, input store.EmailFamilyDecisionInput) (*store.EmailFamilyDecisionPreview, error) {
	return a.store.PreviewEmailFamilyDecisionContext(ctx, input)
}

func (a *storeAPIAdapter) ApplyEmailFamilyDecisionContext(ctx context.Context, input store.EmailFamilyDecisionInput) (*store.EmailFamilyDecisionResult, error) {
	return a.store.ApplyEmailFamilyDecisionContext(ctx, input)
}

func (a *storeAPIAdapter) UndoEmailFamilyDecisionContext(ctx context.Context, decisionID int64, revision int64, familyRevision string, options store.EmailFamilyOptions) (*store.EmailFamilyDecisionResult, error) {
	return a.store.UndoEmailFamilyDecisionContext(ctx, decisionID, revision, familyRevision, options)
}

func (a *storeAPIAdapter) ListEmailFamilyDecisionsContext(ctx context.Context, messageID int64, options store.EmailFamilyOptions) ([]store.EmailFamilyDecision, error) {
	return a.store.ListEmailFamilyDecisionsContext(ctx, messageID, options)
}
