package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

var (
	_ api.ScopedPersonEditStore      = (*storeAPIAdapter)(nil)
	_ api.ScopedPersonAttributeStore = (*storeAPIAdapter)(nil)
	_ api.ScopedPersonMergeStore     = (*storeAPIAdapter)(nil)
)

func (a *storeAPIAdapter) IdentityGrantSelectionContext(ctx context.Context, people, books []int64) (*store.IdentityGrantSelection, error) {
	return a.store.IdentityGrantSelectionContext(ctx, people, books)
}

func (a *storeAPIAdapter) PersonEditScopeContext(ctx context.Context, id int64) (*store.IdentityGrantSelection, error) {
	return a.store.PersonEditScopeContext(ctx, id)
}

func (a *storeAPIAdapter) PersonProfileEditScopeContext(ctx context.Context, id int64) (*store.IdentityGrantSelection, error) {
	return a.store.PersonProfileEditScopeContext(ctx, id)
}

func (a *storeAPIAdapter) UpdatePersonDisplayNameAuthorizedContext(ctx context.Context, id, revision int64, name *string, authorize store.PersonEditAuthorizer) (*store.Person, error) {
	return a.store.UpdatePersonDisplayNameAuthorizedContext(ctx, id, revision, name, authorize)
}

func (a *storeAPIAdapter) ApplyPersonProfilePatchAuthorizedContext(ctx context.Context, id, revision int64, patch store.PersonProfilePatch, authorize store.PersonEditAuthorizer) (*store.PersonProfile, error) {
	return a.store.ApplyPersonProfilePatchAuthorizedContext(ctx, id, revision, patch, authorize)
}

func (a *storeAPIAdapter) SetPersonAttributeValueAuthorizedContext(ctx context.Context, input store.PersonAttributeValueInput, authorize store.PersonEditAuthorizer) (*store.PersonAttributeWrite, error) {
	return a.store.SetPersonAttributeValueAuthorizedContext(ctx, input, authorize)
}

func (a *storeAPIAdapter) SupersedePersonAttributeValueAuthorizedContext(ctx context.Context, input store.PersonAttributeSupersedeInput, authorize store.PersonEditAuthorizer) (*store.PersonAttributeWrite, error) {
	return a.store.SupersedePersonAttributeValueAuthorizedContext(ctx, input, authorize)
}

func (a *storeAPIAdapter) MergePersonsAuthorizedContext(ctx context.Context, request store.PersonMergeRequest, authorize store.PersonEditAuthorizer) (*store.PersonMergeResult, error) {
	return a.store.MergePersonsAuthorizedContext(ctx, request, authorize)
}

func (a *storeAPIAdapter) ListPersonAttributeValuesForUIDContext(ctx context.Context, id int64, uid string, query store.PersonAttributeQuery) (*store.Person, []store.PersonAttributeValue, error) {
	return a.store.ListPersonAttributeValuesForUIDContext(ctx, id, uid, query)
}
