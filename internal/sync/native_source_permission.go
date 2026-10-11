package sync

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/store"
	"golang.org/x/oauth2"
)

type nativeMessageReadPermission interface {
	NativeMessageReadPermission() (read, known bool)
}

// nativeProfileWithPermission pins the account before provider IO, then binds
// the authenticated native identity and current native grant to that lifetime.
// An empty successful sync is not permission evidence. IMAP uses its separate
// authenticated session and mailbox epoch authority.
func (s *Syncer) nativeProfileWithPermission(ctx context.Context, source *store.Source) (*gmail.Profile, error) {
	if source.SourceType == sourceTypeIMAP {
		return s.nativeIMAPProfileWithPermission(ctx, source)
	}
	if source.SourceType != sourceTypeGmail {
		return s.client.GetProfile(ctx)
	}
	resource, err := s.store.GetMCPResource(ctx, "source", source.ID)
	if err != nil {
		return nil, err
	}
	if resource.SourceType == "" && source.SourceType == sourceTypeGmail && store.EqualIdentifier(resource.ProviderID, source.Identifier) {
		// FullWithFinalizer normalizes legacy empty source types in memory. Keep
		// syncing that selected row; an untyped archive is not native Events evidence.
		return s.client.GetProfile(ctx)
	}
	if resource.SourceType != source.SourceType || !store.EqualIdentifier(resource.ProviderID, source.Identifier) {
		return nil, errors.New("native_authority_unavailable")
	}
	observation := store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: sourceTypeGmail, NativeIdentity: source.Identifier}
	if _, native := s.client.(nativeMessageReadPermission); !native {
		// Events-only grant checks must preserve ordinary archival semantics.
		// Generic archive adapters cannot establish native permission evidence.
		if err := s.store.ObserveNativeSourcePermission(ctx, observation); err != nil {
			return nil, err
		}
		return s.client.GetProfile(ctx)
	}

	profile, err := s.client.GetProfile(ctx)
	if err != nil {
		// Quota errors are a separate native error type. Only an observed
		// authorization/permission denial retires the account's prior authority.
		revoked := false
		if refresh, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
			revoked = refresh.ErrorCode == "invalid_grant"
		}
		if status, ok := errors.AsType[*gmail.StatusError](err); ok {
			revoked = revoked || status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden
		}
		if revoked {
			observation.Known = true
			return nil, errors.Join(err, s.store.ObserveNativeSourcePermission(ctx, observation))
		}
		return nil, err
	}
	if profile == nil {
		return nil, errors.New("read authenticated Gmail identity: provider returned no profile")
	}
	if _, err := provideridentity.GmailProfileEvidence(source, profile.EmailAddress); err != nil {
		// A foreign authenticated response cannot grant authority to this
		// account or preserve a previous grant. Fail before archive ingestion.
		observation.Known = true
		reduction := s.store.ObserveNativeSourcePermission(ctx, observation)
		if !s.store.MCPMessageCaptureEnabled(sourceTypeGmail) {
			// Retain ordinary alias/profile-change archival behavior when this
			// source is unselected, while retiring its mismatched Events grant.
			return profile, reduction
		}
		return nil, errors.Join(errors.New("native_authority_unavailable"), reduction)
	}
	if native, ok := s.client.(nativeMessageReadPermission); ok {
		observation.MessageRead, observation.Known = native.NativeMessageReadPermission()
	}
	if err := s.store.ObserveNativeSourcePermission(ctx, observation); err != nil {
		return nil, err
	}
	return profile, nil
}
