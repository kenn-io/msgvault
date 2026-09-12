package carddav

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

type conflictGuardKey struct{}

func (s *Service) conflictPersonOperation(ctx context.Context, conflictID int64) (context.Context, func(), error) {
	conflict, err := s.store.GetCardDAVConflictContext(ctx, conflictID)
	if err != nil {
		return ctx, nil, err
	}
	mapping, err := s.store.GetCardDAVResourceContext(ctx, conflict.AddressBookID, conflict.Href)
	if err != nil {
		return ctx, nil, err
	}
	if s.conflictOperationMappingReadHook != nil {
		s.conflictOperationMappingReadHook()
	}
	if mapping.PersonID == nil {
		return context.WithValue(ctx, conflictGuardKey{}, int64(0)), func() {}, nil
	}
	release, err := s.store.AcquireCardDAVPersonOperation(ctx, *mapping.PersonID)
	if err != nil {
		return ctx, nil, err
	}
	current, err := s.store.GetCardDAVResourceContext(ctx, conflict.AddressBookID, conflict.Href)
	if err != nil || current.PersonID == nil || *current.PersonID != *mapping.PersonID || current.MappingRevision != mapping.MappingRevision {
		release()
		return ctx, nil, store.ErrCardDAVReviewStale
	}
	return context.WithValue(ctx, conflictGuardKey{}, *mapping.PersonID), release, nil
}

func (s *Service) PreviewConflictPublication(ctx context.Context, conflictID int64) (*PublicationPreview, error) {
	if s == nil || s.store == nil || s.client == nil {
		return nil, errors.New("CardDAV service is not configured")
	}
	ctx, release, err := s.conflictPersonOperation(ctx, conflictID)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.previewConflictPublicationUnlocked(ctx, conflictID)
}

func (s *Service) conflictPublicationPlanUnlocked(ctx context.Context, conflictID int64) (*store.CardDAVPublicationReviewSource, store.CardDAVConflictLocalApprovalPlan, error) {
	var plan store.CardDAVConflictLocalApprovalPlan
	source, err := s.store.LoadCardDAVConflictReviewSourceContext(ctx, conflictID)
	if err != nil {
		return nil, plan, err
	}
	if personID, ok := ctx.Value(conflictGuardKey{}).(int64); ok && source.Person.ID != personID {
		return nil, plan, store.ErrCardDAVReviewStale
	}
	if len(source.Conflict.LocalMutationIntent) > 0 || source.Publication != nil && source.Publication.PendingOperation != "" {
		return nil, plan, store.ErrCardDAVPublicationPending
	}
	envelope, err := s.preparePublicationEnvelope(source)
	if err != nil {
		return nil, plan, err
	}
	if err := checkPublicationPreviewSize(envelope.StoredBody); err != nil {
		return nil, plan, err
	}
	metadata, err := vcard.MarshalResourceMetadata(envelope)
	if err != nil {
		return nil, plan, err
	}
	plan.Body, plan.EnvelopeMetadata = envelope.StoredBody, metadata
	plan.Fence = store.CardDAVConflictReviewFence(source, plan.Body)
	plan.ApprovalToken = store.CardDAVReviewToken(plan.Fence)
	return source, plan, nil
}

func (s *Service) previewConflictPublicationUnlocked(ctx context.Context, conflictID int64) (*PublicationPreview, error) {
	source, plan, err := s.conflictPublicationPlanUnlocked(ctx, conflictID)
	if err != nil {
		return nil, err
	}
	return &PublicationPreview{PersonID: source.Person.ID, AddressBook: publicAddressBookIdentity(source.Book.ID, source.Book.DisplayName), Kind: PublicationReviewConflict, VCard: string(plan.Body), ApprovalToken: plan.ApprovalToken, ConflictID: plan.Fence.ConflictID, ReviewRequired: source.Inference.InferenceRevision > 0 && !source.Conflict.HasExactLocalApproval(source.Inference, source.ConnectionGeneration, source.Book.SyncRevision)}, nil
}

func (s *Service) ApproveConflictPublication(ctx context.Context, conflictID int64, token string) error {
	if s == nil || s.store == nil || s.client == nil {
		return errors.New("CardDAV service is not configured")
	}
	ctx, release, err := s.conflictPersonOperation(ctx, conflictID)
	if err != nil {
		return err
	}
	defer release()
	return s.approveConflictPublicationUnlocked(ctx, conflictID, token)
}

func (s *Service) approveConflictPublicationUnlocked(ctx context.Context, conflictID int64, token string) error {
	_, plan, err := s.conflictPublicationPlanUnlocked(ctx, conflictID)
	if err != nil {
		return reviewArtifactSourceError(err)
	}
	if plan.ApprovalToken != token {
		return store.ErrCardDAVReviewStale
	}
	return s.store.ApproveCardDAVConflictLocalContext(ctx, plan)
}
