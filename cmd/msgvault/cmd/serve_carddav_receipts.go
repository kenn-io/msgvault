package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/store"
)

// Preserve the native Store's caller/request binding and authorization callback
// through the daemon adapter. These methods do not select owner credentials or
// dispatch provider work.
func (a *storeAPIAdapter) WithReviewedCardDAVPublicationReceipt(ctx context.Context, principal, key string, personID int64, token string) (context.Context, error) {
	return a.store.WithReviewedCardDAVPublicationReceipt(ctx, principal, key, personID, token)
}

func (a *storeAPIAdapter) ReviewedCardDAVPublicationReceiptContext(ctx context.Context, authorize store.PersonEditAuthorizer) (*store.CardDAVPublicationReceipt, error) {
	return a.store.ReviewedCardDAVPublicationReceiptContext(ctx, authorize)
}

func (a *storeAPIAdapter) ReviewedCardDAVPublicationRecoveryContext(ctx context.Context, authorize store.PersonEditAuthorizer) (*store.CardDAVPublicationReceipt, *store.CardDAVPublication, error) {
	return a.store.ReviewedCardDAVPublicationRecoveryContext(ctx, authorize)
}

func (a *storeAPIAdapter) LoadCardDAVPublicationReviewSourceAuthorizedContext(ctx context.Context, personID int64, authorize store.PersonEditAuthorizer) (*store.CardDAVPublicationReviewSource, error) {
	return a.store.LoadCardDAVPublicationReviewSourceAuthorizedContext(ctx, personID, authorize)
}
