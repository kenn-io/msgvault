package carddav

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

var ErrScopedPublicationReconciliationRequired = errors.New("CardDAV publication requires owner reconciliation")

func (s *Service) validateScopedPublication(authorize store.PersonEditAuthorizer) error {
	if s == nil || s.store == nil || authorize == nil {
		return store.ErrCardDAVInvalidPlan
	}
	// The native DAV transport checks authority before every actual dispatch,
	// including Digest retries and redirects within one Remote call.
	if _, ok := s.remote.(*davRemote); !ok {
		return store.ErrCardDAVInvalidPlan
	}
	return nil
}

func (s *Service) scopedPublicationPlan(ctx context.Context, personID int64, authorize store.PersonEditAuthorizer) (*store.CardDAVPublicationReviewSource, store.CardDAVPublicationPlan, error) {
	source, err := s.store.LoadCardDAVPublicationReviewSourceAuthorizedContext(ctx, personID, authorize)
	if err != nil {
		return nil, store.CardDAVPublicationPlan{}, err
	}
	if source.Resource == nil || source.Resource.PersonID == nil || *source.Resource.PersonID != personID {
		return nil, store.CardDAVPublicationPlan{}, store.ErrCardDAVInvalidPlan
	}
	return s.publicationPlanFromSource(ctx, personID, source)
}

// PreviewPublicationAuthorized previews one mapped update without remote work
// or pending-publication recovery.
func (s *Service) PreviewPublicationAuthorized(ctx context.Context, personID int64, authorize store.PersonEditAuthorizer) (*PublicationPreview, error) {
	if err := s.validateScopedPublication(authorize); err != nil {
		return nil, err
	}
	release, err := s.store.AcquireCardDAVPersonOperation(ctx, personID)
	if err != nil {
		return nil, err
	}
	defer release()
	source, plan, err := s.scopedPublicationPlan(ctx, personID, authorize)
	if err != nil {
		return nil, err
	}
	fence := store.CardDAVCurrentReviewFence(source, plan.OutgoingBody, plan.Href)
	return &PublicationPreview{PersonID: personID, AddressBook: publicAddressBookIdentity(source.Book.ID, source.Book.DisplayName), Kind: PublicationReviewCurrent, VCard: string(plan.OutgoingBody), ApprovalToken: store.CardDAVReviewToken(fence), ReviewRequired: source.Inference.ReviewRequired(source.ConnectionGeneration, source.Book.ID)}, nil
}

// PublishReviewedPersonAuthorized accepts only a reviewed mapped update.
func (s *Service) PublishReviewedPersonAuthorized(ctx context.Context, personID int64, token string, authorize store.PersonEditAuthorizer) error {
	return s.publishReviewedPersonAuthorized(ctx, personID, token, authorize, nil)
}

// PublishReviewedPersonAuthorizedWithAdmission runs trusted receipt admission
// after native preparation commits and before provider work. Admission receives
// an owned snapshot; an error retains the native pending intent without HTTP.
// No-op publications need no provider admission. The callback must not acquire
// another person-operation lease: this service already owns that lease.
func (s *Service) PublishReviewedPersonAuthorizedWithAdmission(ctx context.Context, personID int64, token string, authorize store.PersonEditAuthorizer, admission func(context.Context, store.CardDAVPublication) error) error {
	if admission == nil {
		return store.ErrCardDAVInvalidPlan
	}
	return s.publishReviewedPersonAuthorized(ctx, personID, token, authorize, admission)
}

func (s *Service) publishReviewedPersonAuthorized(ctx context.Context, personID int64, token string, authorize store.PersonEditAuthorizer, admission func(context.Context, store.CardDAVPublication) error) error {
	if err := s.validateScopedPublication(authorize); err != nil {
		return err
	}
	if token == "" {
		return store.ErrCardDAVReviewStale
	}
	release, err := s.store.AcquireCardDAVPersonOperation(ctx, personID)
	if err != nil {
		return err
	}
	defer release()
	return s.publishReviewedPersonAuthorizedUnderLease(ctx, personID, token, authorize, admission)
}

// The caller owns the native person-operation lease.
func (s *Service) publishReviewedPersonAuthorizedUnderLease(ctx context.Context, personID int64, token string, authorize store.PersonEditAuthorizer, admission func(context.Context, store.CardDAVPublication) error) error {
	operationCtx, cancel := context.WithTimeout(ctx, s.operationTimeout())
	defer cancel()
	source, plan, err := s.scopedPublicationPlan(operationCtx, personID, authorize)
	if err != nil {
		return err
	}
	fence := store.CardDAVCurrentReviewFence(source, plan.OutgoingBody, plan.Href)
	pending, err := s.store.PrepareReviewedCardDAVPublicationAuthorizedContext(operationCtx, store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: token}, authorize)
	if err != nil {
		return err
	}
	if pending.Noop {
		return nil
	}
	if pending.PendingOperation != store.CardDAVMutationUpdate {
		return store.ErrCardDAVInvalidPlan
	}
	if admission != nil {
		if err := admission(operationCtx, clonePublicationIntent(*pending)); err != nil {
			return err
		}
	}
	operationCtx = s.withPublicationRequestAuthority(operationCtx, *pending, authorize)
	err = s.gate(operationCtx, func(ctx context.Context) error {
		return s.remote.Put(ctx, pending.Href, pending.OutgoingBody, pending.RemoteETag, false)
	})
	if err != nil {
		if status := retryStatus(err); status != nil {
			return errors.Join(err, s.store.RollbackCardDAVPublicationThrottleAuthorizedContext(operationCtx, pending, time.Now().Add(status.RetryAfter).UTC(), authorize))
		}
		if isStatus(err, http.StatusPreconditionFailed) {
			return errors.Join(ErrScopedPublicationReconciliationRequired, err)
		}
		if isDefinitiveMutationRejection(err) {
			return errors.Join(err, s.store.RollbackCardDAVPublicationAuthorizedContext(operationCtx, pending, authorize))
		}
		return err
	}
	return s.commitScopedCanonical(operationCtx, pending, authorize)
}

// RecoverPublicationAuthorized observes the exact pending update. It never
// sends another PUT or invokes owner conflict resolution.
func (s *Service) RecoverPublicationAuthorized(ctx context.Context, expected store.CardDAVPublication, authorize store.PersonEditAuthorizer) error {
	if err := s.validateScopedPublication(authorize); err != nil {
		return err
	}
	if expected.PendingOperation != store.CardDAVMutationUpdate || expected.PendingIntentID == "" || expected.ConflictOwned || expected.ResolutionConflictID != 0 {
		return store.ErrCardDAVInvalidPlan
	}
	release, err := s.store.AcquireCardDAVPersonOperation(ctx, expected.PersonID)
	if err != nil {
		return err
	}
	defer release()
	return s.recoverPublicationAuthorizedUnderLease(ctx, expected, authorize)
}

// The caller owns the native person-operation lease.
func (s *Service) recoverPublicationAuthorizedUnderLease(ctx context.Context, expected store.CardDAVPublication, authorize store.PersonEditAuthorizer) error {
	operationCtx, cancel := context.WithTimeout(ctx, s.operationTimeout())
	defer cancel()
	if err := s.requireOwnBook(operationCtx, expected.AddressBookID); err != nil {
		return err
	}
	pending, err := s.store.RefreshCardDAVPublicationFenceAuthorizedContext(operationCtx, expected, authorize)
	if err != nil {
		return err
	}
	operationCtx = s.withPublicationRequestAuthority(operationCtx, *pending, authorize)
	return s.commitScopedCanonical(operationCtx, pending, authorize)
}

func (s *Service) commitScopedCanonical(ctx context.Context, pending *store.CardDAVPublication, authorize store.PersonEditAuthorizer) error {
	remote, tombstone, err := s.fetchCanonical(ctx, pending.Href)
	if err != nil {
		return err
	}
	err = s.store.CommitCardDAVPublicationAuthorizedContext(ctx, store.CardDAVCanonicalMutation{Publication: *pending, Remote: remote, Tombstone: tombstone}, authorize)
	if errors.Is(err, store.ErrCardDAVPublicationMismatch) {
		return errors.Join(ErrScopedPublicationReconciliationRequired, err)
	}
	return err
}
