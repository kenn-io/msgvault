package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/mailkind"
	"go.kenn.io/msgvault/internal/mailkindworker"
	"go.kenn.io/msgvault/internal/store"
)

type mailKindConsentStoreAdapter interface {
	GrantMailKindConsentContext(ctx context.Context, disclosure mailkind.Disclosure, actor string, mutationGate func(context.Context) (func(), error)) (*store.ProviderConsent, bool, error)
	RevokeMailKindConsentContext(ctx context.Context, fingerprint, actor string, mutationGate func(context.Context) (func(), error)) (bool, error)
	HasMailKindConsentContext(ctx context.Context, fingerprint string) (bool, error)
}

var _ mailKindConsentStoreAdapter = (*storeAPIAdapter)(nil)
var _ mailkindworker.Store = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) GrantMailKindConsentContext(
	ctx context.Context,
	disclosure mailkind.Disclosure,
	actor string,
	mutationGate func(context.Context) (func(), error),
) (*store.ProviderConsent, bool, error) {
	return a.store.GrantMailKindConsentContext(ctx, disclosure, actor, mutationGate)
}

func (a *storeAPIAdapter) RevokeMailKindConsentContext(
	ctx context.Context,
	fingerprint, actor string,
	mutationGate func(context.Context) (func(), error),
) (bool, error) {
	return a.store.RevokeMailKindConsentContext(ctx, fingerprint, actor, mutationGate)
}

func (a *storeAPIAdapter) HasMailKindConsentContext(ctx context.Context, fingerprint string) (bool, error) {
	return a.store.HasMailKindConsentContext(ctx, fingerprint)
}

func (a *storeAPIAdapter) MailKindConsentEgressContext(ctx context.Context, fingerprint string, dispatch func() error) (bool, error) {
	return a.store.MailKindConsentEgressContext(ctx, fingerprint, dispatch)
}

func (a *storeAPIAdapter) ListMailKindCandidatesContext(ctx context.Context, limit int, ids []int64) ([]int64, error) {
	return a.store.ListMailKindCandidatesContext(ctx, limit, ids)
}

func (a *storeAPIAdapter) NextMailKindCandidatesContext(ctx context.Context, fingerprint string, limit int) ([]int64, error) {
	return a.store.NextMailKindCandidatesContext(ctx, fingerprint, limit)
}

func (a *storeAPIAdapter) MailKindInputContext(ctx context.Context, id int64) (mailkind.Input, string, error) {
	return a.store.MailKindInputContext(ctx, id)
}

func (a *storeAPIAdapter) ApplyMailKindLabelContext(
	ctx context.Context,
	fingerprint string,
	id int64,
	token, kind string,
	mutationGate func(context.Context) (func(), error),
) (bool, error) {
	return a.store.ApplyMailKindLabelContext(ctx, fingerprint, id, token, kind, mutationGate)
}
