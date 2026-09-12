package carddav

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
)

// observePendingCreateUnlocked always reads the canonical href before offering
// authorization or cancellation. An existing resource is settled or conflicted.
func (s *Service) observePendingCreateUnlocked(ctx context.Context, pending *store.CardDAVPublication) (bool, error) {
	remote, absent, err := s.fetchCanonical(ctx, pending.Href)
	if err != nil {
		return false, err
	}
	if absent {
		return true, nil
	}
	err = s.commitCardDAVCanonicalMutation(ctx, store.CardDAVCanonicalMutation{Publication: *pending, Remote: remote})
	if errors.Is(err, store.ErrCardDAVPublicationMismatch) {
		err = s.captureCardDAVCreateConflict(ctx, pending, remote)
	}
	return false, err
}

func (s *Service) pendingPublicationSourceUnlocked(ctx context.Context, personID int64) (*store.CardDAVPublicationReviewSource, error) {
	pending, err := s.store.RefreshCardDAVPublicationFenceContext(ctx, personID)
	if err != nil {
		return nil, err
	}
	if pending.PendingOperation != store.CardDAVMutationCreate {
		return nil, store.ErrCardDAVPublicationPending
	}
	absent, err := s.observePendingCreateUnlocked(ctx, pending)
	if err != nil {
		return nil, err
	}
	if !absent {
		return nil, nil //nolint:nilnil // (nil, nil) means the pending create already exists remotely; callers branch on the nil source
	}
	source, err := s.store.LoadCardDAVPublicationReviewSourceContext(ctx, personID)
	if err != nil {
		return nil, err
	}
	if source.Conflict != nil {
		return nil, &ConflictError{ID: source.Conflict.ID}
	}
	if source.Publication == nil || source.Publication.MutationRevision != pending.MutationRevision {
		return nil, store.ErrCardDAVReviewStale
	}
	return source, nil
}

func pendingPublicationPreview(source *store.CardDAVPublicationReviewSource) (*PublicationPreview, error) {
	if err := checkPublicationPreviewSize(source.Publication.OutgoingBody); err != nil {
		return nil, err
	}
	fence := store.CardDAVPendingReviewFence(source)
	reviewRequired := !source.Publication.HasExactBodyApproval() ||
		source.Publication.LocalHash != source.Snapshot.Fingerprint ||
		source.Inference.ReviewRequired(source.ConnectionGeneration, source.Book.ID)
	return &PublicationPreview{PersonID: source.Person.ID, AddressBook: publicAddressBookIdentity(source.Book.ID, source.Book.DisplayName), Kind: PublicationReviewPending, VCard: string(source.Publication.OutgoingBody), ApprovalToken: store.CardDAVReviewToken(fence), ReviewRequired: reviewRequired}, nil
}

func (s *Service) publishReviewedPendingUnlocked(ctx context.Context, personID int64, token string) error {
	source, err := s.pendingPublicationSourceUnlocked(ctx, personID)
	if err != nil {
		return reviewArtifactSourceError(err)
	}
	if source == nil {
		return store.ErrCardDAVReviewStale
	}
	fence := store.CardDAVPendingReviewFence(source)
	if token != store.CardDAVReviewToken(fence) {
		return store.ErrCardDAVReviewStale
	}
	pending, err := s.store.ApprovePendingCardDAVCreateContext(ctx, store.CardDAVPendingCreateApprovalPlan{Fence: fence, Body: source.Publication.OutgoingBody, ApprovalToken: token})
	if err != nil {
		return err
	}
	pending.RecoveryOnly = true
	return s.executeMutation(ctx, pending)
}

func (s *Service) cancelPendingCreateUnlocked(ctx context.Context, pending *store.CardDAVPublication) error {
	pending, err := s.store.RefreshCardDAVPublicationFenceContext(ctx, pending.PersonID)
	if err != nil {
		return err
	}
	absent, err := s.observePendingCreateUnlocked(ctx, pending)
	if err != nil {
		return err
	}
	if absent {
		return s.store.CancelAbsentCardDAVCreateContext(ctx, *pending)
	}
	return s.mutateUnlocked(ctx, Mutation{PersonID: pending.PersonID, Desired: false})
}
