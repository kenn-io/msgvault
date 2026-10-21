package sync

import (
	"context"
	"errors"
	"net/http"

	imaplib "github.com/emersion/go-imap/v2"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/store"
	"golang.org/x/oauth2"
)

var errNativeSourcePermissionReduction = errors.New("observe native source permission denial")

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

	previouslyAllowed, err := s.store.NativeSourceMessagePermission(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	if previouslyAllowed {
		observation.Known, observation.MessageRead = true, true
	}
	s.nativeSourcePermission = &observation
	profile, err := s.client.GetProfile(ctx)
	if err != nil {
		return nil, errors.Join(err, s.observeNativeSourceDenial(ctx, err), s.observeNativeSourcePermissionReduction(ctx))
	}
	if profile == nil {
		return nil, errors.Join(errors.New("read authenticated Gmail identity: provider returned no profile"), s.observeNativeSourcePermissionReduction(ctx))
	}
	if _, err := provideridentity.GmailProfileEvidence(source, profile.EmailAddress); err != nil {
		// A foreign authenticated response cannot grant authority to this
		// account or preserve a previous grant. Fail before archive ingestion.
		observation.Known, observation.MessageRead = true, false
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

// Reduce only the source lifetime pinned before provider IO, through this run's
// scoped Store. A denial that revokes granted authority stops the run so
// incremental sync cannot advance past a partially denied batch. Quota and
// transport failures do not establish revocation.
func (s *Syncer) observeNativeSourceDenial(ctx context.Context, cause error) error {
	if s.nativeSourcePermission == nil || cause == nil {
		return nil
	}
	wasAllowed := s.nativeSourcePermission.Known && s.nativeSourcePermission.MessageRead
	revoked := false
	if refresh, ok := errors.AsType[*oauth2.RetrieveError](cause); ok {
		revoked = refresh.ErrorCode == "invalid_grant"
	}
	if status, ok := errors.AsType[*gmail.StatusError](cause); ok {
		revoked = revoked || status.StatusCode == http.StatusUnauthorized || status.StatusCode == http.StatusForbidden
	}
	if status, ok := errors.AsType[*imaplib.Error](cause); ok {
		revoked = revoked || status.Code == imaplib.ResponseCodeAuthenticationFailed || status.Code == imaplib.ResponseCodeAuthorizationFailed
	}
	if !revoked {
		return nil
	}
	observation := *s.nativeSourcePermission
	observation.Known, observation.MessageRead = true, false
	reduction := s.store.ObserveNativeSourcePermission(ctx, observation)
	if reduction == nil {
		// The original source reference has been retired. Do not reread and adopt
		// its replacement if another item in the same batch reports denial.
		s.nativeSourcePermission = nil
	}
	if reduction != nil {
		return errors.Join(errNativeSourcePermissionReduction, reduction)
	}
	if wasAllowed {
		return errNativeSourcePermissionReduction
	}
	return nil
}

// observeNativeSourcePermissionReduction checks token metadata after a
// provider request. A successful OAuth refresh can narrow Gmail grants even
// when the request itself succeeds, so persist a known read loss against the
// source lifetime pinned before provider IO and stop this run before it can
// publish more work under the old grant.
func (s *Syncer) observeNativeSourcePermissionReduction(ctx context.Context) error {
	pinned := s.nativeSourcePermission
	if pinned == nil || !pinned.Known || !pinned.MessageRead {
		return nil
	}
	native, ok := s.client.(nativeMessageReadPermission)
	if !ok {
		return nil
	}
	messageRead, known := native.NativeMessageReadPermission()
	if known && messageRead {
		return nil
	}

	observation := *pinned
	observation.Known = known
	observation.MessageRead = messageRead
	if err := s.store.ObserveNativeSourcePermission(ctx, observation); err != nil {
		return errors.Join(errNativeSourcePermissionReduction, err)
	}
	// The source lifetime has been retired. Do not reread and adopt its
	// replacement if another item in the same batch observes a denial.
	s.nativeSourcePermission = nil
	return errNativeSourcePermissionReduction
}
