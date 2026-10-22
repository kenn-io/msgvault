package cmd

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/syncerr"
	"golang.org/x/oauth2"
)

// Pin the selected native account before credential IO. A stale failure cannot
// pause a replacement lifetime, even when its identifier has changed back.
//
//nolint:nilnil // No native authority is a valid archive-only result.
func pinNativeMessagePermission(ctx context.Context, email string, source *store.Source, st *store.Store) (*store.NativeSourcePermission, error) {
	if st == nil || source == nil || source.SourceType != sourceTypeGmail && source.SourceType != "msmail" && source.SourceType != sourceTypeIMAP && source.SourceType != sourceTypeSlack && source.SourceType != sourceTypeTeams && source.SourceType != sourceTypeDiscord && source.SourceType != sourceTypeMatrix && source.SourceType != sourceTypeBeeper {
		return nil, nil
	}
	resource, err := st.GetMCPResource(ctx, "source", source.ID)
	if err != nil {
		return nil, err
	}
	if resource.SourceType != source.SourceType || !store.EqualIdentifier(resource.ProviderID, email) || !store.EqualIdentifier(source.Identifier, email) {
		return nil, errors.New("native_authority_unavailable")
	}
	return &store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: source.SourceType, NativeIdentity: email}, nil
}

// Missing local credentials pause Events independently of archive access. They
// do not prove native account removal. Transient network failures retain the
// last native observation; no credential is created or modified here.
func pauseNativeMessagePermission(ctx context.Context, st *store.Store, permission *store.NativeSourcePermission, cause error) error {
	if permission == nil {
		return cause
	}
	observation := *permission
	if refresh, ok := errors.AsType[*oauth2.RetrieveError](cause); ok {
		observation.Known = refresh.ErrorCode == "invalid_grant"
	}
	if !observation.Known && syncerr.IsTransientNetwork(cause) {
		return cause
	}
	return errors.Join(cause, st.ObserveNativeSourcePermission(ctx, observation))
}

// Before callbacks or native writers start, a fresh daemon requires its selected
// adapter to re-observe identity and permission. Persisted prior-process grants
// alone are not current credential evidence. This unknown pause keeps resource
// lifetimes, subscriptions and pending envelopes intact and performs no provider
// or credential IO.
func pausePersistedNativeMessageAuthority(ctx context.Context, st *store.Store) error {
	for _, sourceType := range []string{sourceTypeGmail, "msmail", sourceTypeIMAP, sourceTypeSlack, sourceTypeTeams, sourceTypeDiscord, sourceTypeMatrix, sourceTypeBeeper} {
		sources, err := st.ListSourcesContext(ctx, sourceType)
		if err != nil {
			return err
		}
		for _, source := range sources {
			permission, err := pinNativeMessagePermission(ctx, source.Identifier, source, st)
			if err != nil {
				return err
			}
			if permission != nil {
				if err := st.ObserveNativeSourcePermission(ctx, *permission); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

//nolint:nilnil // No native authority is a valid archive-only result.
func pinNativeGmailPermission(ctx context.Context, email string, source *store.Source, st *store.Store) (*store.NativeSourcePermission, error) {
	if source == nil || source.SourceType != sourceTypeGmail {
		return nil, nil
	}
	return pinNativeMessagePermission(ctx, email, source, st)
}

func pauseNativeGmailPermission(ctx context.Context, st *store.Store, permission *store.NativeSourcePermission, cause error) error {
	return pauseNativeMessagePermission(ctx, st, permission, cause)
}
