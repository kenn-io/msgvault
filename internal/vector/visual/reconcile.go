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
	GenerationID int64
	ConsumerKey  string
	MessageTypes []string
	// SourceIDs restricts the lane to these accounts; empty means every
	// account. Resolved from [vector.multimodal.scope] before construction.
	SourceIDs []int64
	PageSize  int
	// MaxScanPagesPerPass bounds how many candidate or journal pages one
	// FullReconcile or Replay invocation walks. Every candidate inspection
	// reads its blob, so an archive dominated by current, terminal, or
	// unavailable media would otherwise scan the whole corpus in a single
	// pass and monopolize the scheduler. Zero means the default of 50; the
	// cursor and journal consumer resume where the pass stopped, and
	// activation stays fenced until the scan genuinely completes.
	MaxScanPagesPerPass int
	LeaseOwner          string
	LeaseDuration       time.Duration
	MediaPolicy         MediaPolicy
	ContextPolicy       ContextPolicy
	Now                 func() time.Time
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
	// scanCursor resumes FullReconcile's bounded candidate scan across
	// passes so each pass advances the frontier instead of rescanning (and
	// re-reading the blobs of) everything already processed. In-memory:
	// a restart costs one linear catch-up scan, never quadratic work.
	scanCursor int64
}

// Activate makes the reconciler's generation searchable only after callers
// have observed a fully drained reconciliation and journal.
// Activate promotes this generation and returns the generations the swap
// retired, so the caller can delete their backend vectors and unregister
// their journal consumers — a retired generation left registered would pin
// journal pruning forever and its vectors would accumulate unreachably.
func (r *Reconciler) Activate(ctx context.Context) ([]store.VisualGeneration, error) {
	highWater, err := r.archive.AttachmentChangeHighWater(ctx)
	if err != nil {
		return nil, err
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
		MessageIDs: []int64{messageID}, MessageTypes: r.config.MessageTypes, SourceIDs: r.config.SourceIDs,
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
	// An explicit retry must bypass terminal convergence: clear the durable
	// outcome first so reconciliation re-claims the owner instead of
	// treating the recorded decision as converged and doing nothing.
	for _, candidate := range candidates {
		if err := r.archive.ClearVisualOutcome(ctx, r.config.GenerationID, candidate.Owner); err != nil {
			return ReconcileResult{}, err
		}
	}
	result := ReconcileResult{SourceFence: highWater, Work: make([]WorkItem, 0, 1)}
	if err := r.reconcileCandidates(ctx, candidates, highWater, &result, false); err != nil {
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

// otherWorkersActive reports whether any unexpired claim exists beyond the
// ones this pass just created. Completing the baseline or acknowledging a
// journal page while another worker holds claims would let a crash plus
// lease expiry orphan that work permanently: the journal would already be
// consumed and no full reconcile would be owed.
func (r *Reconciler) otherWorkersActive(ctx context.Context, result *ReconcileResult) (bool, error) {
	active, err := r.archive.CountActiveVisualClaims(ctx, r.config.GenerationID, r.config.Now())
	if err != nil {
		return false, err
	}
	return active > result.Claimed, nil
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
	maxPages := r.maxScanPages()
	pages := 0
	for after := r.scanCursor; ; {
		if pages >= maxPages {
			// Bounded pass: resume from the cursor next time. Completion,
			// tombstoning, and the fence advance only happen when the scan
			// truly reaches the end.
			return result, nil
		}
		pages++
		page, pageErr := r.archive.ListVisualCandidates(ctx, store.VisualCandidateFilter{
			AfterMessageID: after, LimitMessages: r.config.PageSize,
			MessageTypes: r.config.MessageTypes, SourceIDs: r.config.SourceIDs,
		})
		if pageErr != nil {
			return ReconcileResult{}, pageErr
		}
		if err := r.reconcileCandidates(ctx, page.Candidates, sourceFence, &result, true); err != nil {
			return ReconcileResult{}, err
		}
		// Return one bounded page of claimed work to the caller for immediate
		// provider execution. The next pass resumes from this page's start —
		// its claimed owners have become AlreadyCurrent or carry durable
		// outcomes by then — instead of rescanning the whole archive, which
		// would reread every already-processed blob and make the build
		// quadratic. A process restart pays one linear catch-up scan.
		if len(result.Work) > 0 {
			r.scanCursor = after
			return result, nil
		}
		if !page.HasMore {
			break
		}
		after = page.NextAfterMessageID
		r.scanCursor = after
	}
	// The scan reached the end of the candidate set; the next full
	// reconcile starts over from the beginning.
	r.scanCursor = 0
	if err := r.tombstoneMissingPublications(ctx, sourceFence, &result); err != nil {
		return ReconcileResult{}, err
	}
	if busy, busyErr := r.otherWorkersActive(ctx, &result); busyErr != nil {
		return ReconcileResult{}, busyErr
	} else if busy {
		// Another worker still holds claims; leave the baseline open so its
		// work is re-scheduled if it dies, and let a later pass complete.
		return result, nil
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
	maxPages := r.maxScanPages()
	for pages := 0; ; pages++ {
		if pages >= maxPages {
			// Bounded pass: the consumer cursor holds the frontier and the
			// remaining journal lag keeps activation gated until a later
			// pass drains it.
			return result, nil
		}
		changes, err := r.archive.ListAttachmentChanges(ctx, r.config.ConsumerKey, r.config.PageSize)
		if err != nil {
			return ReconcileResult{}, err
		}
		if len(changes) == 0 {
			return result, r.sweepStalePublications(ctx, &result)
		}
		messageIDs := affectedMessageIDs(changes)
		sourceFence := changes[len(changes)-1].Sequence
		candidates, err := r.archive.ListVisualCandidates(ctx, store.VisualCandidateFilter{
			MessageIDs: messageIDs, MessageTypes: r.config.MessageTypes, SourceIDs: r.config.SourceIDs,
		})
		if err != nil {
			return ReconcileResult{}, err
		}
		if err := r.reconcileCandidates(ctx, candidates.Candidates, sourceFence, &result, true); err != nil {
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
		if busy, busyErr := r.otherWorkersActive(ctx, &result); busyErr != nil {
			return ReconcileResult{}, busyErr
		} else if busy {
			// Another worker's claims cover owners in this page; do not
			// consume the page until their outcomes are durable.
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

// sweepStalePublications re-evaluates publications the source-invalidation
// triggers marked stale in place. Message subject and body changes never
// enter the shared attachment journal — that journal's event kinds belong to
// attachment lifecycle — so context staleness is swept here, one bounded page
// per replay, after the journal is drained.
func (r *Reconciler) sweepStalePublications(ctx context.Context, result *ReconcileResult) error {
	messageIDs, err := r.archive.ListStaleVisualMessageIDs(ctx, r.config.GenerationID, r.config.PageSize)
	if err != nil {
		return err
	}
	if len(messageIDs) == 0 {
		return nil
	}
	// The journal is drained, so its high water is at or above every fence a
	// journal page assigned; claims and commits stay monotonic under it.
	sourceFence, err := r.archive.AttachmentChangeHighWater(ctx)
	if err != nil {
		return err
	}
	candidates, err := r.archive.ListVisualCandidates(ctx, store.VisualCandidateFilter{
		MessageIDs: messageIDs, MessageTypes: r.config.MessageTypes, SourceIDs: r.config.SourceIDs,
	})
	if err != nil {
		return err
	}
	// The sweep is the retry path: retryable outcomes are re-claimed here,
	// after the baseline completed, one bounded page per replay.
	if err := r.reconcileCandidates(ctx, candidates.Candidates, sourceFence, result, false); err != nil {
		return err
	}
	return r.tombstoneMissingForMessages(ctx, messageIDs, candidates.Candidates, sourceFence, result)
}

func (r *Reconciler) reconcileCandidates(
	ctx context.Context,
	candidates []store.VisualCandidate,
	sourceFence int64,
	result *ReconcileResult,
	skipRetryable bool,
) error {
	for _, candidate := range candidates {
		// PageSize pages by message, but the paid-work bound must hold per
		// owner: one message can carry arbitrarily many standalone
		// attachments. Stop claiming — and stop buffering media — once the
		// pass is full; the restart-safe scan picks up the rest next pass.
		if len(result.Work) >= r.config.PageSize {
			break
		}
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
			kind := OutcomeTerminal
			if eligibility.Reason == ReasonContentUnavailable {
				kind = OutcomeRetryable
			}
			if kind == OutcomeTerminal {
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
			if kind == OutcomeTerminal {
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
		// A durable terminal provider decision for this exact document
		// revision is converged: re-claiming would resubmit — and re-bill —
		// the same media every pass and hold reconciliation open forever.
		// Any context or media change produces a different revision and
		// falls through to a fresh claim.
		if getErr == nil && publication.OutcomeKind == OutcomeTerminal &&
			publication.PreparedRevision == document.Revision {
			result.Terminal++
			continue
		}
		// During the baseline scan, a durable retryable outcome for this
		// exact revision counts as evaluated so the cursor advances past it:
		// re-claiming immediately would hammer a persistently failing
		// provider every pass while starving every later owner. The
		// post-completion stale sweep (skipRetryable=false) owns retries.
		if skipRetryable && getErr == nil && publication.OutcomeKind == OutcomeRetryable &&
			publication.PreparedRevision == document.Revision {
			result.Retryable++
			continue
		}
		if getErr != nil && !errors.Is(getErr, sql.ErrNoRows) {
			return getErr
		}
		claim, acquired, err := r.claimWithStamp(ctx, candidate.Owner, document.Revision,
			sourceFence, &messageContext.ContentStamp)
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

func (r *Reconciler) maxScanPages() int {
	if r.config.MaxScanPagesPerPass > 0 {
		return r.config.MaxScanPagesPerPass
	}
	return 50
}

func (r *Reconciler) claim(
	ctx context.Context,
	owner store.VisualOwner,
	revision string,
	sourceFence int64,
) (store.VisualWorkClaim, bool, error) {
	return r.claimWithStamp(ctx, owner, revision, sourceFence, nil)
}

// claimWithStamp records the content stamp the caller read together with the
// context snapshot, so an edit racing the snapshot fails the commit-time CAS
// instead of being absorbed by a claim that reads the post-edit stamp.
func (r *Reconciler) claimWithStamp(
	ctx context.Context,
	owner store.VisualOwner,
	revision string,
	sourceFence int64,
	expectedContentStamp *string,
) (store.VisualWorkClaim, bool, error) {
	return r.archive.ClaimVisualWork(ctx, store.VisualClaimRequest{
		GenerationID: r.config.GenerationID, Owner: owner, ProposedRevision: revision,
		LeaseOwner: r.config.LeaseOwner, Now: r.config.Now(),
		LeaseDuration: r.config.LeaseDuration, SourceFence: sourceFence,
		ExpectedContentStamp: expectedContentStamp,
	})
}

func (r *Reconciler) tombstoneMissingPublications(
	ctx context.Context,
	sourceFence int64,
	result *ReconcileResult,
) error {
	// The paid-work page size (two owners) is for provider batches; this
	// sweep is read-mostly DB work, and walking a large archive two rows at
	// a time turned completion into tens of thousands of round trips that
	// could outlive a request deadline and restart from scratch.
	sweepPageSize := max(r.config.PageSize, 500)
	for after := int64(0); ; {
		page, err := r.archive.ListVisualPublications(ctx, r.config.GenerationID, store.VisualPublicationFilter{
			AfterMessageID: after, LimitMessages: sweepPageSize,
		})
		if err != nil {
			return err
		}
		messageIDs := publicationMessageIDs(page.Publications)
		var candidates store.VisualCandidatePage
		if len(messageIDs) > 0 {
			candidates, err = r.archive.ListVisualCandidates(ctx, store.VisualCandidateFilter{
				MessageIDs: messageIDs, MessageTypes: r.config.MessageTypes, SourceIDs: r.config.SourceIDs,
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
		strings.Join(config.MediaPolicy.AuthorizedCapabilities, ","),
		string(reason),
	} {
		writeRevisionField(hash, value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
