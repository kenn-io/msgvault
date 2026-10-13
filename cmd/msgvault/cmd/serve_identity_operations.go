package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/identitycontrol"
	"go.kenn.io/msgvault/internal/store"
)

func (a *storeAPIAdapter) IdentityOperationPreviewContext(ctx context.Context, operation identitycontrol.Operation, target identitycontrol.IdentityTarget) (*store.IdentitySnapshot, error) {
	return a.store.IdentityOperationPreviewContext(ctx, operation, target)
}
func (a *storeAPIAdapter) ApplyIdentityOperationWithPreviewContext(ctx context.Context, request store.IdentityOperationRequest, authorize, verifyPreview func(context.Context, *store.IdentitySnapshot) error) (*store.IdentityReceipt, error) {
	return a.store.ApplyIdentityOperationWithPreviewContext(ctx, request, authorize, verifyPreview)
}
func (a *storeAPIAdapter) IdentityOperationReceiptContext(ctx context.Context, principal, key string) (*store.IdentityReceipt, error) {
	return a.store.IdentityOperationReceiptContext(ctx, principal, key)
}
func (a *storeAPIAdapter) IdentityReceiptByIDContext(ctx context.Context, id string) (*store.IdentityReceipt, error) {
	return a.store.IdentityReceiptByIDContext(ctx, id)
}

func (a *storeAPIAdapter) IdentityOperationReceiptAuthorizedContext(ctx context.Context, principal, key, authority string, authorize func(context.Context, *store.IdentitySnapshot) error) (*store.IdentityReceipt, error) {
	return a.store.IdentityOperationReceiptAuthorizedContext(ctx, principal, key, authority, authorize)
}
