package peoplesweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

// BriefMode selects how one run treats the person brief step.
//
//   - auto follows the cadence rules: enrolled, new activity past the current
//     brief's boundary, and either an old enough brief or a cadence due inside
//     the pre-call window.
//   - force keeps enrollment and budget but bypasses the min-interval and
//     new-activity checks, which is what a manual "generate now" asks for.
//   - skip omits the brief step entirely.
type BriefMode string

const (
	BriefModeAuto  BriefMode = "auto"
	BriefModeForce BriefMode = "force"
	BriefModeSkip  BriefMode = "skip"
)

// normalized treats the zero value as auto so a caller that never heard of the
// brief keeps the scheduled behavior.
func (m BriefMode) normalized() BriefMode {
	if m == "" {
		return BriefModeAuto
	}
	return m
}

func (m BriefMode) valid() bool {
	switch m.normalized() {
	case BriefModeAuto, BriefModeForce, BriefModeSkip:
		return true
	default:
		return false
	}
}

// Brief version statuses, mirroring the values the store writes to
// person_briefs.status. The worker needs them because a rejected version is an
// explicit request for a new one.
const (
	BriefStatusCurrent    = "current"
	BriefStatusSuperseded = "superseded"
	BriefStatusRejected   = "rejected"
)

// The refusals a manual "generate this person's brief now" can hit before it
// does any work. Each is a caller error rather than a run failure: the sweep
// refuses before it publishes work or spends anything, and the API turns each
// into its own 409 so a request that could never produce a brief never answers
// 200 with version 0 and no failure class.
var (
	// ErrPersonBriefNotEnrolled reports that the person has no enrollment row.
	ErrPersonBriefNotEnrolled = errors.New("person is not enrolled for briefs")
	// ErrPersonBriefLaneDisabled reports that [people.sweep.brief] enabled is
	// false, which turns the lane off globally without unenrolling anybody.
	ErrPersonBriefLaneDisabled = errors.New("person brief lane is disabled")
	// ErrPersonBriefPolicyRefused reports that the consented provider profile
	// sets allow_sensitive = false. A brief packet always carries verbatim
	// archive text, so that profile can never generate one.
	ErrPersonBriefPolicyRefused = errors.New(
		"person brief is refused by the provider profile's sensitive-content policy")
	// ErrPersonBriefNoSupportedLane reports that the consented provider
	// profile's allowed_sources share no lane with the brief window. The window
	// reads conversation_text only in this version, so a profile limited to
	// document_text or meeting_text has nothing a brief could be built from.
	ErrPersonBriefNoSupportedLane = errors.New(
		"person brief has no supported source lane in the provider profile")
	// ErrPersonBriefBusy reports that the person's sweep work could not be
	// claimed because another worker holds the lease. The requested generation
	// did not run; the caller retries once that worker has finished.
	ErrPersonBriefBusy = errors.New("person brief is held by another worker")
)

// BriefEligibility is one enrolled and tracked person's current brief state, in
// the sweep's own vocabulary. Version is zero when the person has no brief yet;
// otherwise the fields describe the latest version, whose status is current
// unless the owner rejected it.
type BriefEligibility struct {
	PersonID    int64
	EnabledAt   time.Time
	Version     int
	Status      string
	GeneratedAt *time.Time
	// Boundary is the latest version's stored input boundary, or nil when the
	// person has no brief yet. Its ThroughSequence bounds the new-activity
	// check and its ThroughEventTime bounds the overlap window.
	Boundary *BriefBoundary
}

// BriefStore is the durable brief state the worker reads, plus the work
// publication a forced single-person run needs. It is deliberately narrow: the
// worker never sees enrollment writes or stored brief text.
type BriefStore interface {
	// PersonSweepBriefEligibility reports nothing when the person is not both
	// enrolled and tracked.
	PersonSweepBriefEligibility(ctx context.Context, personID int64) (BriefEligibility, bool, error)
	// PersonSweepCadenceDueAt reports person_contact_state's cadence due date,
	// and false when the person has no contact state or no due date.
	PersonSweepCadenceDueAt(ctx context.Context, personID int64, now time.Time) (time.Time, bool, error)
	// HasPersonSweepChangesAfter reports whether the person's change journal
	// holds a row past a durable sequence.
	HasPersonSweepChangesAfter(ctx context.Context, personID, sequence int64) (bool, error)
	// LatestPersonSweepChangeSequence is the archive's commit high water, which
	// the brief records as its window's through_sequence.
	LatestPersonSweepChangeSequence(ctx context.Context) (int64, error)
	// EnsurePersonSweepWork publishes work even when extraction is caught up.
	// forceAvailable clears retry backoff for an explicit generation request;
	// automatic scheduling preserves it. Untracked people return false.
	EnsurePersonSweepWork(ctx context.Context, personID int64, forceAvailable bool) (bool, error)
}

// BriefResult is one generated brief, ready to be stored in the same
// transaction as the attempt's extraction result. Possible attributes remain
// suggestions in Structured; they do not enter the profile fact generation.
type BriefResult struct {
	ProgramID          string
	ProgramVersion     string
	ProgramFingerprint string
	Boundary           BriefBoundary
	Structured         json.RawMessage
	Rendered           RenderedBrief
	// Evidence is the archive evidence the structure cites, in citation order.
	// The store aligns each input and writes the surviving rows as the brief's
	// evidence pointers.
	Evidence         []personfacts.EvidenceInput
	DroppedItemCount int
	GeneratedAt      time.Time
}

// briefPlan is the decision the worker made before spending anything.
type briefPlan struct {
	run             bool
	previous        *BriefBoundary
	throughSequence int64
}

// planPersonBrief decides whether this attempt should append a brief call. It
// runs before the assembly so an eligible brief can reserve one of the person's
// request slots, and it reads only durable state: no provider call, no packet,
// and no archive text.
func (w *Worker) planPersonBrief(
	ctx context.Context, personID int64, mode BriefMode, profile ProviderProfile, now time.Time,
) (briefPlan, error) {
	if !w.Config.Brief.IsEnabled() || w.Brief == nil || w.Archive == nil ||
		mode.normalized() == BriefModeSkip {
		return briefPlan{}, nil
	}
	// A brief packet always carries verbatim archive text, so a profile without
	// allow_sensitive refuses it exactly as it refuses real extraction. Deciding
	// that here keeps the refusal free instead of paying for a window first.
	if !profile.AllowSensitive {
		return briefPlan{}, nil
	}
	// The window reads only the brief lanes. A profile whose allowed_sources
	// miss all of them, such as document_text or meeting_text alone, would plan a brief that
	// BuildBriefWindow then refuses, so the attempt would record an internal
	// failure for a condition that is plain configuration. Decide it here, for
	// free, and let a scheduled run skip the step cleanly.
	if len(briefWindowLanes(profile.AllowedSources)) == 0 {
		return briefPlan{}, nil
	}
	eligibility, enrolled, err := w.Brief.PersonSweepBriefEligibility(ctx, personID)
	if err != nil {
		return briefPlan{}, err
	}
	if !enrolled {
		return briefPlan{}, nil
	}
	through, err := w.Brief.LatestPersonSweepChangeSequence(ctx)
	if err != nil {
		return briefPlan{}, err
	}
	plan := briefPlan{previous: eligibility.Boundary, throughSequence: through}
	if mode.normalized() == BriefModeForce {
		plan.run = true
		return plan, nil
	}
	// Ruling R10: a rejected version is the owner asking for a different brief,
	// so it counts as "no brief yet" for the cadence rules. Its boundary still
	// seeds the overlap, because the window it summarized is still the stretch
	// the next brief should recognize as already covered.
	if eligibility.Version == 0 || eligibility.Boundary == nil ||
		eligibility.Status == BriefStatusRejected {
		plan.run = true
		return plan, nil
	}
	fresh, err := w.Brief.HasPersonSweepChangesAfter(
		ctx, personID, eligibility.Boundary.ThroughSequence)
	if err != nil {
		return briefPlan{}, err
	}
	if !fresh {
		return plan, nil
	}
	if eligibility.GeneratedAt == nil ||
		!eligibility.GeneratedAt.Add(w.Config.Brief.MinInterval).After(now) {
		plan.run = true
		return plan, nil
	}
	dueAt, hasDue, err := w.Brief.PersonSweepCadenceDueAt(ctx, personID, now)
	if err != nil {
		return briefPlan{}, err
	}
	// A cadence that is already overdue is inside the window too: the owner is
	// about to reach out and the brief is what they need before they do.
	if hasDue && !dueAt.After(now.Add(w.Config.Brief.PreCallWindow)) {
		plan.run = true
	}
	return plan, nil
}

// briefWindowRequest turns the plan and the configuration into the window the
// brief call will summarize.
func (w *Worker) briefWindowRequest(
	personID int64, catalog personfacts.Catalog, profile ProviderProfile,
	plan briefPlan, batchOrdinal int,
) BriefWindowRequest {
	return BriefWindowRequest{
		PersonID: personID, Catalog: catalog, Profile: profile,
		MaxItems: w.Config.Brief.MaxItems, MaxBytes: w.Config.Brief.MaxBytes,
		OverlapItems:     w.Config.Brief.OverlapItems,
		MaxOutputTokens:  w.Config.Brief.MaxOutputTokens,
		MaxRenderedRunes: w.Config.Brief.MaxRenderedRunes,
		ThroughSequence:  plan.throughSequence,
		Previous:         plan.previous, BatchOrdinal: batchOrdinal,
	}
}

// briefEvidenceInputs is the citation-ordered evidence the store must align and
// point at. It is separated so the worker never hands the store a slice it
// still owns.
func briefEvidenceInputs(parsed ParsedBrief) []personfacts.EvidenceInput {
	inputs := make([]personfacts.EvidenceInput, len(parsed.Evidence))
	copy(inputs, parsed.Evidence)
	return inputs
}

// renderBriefResult renders, trims, and freezes one validated brief. The
// paragraph is capped at the configured rune count by dropping structured items
// from the tail, and the trimmed structure is what gets stored. It reports
// ErrEmptyBrief when every structured item was dropped, which the caller
// records as a brief failure rather than storing a blank version.
func renderBriefResult(
	parsed ParsedBrief, window BriefWindow, generatedAt time.Time,
) (BriefResult, error) {
	trimmed, rendered, err := trimRenderedBrief(parsed, window, window.Request.MaxRenderedRunes)
	if err != nil {
		return BriefResult{}, err
	}
	return newBriefResult(trimmed, rendered, window, generatedAt)
}

// newBriefResult freezes one validated, rendered brief into the durable shape
// the apply transaction stores.
func newBriefResult(
	parsed ParsedBrief, rendered RenderedBrief, window BriefWindow, generatedAt time.Time,
) (BriefResult, error) {
	structured, err := json.Marshal(parsed.Output)
	if err != nil {
		return BriefResult{}, fmt.Errorf("encode person brief structure: %w", err)
	}
	if len(rendered.Sentences) == 0 || rendered.Text == "" {
		return BriefResult{}, ErrEmptyBrief
	}
	return BriefResult{
		ProgramID: BriefProgramID, ProgramVersion: BriefProgramVersion,
		ProgramFingerprint: BriefProgramFingerprint(),
		Boundary:           window.Boundary.Canonical(), Structured: structured, Rendered: rendered,
		Evidence: briefEvidenceInputs(parsed), DroppedItemCount: parsed.DroppedItemCount,
		GeneratedAt: generatedAt.UTC(),
	}, nil
}

// ValidateBriefResult rejects a brief that is not internally complete. The
// store calls it before it writes anything, so a malformed result fails the
// apply rather than storing a version a reader cannot interpret.
func ValidateBriefResult(personID int64, result *BriefResult) error {
	if result == nil {
		return nil
	}
	if result.ProgramID != BriefProgramID || result.ProgramVersion != BriefProgramVersion ||
		result.ProgramFingerprint != BriefProgramFingerprint() {
		return errors.New("person brief result does not use the frozen brief program")
	}
	if result.Rendered.Policy != BriefRendererPolicyV1 || result.Rendered.Text == "" ||
		len(result.Rendered.Sentences) == 0 {
		return errors.New("person brief result has no rendered paragraph")
	}
	if !json.Valid(result.Structured) {
		return errors.New("person brief result structure is not JSON")
	}
	if result.DroppedItemCount < 0 || result.GeneratedAt.IsZero() {
		return errors.New("person brief result has an invalid drop count or generation time")
	}
	if result.Boundary.PacketSHA256 == "" || result.Boundary.ItemCount <= 0 {
		return errors.New("person brief result has an incomplete input boundary")
	}
	for _, evidence := range result.Evidence {
		if evidence.PersonID != personID {
			return errors.New("person brief evidence belongs to another person")
		}
	}
	return nil
}

// briefCall is everything the brief step needs from the attempt that owns it.
type briefCall struct {
	runID        string
	attemptID    string
	lease        Lease
	profile      ProviderProfile
	catalog      personfacts.Catalog
	resolvedAt   time.Time
	plan         briefPlan
	batchOrdinal int
}

// runBriefCall appends the person brief to an attempt whose extraction batches
// already ran.
//
// Everything that can fail cheaply happens before the reservation, so a brief
// that cannot run never leaves a stranded budget row. After the reservation the
// step is deliberately forgiving: a provider failure, an unusable candidate, or
// a brief where every item was dropped is recorded as a brief failure class and
// the extraction still applies, because a brief is derived context and the
// facts the attempt already paid for are not.
//
// The returned error is fatal to the attempt and is reserved for the cases
// where the store itself could not record what happened.
func (w *Worker) runBriefCall(
	ctx context.Context, call briefCall, accounting *sweepCallAccounting,
	reservations *[]BudgetReservation,
) (*BriefResult, FailureClass, Lease, error) {
	lease := call.lease
	window, err := BuildBriefWindow(ctx, w.Archive, w.briefWindowRequest(
		lease.PersonID, call.catalog, call.profile, call.plan, call.batchOrdinal))
	if err != nil {
		if errors.Is(err, ErrNoBriefEvidence) {
			// Nothing to summarize is not a failure: the person is enrolled but
			// has no eligible evidence in the profile's lanes and dates.
			return nil, "", lease, nil
		}
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}

	prepared, err := w.Runner.PrepareStructured(ctx, window.Batch.Request)
	if err != nil {
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}
	estimate, err := EstimateWireTokenReservation(
		prepared.WireRequest(), window.Batch.Request.MaxOutputTokens)
	if err != nil {
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}
	estimatedCost, err := EstimateCostMicroUSD(estimate, w.Config.Budgets)
	if err != nil {
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}
	execution, err := w.Runner.BeginStructuredExecution(ctx, prepared)
	if err != nil {
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}
	preparedCall, err := execution.PrimaryCall(prepared)
	if err != nil {
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}

	reservation, err := w.Store.ReservePersonSweepBudget(ctx, BudgetReservationRequest{
		RunID: call.runID, AttemptID: call.attemptID, BatchOrdinal: call.batchOrdinal,
		CallOrdinal: 0, Purpose: ProviderCallPurposeBrief, PersonID: lease.PersonID,
		ProviderFingerprint: call.profile.Fingerprint,
		UTCDate:             call.resolvedAt.UTC().Format(time.DateOnly),
		InputHash:           prepared.WireSHA256(),
		ItemCount:           len(window.Batch.Packet.Seeds) + len(window.Batch.Packet.Context),
		EstimatedRequests:   1, EstimatedInputTokens: estimate.InputTokens,
		EstimatedOutputTokens: estimate.OutputTokens, EstimatedCostMicroUSD: estimatedCost,
		Budget: w.Config.Budgets,
	})
	if err != nil {
		// Budget exhaustion defers the brief to the next run; the reservation
		// rolled back, so there is nothing to reconcile.
		return nil, briefFailureClass(err), lease, briefFatalError(ctx, err)
	}
	*reservations = append(*reservations, reservation)

	current := sweepProviderCall{batch: window.Batch, prepared: prepared, estimate: estimate,
		estimatedCost: estimatedCost, reservation: reservation, callOrdinal: 0,
		purpose: ProviderCallPurposeBrief}
	for {
		renewed, renewErr := w.Store.RenewPersonSweep(ctx, lease, w.Config.LeaseDuration)
		if renewErr != nil {
			return nil, "", lease, renewErr
		}
		if renewed == nil {
			return nil, "", lease, ErrLeaseLost
		}
		lease = *renewed
		started := w.now()
		marked := false
		updated, response, runErr := w.runPreparedWithLeaseHeartbeat(ctx, lease,
			func(markCtx context.Context) error {
				if markErr := w.Store.MarkPersonSweepBudgetStarted(
					markCtx, current.reservation, lease); markErr != nil {
					return markErr
				}
				marked = true
				return nil
			}, preparedCall)
		lease = updated
		if !marked {
			// The store never recorded the call as started, so the reservation
			// is still refundable and the attempt cannot be trusted to finish.
			_ = w.Store.ReleasePersonSweepBudget(ctx, current.reservation)
			return nil, "", lease, briefMarkFailure(runErr)
		}
		latency := max(time.Duration(0), w.now().Sub(started))
		if structuredResponseCompleted(response) {
			if recordErr := accounting.record(current, response, latency); recordErr != nil {
				return nil, briefFailureClass(recordErr), lease, briefFatalError(ctx, recordErr)
			}
		}

		var failure *ValidationFailure
		if runErr != nil {
			if !errors.As(runErr, &failure) || current.callOrdinal != 0 || failure.repair {
				return nil, briefFailureClass(runErr), lease, briefFatalError(ctx, runErr)
			}
		} else {
			parsed, parseErr := ParseBrief(response.Output, window, call.profile)
			if parseErr == nil {
				// Every item may have been dropped, leaving nothing to store.
				// The call is still paid for and reconciled; the attempt records
				// invalid_output so the empty result is visible in history.
				result, buildErr := renderBriefResult(parsed, window, w.now())
				if buildErr != nil {
					return nil, FailureInvalidOutput, lease, nil //nolint:nilerr // An empty brief is a recorded outcome, not an attempt failure.
				}
				// Brief text and citations do not independently establish a profile
				// fact. Keep possible attributes in the saved structure as suggestions.
				return &result, "", lease, nil
			}
			if current.callOrdinal != 0 {
				return nil, FailureInvalidOutput, lease, nil
			}
			semanticFailure, semanticErr := execution.SemanticValidationFailure(response)
			if semanticErr != nil {
				return nil, briefFailureClass(semanticErr), lease, briefFatalError(ctx, semanticErr)
			}
			failure = &semanticFailure
		}

		repair, repairErr := execution.PrepareRepair(*failure)
		if repairErr != nil {
			return nil, briefFailureClass(repairErr), lease, briefFatalError(ctx, repairErr)
		}
		repairCall, repairErr := execution.RepairCall(repair)
		if repairErr != nil {
			return nil, briefFailureClass(repairErr), lease, briefFatalError(ctx, repairErr)
		}
		repairEstimate, estimateErr := EstimateWireTokenReservation(
			repair.WireRequest(), window.Batch.Request.MaxOutputTokens)
		if estimateErr != nil {
			return nil, briefFailureClass(estimateErr), lease, briefFatalError(ctx, estimateErr)
		}
		repairCost, estimateErr := EstimateCostMicroUSD(repairEstimate, w.Config.Budgets)
		if estimateErr != nil {
			return nil, briefFailureClass(estimateErr), lease, briefFatalError(ctx, estimateErr)
		}
		repairReservation, reserveErr := w.Store.ReservePersonSweepBudget(ctx, BudgetReservationRequest{
			RunID: call.runID, AttemptID: call.attemptID, BatchOrdinal: call.batchOrdinal,
			CallOrdinal: 1, Purpose: ProviderCallPurposeBriefRepair, PersonID: lease.PersonID,
			ProviderFingerprint: call.profile.Fingerprint,
			UTCDate:             call.resolvedAt.UTC().Format(time.DateOnly),
			InputHash:           repair.WireSHA256(),
			ItemCount:           len(window.Batch.Packet.Seeds) + len(window.Batch.Packet.Context),
			EstimatedRequests:   1, EstimatedInputTokens: repairEstimate.InputTokens,
			EstimatedOutputTokens: repairEstimate.OutputTokens,
			EstimatedCostMicroUSD: repairCost, Budget: w.Config.Budgets,
		})
		if reserveErr != nil {
			return nil, briefFailureClass(reserveErr), lease, briefFatalError(ctx, reserveErr)
		}
		*reservations = append(*reservations, repairReservation)
		current = sweepProviderCall{batch: window.Batch, prepared: repair, estimate: repairEstimate,
			estimatedCost: repairCost, reservation: repairReservation, callOrdinal: 1,
			purpose: ProviderCallPurposeBriefRepair}
		preparedCall = repairCall
	}
}

// briefFatalError reports the causes that must fail the whole attempt rather
// than be recorded as a brief failure. A failed heartbeat, lost lease, or
// cancelled parent context prevents apply; a provider timeout does not.
func briefFatalError(ctx context.Context, cause error) error {
	if errors.Is(cause, ErrLeaseLost) || errors.Is(cause, errLeaseHeartbeat) {
		return cause
	}
	return ctx.Err()
}

// briefMarkFailure names the cause when the pre-call budget mark never
// happened. A nil cause still fails the attempt: the store refused to record
// the call and the reason is not observable here.
func briefMarkFailure(cause error) error {
	if cause != nil {
		return cause
	}
	return errors.New("person brief call could not be marked as started")
}

func briefFailureClass(cause error) FailureClass {
	class, _ := classifyPersonSweepFailure(cause)
	return class
}

// forcedSinglePersonBrief reports whether the request is a manual "generate
// this person's brief now": the one shape that both requires enrollment up
// front and publishes work of its own.
func (w *Worker) forcedSinglePersonBrief(request RunRequest) bool {
	return w.Brief != nil && request.Kind == RunManual && request.PersonID > 0 &&
		request.Brief.normalized() == BriefModeForce
}

// refuseForcedBrief rejects a manual brief request that could never produce a
// brief. planPersonBrief treats a disabled lane, a profile that refuses
// sensitive content, a profile with no brief lane, and a missing enrollment
// alike: it plans no brief and the attempt runs extraction only. That is the
// right answer for a scheduled run, which should not fail because one lane is
// off, but for an explicit "generate now" it is a silent no-op. So the same
// four gates are checked here, before the run row exists and before
// reconciliation publishes any work, and each one refuses with its own typed
// error.
func (w *Worker) refuseForcedBrief(
	ctx context.Context, personID int64, profile ProviderProfile,
) error {
	if !w.Config.Brief.IsEnabled() {
		return fmt.Errorf("person %d: %w", personID, ErrPersonBriefLaneDisabled)
	}
	if !profile.AllowSensitive {
		return fmt.Errorf("person %d: %w", personID, ErrPersonBriefPolicyRefused)
	}
	if len(briefWindowLanes(profile.AllowedSources)) == 0 {
		return fmt.Errorf("person %d: %w", personID, ErrPersonBriefNoSupportedLane)
	}
	_, enrolled, err := w.Brief.PersonSweepBriefEligibility(ctx, personID)
	if err != nil {
		return err
	}
	if !enrolled {
		return fmt.Errorf("person %d: %w", personID, ErrPersonBriefNotEnrolled)
	}
	return nil
}
