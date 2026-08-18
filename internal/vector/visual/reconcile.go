package visual

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

const defaultReconcilePageSize = 250

type ReconcileConfig struct {
	GenerationID  int64
	ConsumerKey   string
	MessageTypes  []string
	PageSize      int
	LeaseOwner    string
	LeaseDuration time.Duration
	MediaPolicy   MediaPolicy
	ContextPolicy ContextPolicy
	Now           func() time.Time
}

type WorkItem struct {
	Candidate store.VisualCandidate
	Document  DocumentInput
	Claim     store.VisualWorkClaim
}

type ReconcileResult struct {
	SourceFence     int64
	CandidateOwners int64
	Claimed         int64
	AlreadyCurrent  int64
	ActiveClaims    int64
	Terminal        int64
	Retryable       int64
	Tombstoned      int64
	Work            []WorkItem
}

type Reconciler struct {
	archive *store.Store
	opener  StreamOpener
	config  ReconcileConfig
}

// Activate makes the reconciler's generation searchable only after callers
// have observed a fully drained reconciliation and journal.
func (r *Reconciler) Activate(ctx context.Context) error {
	highWater, err := r.archive.AttachmentChangeHighWater(ctx)
	if err != nil {
		return err
	}
	return r.archive.ActivateVisualGeneration(ctx, r.config.GenerationID, highWater)
}

func (r *Reconciler) ObsoleteTokens(ctx context.Context, limit int) ([]string, error) {
	return r.archive.ListObsoleteVisualTokens(ctx, r.config.GenerationID, limit)
}

func (r *Reconciler) ClearObsoleteToken(ctx context.Context, token string) error {
	return r.archive.ClearObsoleteVisualToken(ctx, r.config.GenerationID, token)
}

func (r *Reconciler) NeedsFullReconcile(ctx context.Context) (bool, error) {
	consumer, _, err := r.archive.RegisterAttachmentChangeConsumer(ctx, r.config.ConsumerKey)
	if err != nil {
		return false, err
	}
	return !consumer.ReconciliationComplete, nil
}

// RetryOwner re-evaluates one bounded owner without resetting the generation's
// full-reconciliation cursor. It is the operator path for retryable provider
// failures and explicit re-attempts of terminal provider decisions.
func (r *Reconciler) RetryOwner(ctx context.Context, messageID int64, blobHash string) (ReconcileResult, error) {
	blobHash = strings.ToLower(strings.TrimSpace(blobHash))
	if messageID <= 0 || len(blobHash) != 64 {
		return ReconcileResult{}, errors.New("visual retry requires a positive message ID and SHA-256 hash")
	}
	if _, err := hex.DecodeString(blobHash); err != nil {
		return ReconcileResult{}, errors.New("visual retry requires a valid SHA-256 hash")
	}
	highWater, err := r.archive.AttachmentChangeHighWater(ctx)
	if err != nil {
		return ReconcileResult{}, err
	}
	page, err := r.archive.ListVisualCandidates(ctx, store.VisualCandidateFilter{
		MessageIDs: []int64{messageID}, MessageTypes: r.config.MessageTypes,
	})
	if err != nil {
		return ReconcileResult{}, err
	}
	candidates := make([]store.VisualCandidate, 0, 1)
	for _, candidate := range page.Candidates {
		if strings.EqualFold(candidate.Owner.BlobHash, blobHash) {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return ReconcileResult{}, store.ErrVisualOwnerMissing
	}
	result := ReconcileResult{SourceFence: highWater, Work: make([]WorkItem, 0, 1)}
	if err := r.reconcileCandidates(ctx, candidates, highWater, &result); err != nil {
		return ReconcileResult{}, err
	}
	return result, nil
}

func (r *Reconciler) Retire(ctx context.Context) error {
	if err := r.archive.RetireVisualGeneration(ctx, r.config.GenerationID); err != nil {
		return err
	}
	return r.archive.UnregisterAttachmentChangeConsumer(ctx, r.config.ConsumerKey)
}

func (r *Reconciler) GenerationTokens(ctx context.Context) ([]string, error) {
	return r.archive.ListVisualGenerationTokens(ctx, r.config.GenerationID)
}

func NewReconciler(archive *store.Store, opener StreamOpener, config ReconcileConfig) (*Reconciler, error) {
	if archive == nil || opener == nil {
		return nil, errors.New("visual reconciler requires archive and stream opener")
	}
	if config.GenerationID <= 0 || strings.TrimSpace(config.ConsumerKey) == "" ||
		strings.TrimSpace(config.LeaseOwner) == "" || config.LeaseDuration <= 0 {
		return nil, errors.New("invalid visual reconcile configuration")
	}
	if config.PageSize == 0 {
		config.PageSize = defaultReconcilePageSize
	}
	if config.PageSize < 1 || config.PageSize > 1000 {
		return nil, errors.New("visual reconcile page size must be between 1 and 1000")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Reconciler{archive: archive, opener: opener, config: config}, nil
}

// FullReconcile establishes the consumer baseline on first use, schedules the
// authoritative owner set, and tombstones publications no longer represented
// by a live qualifying occurrence. Changes racing the scan remain after the
// baseline and are repaired by Replay.
func (r *Reconciler) FullReconcile(ctx context.Context) (ReconcileResult, error) {
	consumer, _, err := r.archive.RegisterAttachmentChangeConsumer(ctx, r.config.ConsumerKey)
	if err != nil {
		return ReconcileResult{}, err
	}
	sourceFence := consumer.BaselineSequence
	if consumer.ReconciliationComplete {
		sourceFence, err = r.archive.AttachmentChangeHighWater(ctx)
		if err != nil {
			return ReconcileResult{}, err
		}
	}
	result := ReconcileResult{SourceFence: sourceFence, Work: make([]WorkItem, 0)}
	for after := int64(0); ; {
		page, pageErr := r.archive.ListVisualCandidates(ctx, store.VisualCandidateFilter{
			AfterMessageID: after, LimitMessages: r.config.PageSize,
			MessageTypes: r.config.MessageTypes,
		})
		if pageErr != nil {
			return ReconcileResult{}, pageErr
		}
		if err := r.reconcileCandidates(ctx, page.Candidates, sourceFence, &result); err != nil {
			return ReconcileResult{}, err
		}
		// Return one bounded page of claimed work to the caller for immediate
		// provider execution. Restarting the scan is safe: published owners take
		// the AlreadyCurrent path, while claims prevent duplicate requests. This
		// keeps media bytes and lease age bounded by PageSize instead of archive
		// size.
		if len(result.Work) > 0 {
			return result, nil
		}
		if !page.HasMore {
			break
		}
		after = page.NextAfterMessageID
	}
	if err := r.tombstoneMissingPublications(ctx, sourceFence, &result); err != nil {
		return ReconcileResult{}, err
	}
	if !consumer.ReconciliationComplete {
		if err := r.archive.CompleteAttachmentChangeReconciliation(
			ctx, consumer.ConsumerKey, consumer.BaselineSequence,
		); err != nil {
			return ReconcileResult{}, err
		}
	}
	if err := r.archive.AdvanceVisualGenerationSourceFence(ctx, r.config.GenerationID, sourceFence); err != nil {
		return ReconcileResult{}, err
	}
	return result, nil
}

// Replay consumes journal pages only after every source decision in the page
// is durable. An error leaves the cursor untouched, so the complete page is
// retried at least once.
func (r *Reconciler) Replay(ctx context.Context) (ReconcileResult, error) {
	result := ReconcileResult{Work: make([]WorkItem, 0)}
	for {
		changes, err := r.archive.ListAttachmentChanges(ctx, r.config.ConsumerKey, r.config.PageSize)
		if err != nil {
			return ReconcileResult{}, err
		}
		if len(changes) == 0 {
			return result, nil
		}
		messageIDs := affectedMessageIDs(changes)
		sourceFence := changes[len(changes)-1].Sequence
		candidates, err := r.archive.ListVisualCandidates(ctx, store.VisualCandidateFilter{
			MessageIDs: messageIDs, MessageTypes: r.config.MessageTypes,
		})
		if err != nil {
			return ReconcileResult{}, err
		}
		if err := r.reconcileCandidates(ctx, candidates.Candidates, sourceFence, &result); err != nil {
			return ReconcileResult{}, err
		}
		if err := r.tombstoneMissingForMessages(ctx, messageIDs, candidates.Candidates, sourceFence, &result); err != nil {
			return ReconcileResult{}, err
		}
		// Do not acknowledge source events merely because work was claimed.
		// Publication or a durable rejection happens in the worker; the next
		// replay observes that decision and only then advances this page.
		if len(result.Work) > 0 {
			result.SourceFence = sourceFence
			return result, nil
		}
		if err := r.archive.AdvanceAttachmentChangeConsumer(ctx, r.config.ConsumerKey, sourceFence); err != nil {
			return ReconcileResult{}, err
		}
		if err := r.archive.AdvanceVisualGenerationSourceFence(ctx, r.config.GenerationID, sourceFence); err != nil {
			return ReconcileResult{}, err
		}
		result.SourceFence = sourceFence
	}
}

func (r *Reconciler) reconcileCandidates(
	ctx context.Context,
	candidates []store.VisualCandidate,
	sourceFence int64,
	result *ReconcileResult,
) error {
	for _, candidate := range candidates {
		result.CandidateOwners++
		eligibility, err := InspectMedia(ctx, r.opener, Occurrence{
			MessageID: candidate.Owner.MessageID, BlobHash: candidate.Owner.BlobHash,
			DeclaredMIME: candidate.DeclaredMIME, Role: candidate.Role, RoleSource: candidate.RoleSource,
		}, r.config.MediaPolicy)
		if err != nil {
			return fmt.Errorf("inspect visual candidate for message %d: %w",
				candidate.Owner.MessageID, err)
		}
		owner := visualOwner(candidate.Owner)
		if !eligibility.Eligible {
			revision := rejectionRevision(candidate, eligibility.Reason, r.config)
			kind := "terminal"
			if eligibility.Reason == ReasonContentUnavailable {
				kind = "retryable"
			}
			if kind == "terminal" {
				publication, getErr := r.archive.GetVisualPublication(ctx, r.config.GenerationID, candidate.Owner)
				if getErr == nil && publication.PreparedRevision == revision &&
					publication.OutcomeKind == kind && publication.OutcomeReason == string(eligibility.Reason) {
					result.Terminal++
					continue
				}
				if getErr != nil && !errors.Is(getErr, sql.ErrNoRows) {
					return getErr
				}
			}
			claim, acquired, claimErr := r.claim(ctx, candidate.Owner, revision, sourceFence)
			if claimErr != nil {
				return claimErr
			}
			if !acquired {
				result.ActiveClaims++
				continue
			}
			if err := r.archive.RejectVisualPublication(ctx, claim, store.VisualOutcome{
				Kind: kind, Reason: string(eligibility.Reason),
			}); err != nil {
				return err
			}
			if kind == "terminal" {
				result.Terminal++
			} else {
				result.Retryable++
			}
			continue
		}
		messageContext, err := r.archive.GetVisualMessageContext(ctx, candidate.Owner.MessageID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		document, _, err := AssembleDocument(AssembleRequest{
			Owner: owner, Media: eligibility.Media,
			Context: MessageContext{
				Subject: messageContext.Subject, Body: messageContext.Body,
				MessageType: messageContext.MessageType, Filename: candidate.Filename,
			},
			Role: candidate.Role, SourceSequence: sourceFence, Policy: r.config.ContextPolicy,
		})
		if err != nil {
			return err
		}
		publication, getErr := r.archive.GetVisualPublication(ctx, r.config.GenerationID, candidate.Owner)
		if getErr == nil && publication.State == store.VisualPublicationCurrent &&
			publication.PublishedRevision == document.Revision {
			result.AlreadyCurrent++
			continue
		}
		if getErr != nil && !errors.Is(getErr, sql.ErrNoRows) {
			return getErr
		}
		claim, acquired, err := r.claim(ctx, candidate.Owner, document.Revision, sourceFence)
		if err != nil {
			return err
		}
		if !acquired {
			result.ActiveClaims++
			continue
		}
		if getErr == nil && publication.PublishedRevision == document.Revision &&
			publication.CurrentVectorToken != "" {
			err := r.archive.RestoreVisualPublication(ctx, store.PreparedVisualPublication{
				Claim: claim, RepresentativeAttachmentID: candidate.RepresentativeAttachmentID,
				Role: candidate.Role, RoleSource: candidate.RoleSource,
			}, publication.CurrentVectorToken)
			if err == nil {
				result.AlreadyCurrent++
				continue
			}
			if !errors.Is(err, store.ErrVisualSourceChanged) &&
				!errors.Is(err, store.ErrVisualClaimLost) &&
				!errors.Is(err, store.ErrVisualOwnerMissing) {
				return err
			}
			continue
		}
		result.Claimed++
		result.Work = append(result.Work, WorkItem{Candidate: candidate, Document: document, Claim: claim})
	}
	return nil
}

func (r *Reconciler) claim(
	ctx context.Context,
	owner store.VisualOwner,
	revision string,
	sourceFence int64,
) (store.VisualWorkClaim, bool, error) {
	return r.archive.ClaimVisualWork(ctx, store.VisualClaimRequest{
		GenerationID: r.config.GenerationID, Owner: owner, ProposedRevision: revision,
		LeaseOwner: r.config.LeaseOwner, Now: r.config.Now(),
		LeaseDuration: r.config.LeaseDuration, SourceFence: sourceFence,
	})
}

func (r *Reconciler) tombstoneMissingPublications(
	ctx context.Context,
	sourceFence int64,
	result *ReconcileResult,
) error {
	for after := int64(0); ; {
		page, err := r.archive.ListVisualPublications(ctx, r.config.GenerationID, store.VisualPublicationFilter{
			AfterMessageID: after, LimitMessages: r.config.PageSize,
		})
		if err != nil {
			return err
		}
		messageIDs := publicationMessageIDs(page.Publications)
		var candidates store.VisualCandidatePage
		if len(messageIDs) > 0 {
			candidates, err = r.archive.ListVisualCandidates(ctx, store.VisualCandidateFilter{
				MessageIDs: messageIDs, MessageTypes: r.config.MessageTypes,
			})
			if err != nil {
				return err
			}
		}
		if err := r.tombstonePublications(ctx, page.Publications, candidates.Candidates, sourceFence, result); err != nil {
			return err
		}
		if !page.HasMore {
			return nil
		}
		after = page.NextAfterMessageID
	}
}

func (r *Reconciler) tombstoneMissingForMessages(
	ctx context.Context,
	messageIDs []int64,
	candidates []store.VisualCandidate,
	sourceFence int64,
	result *ReconcileResult,
) error {
	if len(messageIDs) == 0 {
		return nil
	}
	page, err := r.archive.ListVisualPublications(ctx, r.config.GenerationID, store.VisualPublicationFilter{
		MessageIDs: messageIDs,
	})
	if err != nil {
		return err
	}
	return r.tombstonePublications(ctx, page.Publications, candidates, sourceFence, result)
}

func (r *Reconciler) tombstonePublications(
	ctx context.Context,
	publications []store.VisualPublication,
	candidates []store.VisualCandidate,
	sourceFence int64,
	result *ReconcileResult,
) error {
	desired := make(map[store.VisualOwner]struct{}, len(candidates))
	for _, candidate := range candidates {
		desired[candidate.Owner] = struct{}{}
	}
	for _, publication := range publications {
		if _, exists := desired[publication.Owner]; exists {
			continue
		}
		if publication.State == store.VisualPublicationTombstoned && publication.SourceFence >= sourceFence {
			continue
		}
		if err := r.archive.TombstoneVisualOwner(
			ctx, r.config.GenerationID, publication.Owner, sourceFence,
		); err != nil {
			return err
		}
		result.Tombstoned++
	}
	return nil
}

func affectedMessageIDs(changes []store.AttachmentChange) []int64 {
	seen := make(map[int64]struct{})
	result := make([]int64, 0)
	for _, change := range changes {
		for _, id := range []*int64{change.OldMessageID, change.NewMessageID} {
			if id == nil || *id <= 0 {
				continue
			}
			if _, exists := seen[*id]; exists {
				continue
			}
			seen[*id] = struct{}{}
			result = append(result, *id)
		}
	}
	return result
}

func publicationMessageIDs(publications []store.VisualPublication) []int64 {
	result := make([]int64, 0)
	var previous int64
	for _, publication := range publications {
		if publication.Owner.MessageID == previous {
			continue
		}
		previous = publication.Owner.MessageID
		result = append(result, previous)
	}
	return result
}

func visualOwner(owner store.VisualOwner) Owner {
	return Owner{MessageID: owner.MessageID, BlobHash: owner.BlobHash, MediaInputKey: owner.MediaInputKey}
}

func rejectionRevision(candidate store.VisualCandidate, reason EligibilityReason, config ReconcileConfig) string {
	hash := sha256.New()
	for _, value := range []string{
		config.ContextPolicy.InputVersion,
		config.ContextPolicy.EligibilityVersion,
		candidate.Owner.BlobHash,
		candidate.Owner.MediaInputKey,
		string(candidate.Role),
		string(candidate.RoleSource),
		candidate.DeclaredMIME,
		strconv.FormatInt(candidate.Size, 10),
		strconv.FormatInt(candidate.Width, 10),
		strconv.FormatInt(candidate.Height, 10),
		strconv.FormatInt(candidate.DurationMS, 10),
		strconv.FormatInt(config.MediaPolicy.MaxBytes, 10),
		strconv.FormatInt(config.MediaPolicy.MaxPixels, 10),
		strconv.FormatBool(config.MediaPolicy.IncludeImages),
		strconv.FormatBool(config.MediaPolicy.IncludeVideo),
		strconv.FormatBool(config.MediaPolicy.AllowAnimatedGIF),
		string(reason),
	} {
		writeRevisionField(hash, value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
