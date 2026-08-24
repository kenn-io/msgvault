package activity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

const (
	DefaultBatchSize             = 500
	DefaultMaxDirectCounterparts = store.DefaultMaxDirectActivityCounterparts
	MaxProjectionBatchSize       = 10_000
	projectorStaleRetries        = 3
	projectorPassRetries         = 8
	defaultProjectorTimezone     = "UTC"
)

var (
	errProjectionEpochChanged       = errors.New("activity projection epoch changed")
	errProjectionTimezoneChanged    = errors.New("activity projection timezone changed")
	errProjectionDirectLimitChanged = errors.New(
		"activity projection direct-counterpart limit changed")
	// ErrActivityTimezoneTransitionActive is returned by exact-ID projection
	// when finishing the persisted transition requires a full archive pass.
	ErrActivityTimezoneTransitionActive = errors.New(
		"activity timezone transition has a different active target")
)

type projectorStore interface {
	LoadQueuedActivityCandidatesContext(ctx context.Context, limit int) ([]store.ActivityCandidate, error)
	ScanForActivityProjectionContext(ctx context.Context, afterID int64, limit int) ([]store.ActivityCandidate, error)
	ScanAllActivityCandidatesContext(ctx context.Context, afterID int64, limit int) ([]store.ActivityCandidate, error)
	LoadActivityCandidatesByIDContext(ctx context.Context, messageIDs []int64) ([]store.ActivityCandidate, error)
	ProjectActivityBatchContext(ctx context.Context, items []store.ActivityProjection) (store.ActivityProjectionResult, error)
	ContactRevisionsContext(ctx context.Context) (store.ContactRevisions, error)
	ActivityReconciledRevisionsContext(ctx context.Context) (store.ContactRevisions, bool, error)
	CompareAndSetActivityReconciledRevisionsContext(ctx context.Context, expected store.ContactRevisions) error
	StaleContactStatePersonsContext(ctx context.Context, limit int) ([]int64, error)
	MarkAllContactStateDirtyContext(ctx context.Context) error
	RecomputeContactStateContext(ctx context.Context, personIDs []int64, revisions store.ContactRevisions) error
	ActivityWatermarkContext(ctx context.Context) (int64, error)
	SetActivityWatermarkContext(ctx context.Context, messageID int64) error
	ActivityTimezoneTransitionContext(ctx context.Context) (store.ActivityTimezoneTransition, error)
	ClaimActivityTimezoneTransitionContext(ctx context.Context, target string) (store.ActivityTimezoneTransition, error)
	CompleteActivityTimezoneTransitionContext(ctx context.Context, observed store.ActivityTimezoneTransition) error
	ActivityDirectLimitTransitionContext(ctx context.Context) (store.ActivityDirectLimitTransition, error)
	ClaimActivityDirectLimitTransitionContext(ctx context.Context, target int) (store.ActivityDirectLimitTransition, error)
	CompleteActivityDirectLimitTransitionContext(ctx context.Context, observed store.ActivityDirectLimitTransition) error
}
type Options struct {
	Timezone              string
	MaxDirectCounterparts int
	BatchSize             int
	Log                   *slog.Logger
}

type RunResult struct {
	EventsProjected   int
	PersonsTouched    int
	PersonsRecomputed int
	Batches           int
	Watermark         int64

	Processed       int
	EventsWritten   int
	EventsRetracted int
	ContactPersons  int
}

func (result *RunResult) add(batch store.ActivityProjectionResult) {
	if batch.Processed > 0 {
		result.Batches++
	}
	result.EventsProjected += batch.EventsWritten + batch.EventsRetracted
	result.PersonsTouched += batch.ContactPersons
	result.Processed += batch.Processed
	result.EventsWritten += batch.EventsWritten
	result.EventsRetracted += batch.EventsRetracted
	result.ContactPersons += batch.ContactPersons
}

type Projector struct {
	store                 projectorStore
	timezone              string
	location              *time.Location
	maxDirectCounterparts int
	batchSize             int
	log                   *slog.Logger
}

func NewProjector(st projectorStore, options Options) (*Projector, error) {
	if st == nil {
		return nil, errors.New("activity projector store is required")
	}
	timezone := options.Timezone
	if timezone == "" {
		timezone = defaultProjectorTimezone
	}
	location, err := LoadZone(timezone)
	if err != nil {
		return nil, err
	}
	batchSize := options.BatchSize
	if batchSize == 0 {
		batchSize = DefaultBatchSize
	}
	maxDirect := options.MaxDirectCounterparts
	if maxDirect == 0 {
		maxDirect = DefaultMaxDirectCounterparts
	}
	if batchSize < 0 || batchSize > MaxProjectionBatchSize {
		return nil, fmt.Errorf("activity: batch size must be between 1 and %d",
			MaxProjectionBatchSize)
	}
	if maxDirect < 0 || maxDirect > MaxProjectionBatchSize {
		return nil, fmt.Errorf(
			"activity: max direct counterparts must be between 1 and %d",
			MaxProjectionBatchSize)
	}
	log := options.Log
	if log == nil {
		log = slog.Default()
	}
	return &Projector{
		store:                 st,
		timezone:              timezone,
		location:              location,
		maxDirectCounterparts: maxDirect,
		batchSize:             batchSize,
		log:                   log,
	}, nil
}

func (p *Projector) RunOnce(ctx context.Context) (RunResult, error) {
	var result RunResult
	if err := p.convergeTimezone(ctx, &result); err != nil {
		return result, err
	}
	if err := p.convergeDirectLimit(ctx, &result); err != nil {
		return result, err
	}
	transition, err := p.configuredTransition(ctx)
	if err != nil {
		return result, err
	}
	directTransition, err := p.configuredDirectLimitTransition(ctx)
	if err != nil {
		return result, err
	}
	// Queue-first is a correctness boundary: repair-driven dirtiness must not
	// be cleared from old spine rows before the queued redates commit.
	if err := p.drainQueue(ctx, nil, &transition, &directTransition,
		p.location, p.maxDirectCounterparts, &result); err != nil {
		return result, err
	}
	if err := p.reconcileRevisions(
		ctx, transition, directTransition, &result); err != nil {
		return result, err
	}
	if err := p.drainQueue(ctx, nil, &transition, &directTransition,
		p.location, p.maxDirectCounterparts, &result); err != nil {
		return result, err
	}
	watermark, err := p.store.ActivityWatermarkContext(ctx)
	if err != nil {
		p.log.Warn("activity: read watermark failed; scanning from start", "error", err)
		watermark = 0
	}
	result.Watermark = watermark
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		candidates, scanErr := p.store.ScanForActivityProjectionContext(
			ctx, result.Watermark, p.batchSize)
		if scanErr != nil {
			return result, scanErr
		}
		if len(candidates) == 0 {
			break
		}
		if err := p.projectCandidates(
			ctx, candidates, nil, &transition, &directTransition,
			p.location, p.maxDirectCounterparts, &result); err != nil {
			if errors.Is(err, errProjectionTimezoneChanged) {
				return result, fmt.Errorf(
					"activity: timezone changed during forward scan: %w", err)
			}
			return result, err
		}
	}
	if err := p.drainQueue(ctx, nil, &transition, &directTransition,
		p.location, p.maxDirectCounterparts, &result); err != nil {
		return result, err
	}
	return result, nil
}

func (p *Projector) RunBackstop(ctx context.Context) (RunResult, error) {
	var result RunResult
	if err := p.convergeTimezone(ctx, &result); err != nil {
		return result, err
	}
	if err := p.convergeDirectLimit(ctx, &result); err != nil {
		return result, err
	}
	transition, err := p.configuredTransition(ctx)
	if err != nil {
		return result, err
	}
	directTransition, err := p.configuredDirectLimitTransition(ctx)
	if err != nil {
		return result, err
	}
	if err := p.drainQueue(ctx, nil, &transition, &directTransition,
		p.location, p.maxDirectCounterparts, &result); err != nil {
		return result, err
	}
	if err := p.reconcileRevisions(
		ctx, transition, directTransition, &result); err != nil {
		return result, err
	}
	if err := p.store.MarkAllContactStateDirtyContext(ctx); err != nil {
		return result, err
	}
	if err := p.forceScan(ctx, nil, &transition, &directTransition,
		p.location, p.maxDirectCounterparts, &result); err != nil {
		return result, err
	}
	if err := p.drainQueue(ctx, nil, &transition, &directTransition,
		p.location, p.maxDirectCounterparts, &result); err != nil {
		return result, err
	}
	recomputed, err := p.RecomputeStale(ctx)
	result.merge(recomputed)
	return result, err
}

func (p *Projector) ProjectMessages(
	ctx context.Context,
	messageIDs []int64,
) (RunResult, error) {
	var result RunResult
	transition, err := p.store.ActivityTimezoneTransitionContext(ctx)
	if err != nil {
		return result, err
	}
	if transition.Active && transition.Target != p.timezone {
		return result, fmt.Errorf("%w: persisted target %q",
			ErrActivityTimezoneTransitionActive, transition.Target)
	}
	if !transition.Active &&
		(transition.Target != p.timezone || transition.Generation <= 0) {
		return result, fmt.Errorf(
			"activity: timezone %q is not reconciled; run the backstop first",
			p.timezone)
	}
	directTransition, err := p.store.ActivityDirectLimitTransitionContext(ctx)
	if err != nil {
		return result, err
	}
	if directTransition.Active ||
		directTransition.Target != p.maxDirectCounterparts ||
		directTransition.Generation <= 0 {
		return result, fmt.Errorf(
			"activity: max direct counterparts %d is not reconciled; run the backstop first",
			p.maxDirectCounterparts)
	}
	unique := make(map[int64]struct{}, len(messageIDs))
	for _, messageID := range messageIDs {
		if messageID <= 0 {
			return result, errors.New("activity: message id must be positive")
		}
		unique[messageID] = struct{}{}
	}
	sorted := make([]int64, 0, len(unique))
	for messageID := range unique {
		sorted = append(sorted, messageID)
	}
	slices.Sort(sorted)
	for start := 0; start < len(sorted); start += p.batchSize {
		end := min(start+p.batchSize, len(sorted))
		candidates, loadErr := p.store.LoadActivityCandidatesByIDContext(
			ctx, sorted[start:end])
		if loadErr != nil {
			return result, loadErr
		}
		if err := p.projectCandidates(
			ctx, candidates, nil, &transition, &directTransition,
			p.location, p.maxDirectCounterparts, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (p *Projector) RecomputeStale(ctx context.Context) (RunResult, error) {
	var result RunResult
	for {
		current, err := p.store.ContactRevisionsContext(ctx)
		if err != nil {
			return result, err
		}
		reconciled, ok, err := p.store.ActivityReconciledRevisionsContext(ctx)
		if err != nil {
			return result, err
		}
		if !ok || current != reconciled {
			return result, errProjectionEpochChanged
		}
		personIDs, err := p.store.StaleContactStatePersonsContext(ctx, p.batchSize)
		if err != nil {
			return result, err
		}
		if len(personIDs) == 0 {
			return result, nil
		}
		if err := p.store.RecomputeContactStateContext(
			ctx, personIDs, current); err != nil {
			if _, ok := errors.AsType[*store.ErrActivityProjectionStale](err); ok {
				return result, errProjectionEpochChanged
			}
			return result, err
		}
		result.PersonsRecomputed += len(personIDs)
		result.Batches++
	}
}

func (p *Projector) convergeTimezone(
	ctx context.Context,
	result *RunResult,
) error {
	for range projectorPassRetries {
		current, err := p.store.ActivityTimezoneTransitionContext(ctx)
		if err != nil {
			return err
		}
		if !current.Active &&
			current.Target == p.timezone &&
			current.Generation > 0 {
			return nil
		}
		if !current.Active {
			current, err = p.store.ClaimActivityTimezoneTransitionContext(
				ctx, p.timezone)
			if err != nil {
				return err
			}
			if !current.Active &&
				current.Target == p.timezone &&
				current.Generation > 0 {
				return nil
			}
		}
		location, err := LoadZone(current.Target)
		if err != nil {
			return fmt.Errorf("activity: persisted timezone transition: %w", err)
		}
		direct, err := p.store.ActivityDirectLimitTransitionContext(ctx)
		if err != nil {
			return err
		}
		if !direct.Active && direct.Generation == 0 {
			direct, err = p.store.ClaimActivityDirectLimitTransitionContext(
				ctx, p.maxDirectCounterparts)
			if err != nil {
				return err
			}
		}
		maxDirect := direct.Target
		if maxDirect <= 0 {
			maxDirect = p.maxDirectCounterparts
		}
		if err := p.forceScan(ctx, nil, &current, &direct,
			location, maxDirect, result); err != nil {
			if errors.Is(err, errProjectionTimezoneChanged) ||
				errors.Is(err, errProjectionDirectLimitChanged) {
				continue
			}
			return err
		}
		if err := p.drainQueue(ctx, nil, &current, &direct,
			location, maxDirect, result); err != nil {
			if errors.Is(err, errProjectionTimezoneChanged) ||
				errors.Is(err, errProjectionDirectLimitChanged) {
				continue
			}
			return err
		}
		if err := p.store.CompleteActivityTimezoneTransitionContext(
			ctx, current); err != nil {
			if _, ok := errors.AsType[*store.ErrActivityProjectionStale](err); ok {
				continue
			}
			return err
		}
		if direct.Active {
			if err := p.store.CompleteActivityDirectLimitTransitionContext(
				ctx, direct); err != nil {
				if _, ok := errors.AsType[*store.ErrActivityProjectionStale](err); ok {
					continue
				}
				return err
			}
		}
	}
	return fmt.Errorf("activity: timezone convergence did not stabilize after %d passes",
		projectorPassRetries)
}

func (p *Projector) convergeDirectLimit(
	ctx context.Context,
	result *RunResult,
) error {
	for range projectorPassRetries {
		current, err := p.store.ActivityDirectLimitTransitionContext(ctx)
		if err != nil {
			return err
		}
		if !current.Active && current.Target == p.maxDirectCounterparts &&
			current.Generation > 0 {
			return nil
		}
		if !current.Active {
			current, err = p.store.ClaimActivityDirectLimitTransitionContext(
				ctx, p.maxDirectCounterparts)
			if err != nil {
				return err
			}
			if !current.Active && current.Target == p.maxDirectCounterparts &&
				current.Generation > 0 {
				return nil
			}
		}
		timezone, err := p.configuredTransition(ctx)
		if err != nil {
			return err
		}
		if err := p.forceScan(ctx, nil, &timezone, &current,
			p.location, current.Target, result); err != nil {
			if errors.Is(err, errProjectionTimezoneChanged) ||
				errors.Is(err, errProjectionDirectLimitChanged) {
				continue
			}
			return err
		}
		if err := p.drainQueue(ctx, nil, &timezone, &current,
			p.location, current.Target, result); err != nil {
			if errors.Is(err, errProjectionTimezoneChanged) ||
				errors.Is(err, errProjectionDirectLimitChanged) {
				continue
			}
			return err
		}
		if err := p.store.CompleteActivityDirectLimitTransitionContext(
			ctx, current); err != nil {
			if _, ok := errors.AsType[*store.ErrActivityProjectionStale](err); ok {
				continue
			}
			return err
		}
	}
	return fmt.Errorf(
		"activity: direct-counterpart convergence did not stabilize after %d passes",
		projectorPassRetries)
}

func (p *Projector) reconcileRevisions(
	ctx context.Context,
	timezoneTransition store.ActivityTimezoneTransition,
	directTransition store.ActivityDirectLimitTransition,
	result *RunResult,
) error {
	for range projectorPassRetries {
		current, err := p.store.ContactRevisionsContext(ctx)
		if err != nil {
			return err
		}
		target := current
		if err := p.drainQueue(
			ctx, &target, &timezoneTransition, &directTransition,
			p.location, p.maxDirectCounterparts, result); err != nil {
			if errors.Is(err, errProjectionEpochChanged) {
				continue
			}
			return err
		}
		current, err = p.store.ContactRevisionsContext(ctx)
		if err != nil {
			return err
		}
		if current != target {
			continue
		}
		reconciled, ok, err := p.store.ActivityReconciledRevisionsContext(ctx)
		if err != nil {
			return err
		}
		if ok && current == reconciled {
			var recomputed RunResult
			recomputed, err = p.RecomputeStale(ctx)
			result.merge(recomputed)
			if errors.Is(err, errProjectionEpochChanged) {
				continue
			}
			return err
		}
		afterID := int64(0)
		restart := false
		for {
			candidates, loadErr := p.store.ScanForActivityProjectionContext(
				ctx, afterID, p.batchSize)
			if loadErr != nil {
				return loadErr
			}
			if len(candidates) == 0 {
				break
			}
			if err := p.projectCandidates(
				ctx, candidates, &target, &timezoneTransition, &directTransition,
				p.location, p.maxDirectCounterparts, result); err != nil {
				if errors.Is(err, errProjectionEpochChanged) {
					restart = true
					break
				}
				return err
			}
			afterID = candidates[len(candidates)-1].MessageID
		}
		if restart {
			continue
		}
		if err := p.drainQueue(
			ctx, &target, &timezoneTransition, &directTransition,
			p.location, p.maxDirectCounterparts, result); err != nil {
			if errors.Is(err, errProjectionEpochChanged) {
				continue
			}
			return err
		}
		remaining, err := p.store.ScanForActivityProjectionContext(
			ctx, 0, 1)
		if err != nil {
			return err
		}
		if len(remaining) != 0 {
			continue
		}
		if err := p.store.CompareAndSetActivityReconciledRevisionsContext(
			ctx, target); err != nil {
			if _, ok := errors.AsType[*store.ErrActivityProjectionStale](err); ok {
				continue
			}
			return err
		}
		recomputed, err := p.RecomputeStale(ctx)
		result.merge(recomputed)
		if err != nil {
			if errors.Is(err, errProjectionEpochChanged) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("activity: identity reconciliation did not stabilize after %d passes",
		projectorPassRetries)
}

func (p *Projector) forceScan(
	ctx context.Context,
	expected *store.ContactRevisions,
	expectedTimezone *store.ActivityTimezoneTransition,
	expectedDirectLimit *store.ActivityDirectLimitTransition,
	location *time.Location,
	maxDirectCounterparts int,
	result *RunResult,
) error {
	afterID := int64(0)
	for {
		candidates, err := p.store.ScanAllActivityCandidatesContext(
			ctx, afterID, p.batchSize)
		if err != nil {
			return err
		}
		if len(candidates) == 0 {
			return nil
		}
		if err := p.projectCandidates(
			ctx, candidates, expected, expectedTimezone, expectedDirectLimit,
			location, maxDirectCounterparts, result); err != nil {
			return err
		}
		afterID = candidates[len(candidates)-1].MessageID
	}
}

func (p *Projector) drainQueue(
	ctx context.Context,
	expected *store.ContactRevisions,
	expectedTimezone *store.ActivityTimezoneTransition,
	expectedDirectLimit *store.ActivityDirectLimitTransition,
	location *time.Location,
	maxDirectCounterparts int,
	result *RunResult,
) error {
	for {
		candidates, err := p.store.LoadQueuedActivityCandidatesContext(
			ctx, p.batchSize)
		if err != nil {
			return err
		}
		if len(candidates) == 0 {
			return nil
		}
		if err := p.projectCandidates(
			ctx, candidates, expected, expectedTimezone, expectedDirectLimit,
			location, maxDirectCounterparts, result); err != nil {
			return err
		}
	}
}

func (p *Projector) projectCandidates(
	ctx context.Context,
	candidates []store.ActivityCandidate,
	expected *store.ContactRevisions,
	expectedTimezone *store.ActivityTimezoneTransition,
	expectedDirectLimit *store.ActivityDirectLimitTransition,
	location *time.Location,
	maxDirectCounterparts int,
	result *RunResult,
) error {
	if len(candidates) == 0 {
		return nil
	}
	current := candidates
	for attempt := range projectorStaleRetries {
		if expected != nil && !candidatesAtRevision(current, *expected) {
			return errProjectionEpochChanged
		}
		if expectedTimezone != nil &&
			!candidatesAtTimezone(current, *expectedTimezone) {
			return errProjectionTimezoneChanged
		}
		if expectedDirectLimit != nil &&
			!candidatesAtDirectLimit(current, *expectedDirectLimit) {
			return errProjectionDirectLimitChanged
		}
		projections, err := p.projections(
			current, location, maxDirectCounterparts)
		if err != nil {
			return err
		}
		batch, err := p.store.ProjectActivityBatchContext(ctx, projections)
		if err == nil {
			result.add(batch)
			p.advanceWatermark(ctx, current, result)
			return nil
		}
		if _, ok := errors.AsType[*store.ErrActivityProjectionStale](err); !ok {
			return err
		}
		if attempt+1 == projectorStaleRetries {
			return fmt.Errorf(
				"activity: projection stayed stale after %d attempts: %w",
				projectorStaleRetries, err)
		}
		ids := make([]int64, len(current))
		for index := range current {
			ids[index] = current[index].MessageID
		}
		current, err = p.store.LoadActivityCandidatesByIDContext(ctx, ids)
		if err != nil {
			return err
		}
	}
	return nil
}

func candidatesAtRevision(
	candidates []store.ActivityCandidate,
	expected store.ContactRevisions,
) bool {
	for _, candidate := range candidates {
		if candidate.IdentityRevision != expected.IdentityRevision ||
			candidate.AccountIdentityRevision != expected.AccountIdentityRevision {
			return false
		}
	}
	return true
}

func candidatesAtTimezone(
	candidates []store.ActivityCandidate,
	expected store.ActivityTimezoneTransition,
) bool {
	for _, candidate := range candidates {
		if candidate.TimezoneTransition != expected {
			return false
		}
	}
	return true
}

func candidatesAtDirectLimit(
	candidates []store.ActivityCandidate,
	expected store.ActivityDirectLimitTransition,
) bool {
	for _, candidate := range candidates {
		if candidate.DirectLimitTransition != expected {
			return false
		}
	}
	return true
}

func (p *Projector) configuredTransition(
	ctx context.Context,
) (store.ActivityTimezoneTransition, error) {
	transition, err := p.store.ActivityTimezoneTransitionContext(ctx)
	if err != nil {
		return store.ActivityTimezoneTransition{}, err
	}
	if transition.Active ||
		transition.Target != p.timezone ||
		transition.Generation <= 0 {
		return store.ActivityTimezoneTransition{},
			fmt.Errorf("%w: expected completed timezone %q",
				errProjectionTimezoneChanged, p.timezone)
	}
	return transition, nil
}

func (p *Projector) configuredDirectLimitTransition(
	ctx context.Context,
) (store.ActivityDirectLimitTransition, error) {
	transition, err := p.store.ActivityDirectLimitTransitionContext(ctx)
	if err != nil {
		return store.ActivityDirectLimitTransition{}, err
	}
	if transition.Active || transition.Target != p.maxDirectCounterparts ||
		transition.Generation <= 0 {
		return store.ActivityDirectLimitTransition{}, fmt.Errorf(
			"%w: expected completed max direct counterparts %d",
			errProjectionDirectLimitChanged, p.maxDirectCounterparts)
	}
	return transition, nil
}

func (p *Projector) advanceWatermark(
	ctx context.Context,
	candidates []store.ActivityCandidate,
	result *RunResult,
) {
	if len(candidates) == 0 {
		return
	}
	messageID := candidates[len(candidates)-1].MessageID
	if messageID <= result.Watermark {
		return
	}
	if err := p.store.SetActivityWatermarkContext(ctx, messageID); err != nil {
		p.log.Warn("activity: advance watermark failed; committed work will be rescanned",
			"message_id", messageID, "error", err)
		return
	}
	result.Watermark = messageID
}

func (result *RunResult) merge(other RunResult) {
	result.EventsProjected += other.EventsProjected
	result.PersonsTouched += other.PersonsTouched
	result.PersonsRecomputed += other.PersonsRecomputed
	result.Batches += other.Batches
	if other.Watermark > result.Watermark {
		result.Watermark = other.Watermark
	}
	result.Processed += other.Processed
	result.EventsWritten += other.EventsWritten
	result.EventsRetracted += other.EventsRetracted
	result.ContactPersons += other.ContactPersons
}

func (p *Projector) projections(
	candidates []store.ActivityCandidate,
	location *time.Location,
	maxDirectCounterparts int,
) ([]store.ActivityProjection, error) {
	result := make([]store.ActivityProjection, 0, len(candidates))
	for _, candidate := range candidates {
		token := store.ActivityProjectionToken{
			MessageID:               candidate.MessageID,
			SourceID:                candidate.SourceID,
			LastModified:            candidate.LastModified,
			Queue:                   candidate.Queue,
			TimezoneTransition:      candidate.TimezoneTransition,
			DirectLimitTransition:   candidate.DirectLimitTransition,
			IdentityRevision:        candidate.IdentityRevision,
			AccountIdentityRevision: candidate.AccountIdentityRevision,
			ConversationID:          candidate.ConversationID,
			ConversationType:        candidate.ConversationType,
			MessageType:             candidate.MessageType,
		}
		projection := store.ActivityProjection{Token: token}
		if !candidate.Eligible {
			result = append(result, projection)
			continue
		}
		dating, err := DateEvent(Timestamps{
			SentAt:       candidate.SentAt,
			ReceivedAt:   candidate.ReceivedAt,
			InternalDate: candidate.InternalDate,
			DateOnly:     candidate.DateOnly,
		}, location)
		if err != nil {
			return nil, fmt.Errorf(
				"activity: date message %d: %w", candidate.MessageID, err)
		}
		classification := store.ClassifyActivityCandidate(candidate, maxDirectCounterparts)
		projection.Event = &store.ActivityEvent{
			MessageID:                        candidate.MessageID,
			RefKind:                          classification.RefKind,
			SourceID:                         candidate.SourceID,
			ConversationID:                   candidate.ConversationID,
			Channel:                          classification.Channel,
			OccurredAt:                       dating.OccurredAt,
			DateOrigin:                       string(dating.Origin),
			DatePrecision:                    string(dating.Precision),
			Timezone:                         dating.Timezone,
			UTCOffsetMinutes:                 dating.UTCOffsetMinutes,
			LocalDate:                        dating.LocalDate,
			Direction:                        classification.Direction,
			OwnerSourceID:                    classification.OwnerSourceID,
			OwnerAddress:                     classification.OwnerAddress,
			ProjectedLastModified:            candidate.LastModified,
			ProjectedIdentityRevision:        candidate.IdentityRevision,
			ProjectedAccountIdentityRevision: candidate.AccountIdentityRevision,
			Persons:                          classification.Persons,
		}
		result = append(result, projection)
	}
	return result, nil
}
