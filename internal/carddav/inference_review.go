package carddav

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

type PublicationReviewKind = store.CardDAVReviewArtifactKind

const (
	PublicationReviewCurrent   = store.CardDAVReviewCurrent
	PublicationReviewPending   = store.CardDAVReviewPending
	PublicationReviewConflict  = store.CardDAVReviewConflict
	MaxPublicationPreviewBytes = 32 * 1024 * 1024
)

var ErrCardDAVPreviewTooLarge = errors.New("CardDAV publication preview exceeds 32 MiB")

type PublicationPreview struct {
	PersonID       int64
	AddressBook    AddressBookIdentity
	Kind           PublicationReviewKind
	VCard          string
	ApprovalToken  string
	ReviewRequired bool
	ConflictID     *int64
}

func (s *Service) PreviewPublication(ctx context.Context, personID int64) (*PublicationPreview, error) {
	if s == nil || s.store == nil || s.client == nil {
		return nil, errors.New("CardDAV service is not configured")
	}
	release, err := s.store.AcquireCardDAVPersonOperation(ctx, personID)
	if err != nil {
		return nil, err
	}
	defer release()
	initial, err := s.store.LoadCardDAVPublicationReviewSourceContext(ctx, personID)
	if err != nil {
		return nil, err
	}
	if initial.Conflict != nil {
		return s.previewConflictPublicationUnlocked(ctx, initial.Conflict.ID)
	}
	if initial.Publication != nil && initial.Publication.PendingOperation != "" {
		pendingSource, err := s.pendingPublicationSourceUnlocked(ctx, personID)
		if err != nil {
			return nil, err
		}
		if pendingSource != nil {
			return pendingPublicationPreview(pendingSource)
		}
	}
	source, plan, err := s.currentPublicationPlan(ctx, personID)
	if err != nil {
		return nil, err
	}
	fence := store.CardDAVCurrentReviewFence(source, plan.OutgoingBody, plan.Href)
	return &PublicationPreview{
		PersonID: personID, AddressBook: publicAddressBookIdentity(source.Book.ID, source.Book.DisplayName),
		Kind: PublicationReviewCurrent, VCard: string(plan.OutgoingBody), ApprovalToken: store.CardDAVReviewToken(fence),
		ReviewRequired: source.Inference.ReviewRequired(source.ConnectionGeneration, source.Book.ID),
	}, nil
}

func (s *Service) PublishReviewedPerson(ctx context.Context, personID int64, token string) error {
	if token == "" {
		return s.PublishPerson(ctx, personID)
	}
	if s == nil || s.store == nil || s.client == nil {
		return errors.New("CardDAV service is not configured")
	}
	release, err := s.store.AcquireCardDAVPersonOperation(ctx, personID)
	if err != nil {
		return err
	}
	defer release()
	operationCtx, cancel := context.WithTimeout(ctx, s.client.operationTimeout)
	defer cancel()
	existing, err := s.store.GetCardDAVPublicationContext(operationCtx, personID)
	if err == nil && existing.PendingOperation == store.CardDAVMutationCreate {
		return s.publishReviewedPendingUnlocked(operationCtx, personID, token)
	}
	if err != nil && !errors.Is(err, store.ErrCardDAVPublicationNotFound) {
		return err
	}
	source, err := s.store.LoadCardDAVPublicationReviewSourceContext(operationCtx, personID)
	if err != nil {
		return reviewArtifactSourceError(err)
	}
	if source.Conflict != nil {
		return s.approveConflictPublicationUnlocked(operationCtx, source.Conflict.ID, token)
	}
	return s.publishCurrentUnlocked(operationCtx, personID, token)
}

func (s *Service) currentPublicationPlan(ctx context.Context, personID int64) (*store.CardDAVPublicationReviewSource, store.CardDAVPublicationPlan, error) {
	var plan store.CardDAVPublicationPlan
	source, err := s.store.LoadCardDAVPublicationReviewSourceContext(ctx, personID)
	if err != nil {
		return nil, plan, err
	}
	if source.Conflict != nil {
		return nil, plan, &ConflictError{ID: source.Conflict.ID}
	}
	if source.Publication != nil && source.Publication.PendingOperation != "" {
		return nil, plan, store.ErrCardDAVPublicationPending
	}
	href, err := s.publicationHref(source.Book.CanonicalURL, source.Person.VCardUID)
	if err != nil {
		return nil, plan, err
	}
	if source.Resource != nil {
		href = source.Resource.Href
	}
	envelope, err := s.preparePublicationEnvelope(source)
	body, hash := envelope.StoredBody, source.Snapshot.Fingerprint
	if err != nil {
		return nil, plan, err
	}
	if err := checkPublicationPreviewSize(body); err != nil {
		return nil, plan, err
	}
	semanticHash, err := SemanticHash(body)
	if err != nil {
		return nil, plan, err
	}
	plan = store.CardDAVPublicationPlan{PersonID: personID, Desired: true, AddressBookID: source.Book.ID,
		Href: href, OutgoingBody: body, OutgoingSemanticHash: semanticHash, LocalHash: hash}
	fence := store.CardDAVCurrentReviewFence(source, body, href)
	plan.SourceFence = &fence
	plan.OutgoingEnvelopeMetadata, err = vcard.MarshalResourceMetadata(envelope)
	if err != nil {
		return nil, plan, err
	}

	return source, plan, nil
}

func (s *Service) publishCurrentUnlocked(ctx context.Context, personID int64, token string) error {
	source, plan, err := s.currentPublicationPlan(ctx, personID)
	if err != nil {
		if token != "" && (errors.Is(err, store.ErrCardDAVNoWriteTarget) || errors.Is(err, store.ErrCardDAVAddressBookNotFound) || errors.Is(err, ErrCardDAVConflictPending) || errors.Is(err, store.ErrCardDAVPublicationPending)) {
			return store.ErrCardDAVReviewStale
		}
		return err
	}
	var prepared *store.CardDAVPublication
	if token == "" {
		prepared, err = s.store.PrepareCardDAVPublicationContext(ctx, plan)
	} else {
		fence := store.CardDAVCurrentReviewFence(source, plan.OutgoingBody, plan.Href)
		if store.CardDAVReviewToken(fence) != token {
			return store.ErrCardDAVReviewStale
		}
		prepared, err = s.store.PrepareReviewedCardDAVPublicationContext(ctx, store.CardDAVReviewedPublicationPlan{
			Publication: plan, Fence: fence, ApprovalToken: token,
		})
	}
	if err != nil {
		return err
	}
	if prepared.Noop {
		return nil
	}
	return s.executeMutation(ctx, prepared)
}

func checkPublicationPreviewSize(body []byte) error {
	if len(body) > MaxPublicationPreviewBytes {
		return ErrCardDAVPreviewTooLarge
	}
	return nil
}

// Artifact selection is an early check; supplied tokens retain the same stale
// error contract when their captured target or artifact has disappeared.
func reviewArtifactSourceError(err error) error {
	if errors.Is(err, store.ErrCardDAVNoWriteTarget) || errors.Is(err, store.ErrCardDAVAddressBookNotFound) || errors.Is(err, store.ErrCardDAVConflictStale) || errors.Is(err, store.ErrCardDAVStalePlan) || errors.Is(err, store.ErrCardDAVPublicationNotFound) || errors.Is(err, store.ErrCardDAVPublicationPending) {
		return store.ErrCardDAVReviewStale
	}
	return err
}
