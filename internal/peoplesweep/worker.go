package peoplesweep

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

const (
	StatusOnlyProvider        = "msgvault_host"
	StatusOnlyProviderVersion = "person-sweep-evidence-status-v1"
	StatusOnlyModel           = "deterministic-evidence-status"
	StatusOnlyModelVersion    = "v1"
)

var ErrPersonSweepConsentRevoked = errors.New("person sweep provider consent is no longer active")

const personSweepCleanupTimeout = 5 * time.Second

type CompletedBatch struct {
	Ordinal            int
	CallOrdinal        int
	Purpose            string
	ReservationID      string
	InputHash          string
	ProviderRequestID  string
	ProviderVersion    string
	ModelVersion       string
	Usage              TokenUsage
	UsageKnown         bool
	ActualCostMicroUSD int64
	Latency            time.Duration
}

type CursorAdvance struct {
	Key                      CursorKey
	Mode                     GenerationCursorMode
	ExpectedSequence         int64
	NextSequence             int64
	ExpectedReconcileKey     string
	NextReconcileKey         string
	ExpectedDocumentKey      string
	NextDocumentKey          string
	ExpectedBackstopUpperKey string
	CapturedBackstopUpperKey string
	ReconciliationDone       bool
	BackstopComplete         bool
	EnvelopeHash             string
}

type ApplyRequest struct {
	Lease              Lease
	RunID              string
	AttemptID          string
	Generation         personfacts.GenerationInput
	CursorEnvelope     []GenerationCursor
	Batches            []CompletedBatch
	Usage              Usage
	Budget             BudgetConfig
	CursorAdvances     []CursorAdvance
	DeferredCursorWork bool
	CompletedAt        time.Time
}

type ApplyMutationMetadata struct {
	GenerationInserted         bool
	ClaimRowsInserted          int
	EvidenceStatusRowsInserted int
	ResolutionRowsInserted     int
	DecisionRowsInserted       int
	ProjectionRowsWritten      int
	VCardRevisionBumped        bool
	BatchRowsReconciled        int
	CursorRowsAdvanced         int
	AttemptRowsSucceeded       int
	WorkRowsUpdated            int
}

type ApplyResult struct {
	Generation personfacts.GenerationResult
	Mutations  ApplyMutationMetadata
}

type sequenceCursorCoordinate struct {
	Bound       string `json:"bound"`
	Sequence    int64  `json:"sequence"`
	DocumentKey string `json:"document_key,omitempty"`
}

type sourceKeyCursorCoordinate struct {
	Bound       string `json:"bound"`
	SourceKey   string `json:"source_key"`
	DocumentKey string `json:"document_key,omitempty"`
}

type encodedGenerationCursor struct {
	cursor GenerationCursor
	source personfacts.SourceCursor
}

func PersonFactSourceCursors(cursors []GenerationCursor) ([]personfacts.SourceCursor, string, error) {
	if len(cursors) == 0 {
		return nil, "", errors.New("person fact source cursors require at least one range")
	}
	encoded := make([]encodedGenerationCursor, 0, len(cursors))
	for _, cursor := range cursors {
		item, err := encodePersonFactSourceCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		encoded = append(encoded, encodedGenerationCursor{cursor: cursor, source: item})
	}
	sort.Slice(encoded, func(i, j int) bool {
		left, right := encoded[i].source, encoded[j].source
		if left.Lane != right.Lane {
			return left.Lane < right.Lane
		}
		if left.Start != right.Start {
			return left.Start < right.Start
		}
		return left.End < right.End
	})
	if err := rejectOverlappingPersonFactCursors(encoded); err != nil {
		return nil, "", err
	}
	result := make([]personfacts.SourceCursor, len(encoded))
	for i := range encoded {
		result[i] = encoded[i].source
	}
	canonical, err := json.Marshal(result)
	if err != nil {
		return nil, "", fmt.Errorf("encode person fact source cursor envelope: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return result, hex.EncodeToString(digest[:]), nil
}

func encodePersonFactSourceCursor(cursor GenerationCursor) (personfacts.SourceCursor, error) {
	if err := validateCursorKey(cursor.Key); err != nil {
		return personfacts.SourceCursor{}, err
	}
	lane := "person-sweep/v1/" + string(cursor.Key.SourceLane) + "/" + string(cursor.Mode)
	var start, end []byte
	var err error
	switch cursor.Mode {
	case GenerationCursorOptimistic:
		if cursor.CursorFrom < 0 || !sweepSequenceCoordinateAdvanced(cursor.CursorFrom,
			cursor.DocumentFromKey, cursor.CursorThrough, cursor.DocumentToKey) ||
			cursor.ReconcileFromKey != "" || cursor.ReconcileToKey != "" || cursor.BackstopUpperKey != "" {
			return personfacts.SourceCursor{}, errors.New("person sweep optimistic cursor has an invalid range")
		}
		start, err = json.Marshal(sequenceCursorCoordinate{Bound: "exclusive", Sequence: cursor.CursorFrom,
			DocumentKey: cursor.DocumentFromKey})
		if err == nil {
			end, err = json.Marshal(sequenceCursorCoordinate{Bound: "inclusive", Sequence: cursor.CursorThrough,
				DocumentKey: cursor.DocumentToKey})
		}
	case GenerationCursorReconciliation:
		if cursor.CursorFrom != 0 || cursor.CursorThrough != 0 ||
			!sweepCursorCoordinateAdvanced(cursor.ReconcileFromKey, cursor.DocumentFromKey,
				cursor.ReconcileToKey, cursor.DocumentToKey) || cursor.BackstopUpperKey != "" {
			return personfacts.SourceCursor{}, errors.New("person sweep source-key cursor has an invalid range")
		}
		start, err = json.Marshal(sourceKeyCursorCoordinate{Bound: "exclusive", SourceKey: cursor.ReconcileFromKey,
			DocumentKey: cursor.DocumentFromKey})
		if err == nil {
			end, err = json.Marshal(sourceKeyCursorCoordinate{Bound: "inclusive", SourceKey: cursor.ReconcileToKey,
				DocumentKey: cursor.DocumentToKey})
		}
	case GenerationCursorBackstop:
		if cursor.CursorFrom != 0 || cursor.CursorThrough != 0 ||
			!sweepCursorCoordinateAdvanced(cursor.ReconcileFromKey, cursor.DocumentFromKey,
				cursor.ReconcileToKey, cursor.DocumentToKey) || cursor.BackstopUpperKey == "" ||
			cursor.ReconcileToKey > cursor.BackstopUpperKey {
			return personfacts.SourceCursor{}, errors.New("person sweep backstop cursor has an invalid bounded range")
		}
		start, err = json.Marshal(sourceKeyCursorCoordinate{Bound: "exclusive", SourceKey: cursor.ReconcileFromKey,
			DocumentKey: cursor.DocumentFromKey})
		if err == nil {
			end, err = json.Marshal(sourceKeyCursorCoordinate{Bound: "inclusive", SourceKey: cursor.ReconcileToKey,
				DocumentKey: cursor.DocumentToKey})
		}
	default:
		return personfacts.SourceCursor{}, fmt.Errorf("person sweep cursor has unknown mode %q", cursor.Mode)
	}
	if err != nil {
		return personfacts.SourceCursor{}, fmt.Errorf("encode person sweep cursor range: %w", err)
	}
	if cursor.Key.SourceLane != SourceDocumentText &&
		(cursor.DocumentFromKey != "" || cursor.DocumentToKey != "") {
		return personfacts.SourceCursor{}, errors.New("person sweep document continuation requires document text")
	}
	return personfacts.SourceCursor{Lane: lane, Start: string(start), End: string(end)}, nil
}

func validateCursorKey(key CursorKey) error {
	if key.PersonID <= 0 || key.ProgramFingerprint == "" || key.CatalogFingerprint == "" {
		return errors.New("person sweep cursor key requires person and fingerprints")
	}
	switch key.SourceLane {
	case SourceConversationText, SourceMeetingText, SourceAttachmentCaption,
		SourceAttachmentOCR, SourceDocumentText:
		return nil
	default:
		return fmt.Errorf("person sweep cursor key has unknown source lane %q", key.SourceLane)
	}
}

func rejectOverlappingPersonFactCursors(cursors []encodedGenerationCursor) error {
	type rangeKey struct {
		Key  CursorKey
		Mode GenerationCursorMode
	}
	byKey := make(map[rangeKey][]GenerationCursor)
	for _, item := range cursors {
		cursor := item.cursor
		key := rangeKey{Key: cursor.Key, Mode: cursor.Mode}
		byKey[key] = append(byKey[key], cursor)
	}
	for _, ranges := range byKey {
		sort.Slice(ranges, func(i, j int) bool {
			if ranges[i].Mode == GenerationCursorOptimistic {
				if ranges[i].CursorFrom != ranges[j].CursorFrom {
					return ranges[i].CursorFrom < ranges[j].CursorFrom
				}
				if ranges[i].DocumentFromKey != ranges[j].DocumentFromKey {
					return ranges[i].DocumentFromKey < ranges[j].DocumentFromKey
				}
				if ranges[i].CursorThrough != ranges[j].CursorThrough {
					return ranges[i].CursorThrough < ranges[j].CursorThrough
				}
				return ranges[i].DocumentToKey < ranges[j].DocumentToKey
			}
			if ranges[i].ReconcileFromKey != ranges[j].ReconcileFromKey {
				return ranges[i].ReconcileFromKey < ranges[j].ReconcileFromKey
			}
			if ranges[i].DocumentFromKey != ranges[j].DocumentFromKey {
				return ranges[i].DocumentFromKey < ranges[j].DocumentFromKey
			}
			if ranges[i].ReconcileToKey != ranges[j].ReconcileToKey {
				return ranges[i].ReconcileToKey < ranges[j].ReconcileToKey
			}
			return ranges[i].DocumentToKey < ranges[j].DocumentToKey
		})
		for i := 1; i < len(ranges); i++ {
			if ranges[i].Mode == GenerationCursorOptimistic {
				if ranges[i].CursorFrom < ranges[i-1].CursorThrough ||
					(ranges[i].CursorFrom == ranges[i-1].CursorThrough &&
						ranges[i].DocumentFromKey < ranges[i-1].DocumentToKey) {
					return errors.New("person sweep cursor ranges overlap or duplicate")
				}
			} else if ranges[i].ReconcileFromKey < ranges[i-1].ReconcileToKey ||
				(ranges[i].ReconcileFromKey == ranges[i-1].ReconcileToKey &&
					ranges[i].DocumentFromKey < ranges[i-1].DocumentToKey) {
				return errors.New("person sweep cursor ranges overlap or duplicate")
			}
		}
	}
	return nil
}

func ValidatePersonFactCursorBinding(
	generation personfacts.GenerationInput, cursors []GenerationCursor, advances []CursorAdvance,
) (string, error) {
	sources, envelopeHash, err := PersonFactSourceCursors(cursors)
	if err != nil {
		return "", err
	}
	for _, cursor := range cursors {
		if cursor.Key.PersonID != generation.PersonID ||
			cursor.Key.ProgramFingerprint != generation.ProgramFingerprint ||
			cursor.Key.CatalogFingerprint != generation.CatalogFingerprint {
			return "", errors.New("person sweep cursor does not match generation identity")
		}
	}
	if !reflect.DeepEqual(sources, generation.SourceCursors) {
		return "", errors.New("person sweep generation source cursors do not match cursor envelope")
	}
	if len(advances) != len(cursors) {
		return "", errors.New("person sweep cursor advances do not exactly cover envelope")
	}
	used := make([]bool, len(advances))
	for _, cursor := range cursors {
		matched := -1
		for i, advance := range advances {
			if !used[i] && advance.Key == cursor.Key && advance.Mode == cursor.Mode &&
				advance.EnvelopeHash == envelopeHash && advanceMatchesCursor(advance, cursor) {
				if matched >= 0 {
					return "", errors.New("person sweep cursor advance is duplicated")
				}
				matched = i
			}
		}
		if matched < 0 || used[matched] {
			return "", errors.New("person sweep cursor advance is missing or duplicated")
		}
		used[matched] = true
	}
	return envelopeHash, nil
}

func advanceMatchesCursor(advance CursorAdvance, cursor GenerationCursor) bool {
	switch cursor.Mode {
	case GenerationCursorOptimistic:
		return advance.ExpectedSequence == cursor.CursorFrom &&
			advance.NextSequence == cursor.CursorThrough &&
			advance.ExpectedDocumentKey == cursor.DocumentFromKey &&
			advance.NextDocumentKey == cursor.DocumentToKey &&
			advance.ExpectedReconcileKey == "" && advance.NextReconcileKey == "" &&
			!advance.ReconciliationDone && !advance.BackstopComplete
	case GenerationCursorReconciliation:
		return advance.ExpectedSequence == advance.NextSequence &&
			advance.ExpectedReconcileKey == cursor.ReconcileFromKey &&
			advance.NextReconcileKey == cursor.ReconcileToKey &&
			advance.ExpectedDocumentKey == cursor.DocumentFromKey &&
			advance.NextDocumentKey == cursor.DocumentToKey && !advance.BackstopComplete
	case GenerationCursorBackstop:
		return advance.ExpectedSequence == advance.NextSequence &&
			advance.ExpectedReconcileKey == cursor.ReconcileFromKey &&
			advance.NextReconcileKey == cursor.ReconcileToKey &&
			advance.ExpectedDocumentKey == cursor.DocumentFromKey &&
			advance.NextDocumentKey == cursor.DocumentToKey &&
			advance.CapturedBackstopUpperKey == cursor.BackstopUpperKey &&
			!advance.ReconciliationDone &&
			advance.BackstopComplete == (cursor.ReconcileToKey == cursor.BackstopUpperKey &&
				cursor.DocumentToKey == "")
	default:
		return false
	}
}

type WorkStore interface {
	StartPersonSweepRun(ctx context.Context, input StartRun) (Run, error)
	FinishPersonSweepRun(ctx context.Context, runID string, status RunStatus, completedAt time.Time) error
	StartPersonSweepAttempt(ctx context.Context, input StartAttempt) error
	ReconcilePersonSweepWorkContext(ctx context.Context, request GapRequest) (GapResult, error)
	ClaimPersonSweep(ctx context.Context, request ClaimRequest) (*Lease, error)
	RenewPersonSweep(ctx context.Context, lease Lease, duration time.Duration) (*Lease, error)
	EnsurePersonSweepCursors(ctx context.Context, keys []CursorKey) ([]Cursor, error)
	ReservePersonSweepBudget(ctx context.Context, input BudgetReservationRequest) (BudgetReservation, error)
	ReleasePersonSweepBudget(ctx context.Context, reservation BudgetReservation) error
	MarkPersonSweepBudgetStarted(ctx context.Context, reservation BudgetReservation, lease Lease) error
	FailPersonSweepWork(ctx context.Context, failure WorkFailure) error
	FinalizePersonSweepFailure(ctx context.Context, input FailureFinalization) error
}

type ClaimSink interface {
	ApplyPersonSweep(ctx context.Context, request ApplyRequest) (ApplyResult, error)
}

type CatalogSource interface {
	BuildPersonFactCatalogContext(ctx context.Context, includeSensitive bool) (personfacts.Catalog, error)
}

type RunRequest struct {
	Kind     RunKind
	Mode     RunMode
	PersonID int64
	Limit    int
}
type RunResult struct {
	RunID                                             string
	PeopleAttempted, PeopleSucceeded, ProjectedWrites int
	Usage                                             Usage
}
type PersonRunResult struct {
	PersonID        int64
	AttemptID       string
	ProjectedWrites int
	CursorAdvances  []CursorAdvance
	Usage           Usage
}

type Worker struct {
	Config   Config
	Store    WorkStore
	Source   AssemblySource
	Context  ContextRetriever
	Sink     ClaimSink
	Runner   StructuredRunner
	Catalog  CatalogSource
	Clock    func() time.Time
	NewID    func() string
	WorkerID string
}

func personSweepAttemptEnvelopeHash(cursors []GenerationCursor) (string, error) {
	encoded, err := json.Marshal(cursors)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalProviderIdentity(value string) bool {
	return strings.TrimSpace(value) != "" && IsSafeProviderMetadata(value)
}

func (w *Worker) Run(ctx context.Context, request RunRequest) (RunResult, error) {
	profile, catalog, now, err := w.ready(ctx)
	if err != nil {
		return RunResult{}, err
	}
	if request.Kind != RunScheduled && request.Kind != RunManual {
		return RunResult{}, errors.New("person sweep worker requires a valid run kind")
	}
	if request.Mode != RunIncremental && request.Mode != RunBackstop {
		return RunResult{}, errors.New("person sweep worker requires a valid run mode")
	}
	limit := request.Limit
	if limit <= 0 || limit > w.Config.WorkBatchSize {
		limit = w.Config.WorkBatchSize
	}
	runID := w.NewID()
	if runID == "" {
		return RunResult{}, errors.New("person sweep worker generated an empty run ID")
	}
	_, err = w.Store.StartPersonSweepRun(ctx, StartRun{ID: runID, Kind: request.Kind,
		Mode: request.Mode, ProgramFingerprint: ProgramFingerprint(),
		CatalogFingerprint: catalog.Fingerprint, ProviderFingerprint: profile.Fingerprint,
		StartedAt: now})
	if err != nil {
		return RunResult{}, err
	}
	result := RunResult{RunID: runID}
	if err := w.reconcile(ctx, profile, catalog, now, request.Mode); err != nil {
		cleanupCtx, cancel := personSweepCleanupContext(ctx)
		defer cancel()
		finishErr := w.Store.FinishPersonSweepRun(cleanupCtx, runID, RunFailed, w.now())
		return result, errors.Join(err, finishErr)
	}
	var firstErr error
	for result.PeopleAttempted < limit {
		lease, claimErr := w.Store.ClaimPersonSweep(ctx, ClaimRequest{
			WorkerID: w.WorkerID, LeaseDuration: w.Config.LeaseDuration,
			AvailableAt: w.now(), PersonID: request.PersonID,
		})
		if claimErr != nil {
			firstErr = claimErr
			break
		}
		if lease == nil {
			break
		}
		if request.PersonID > 0 && lease.PersonID != request.PersonID {
			firstErr = errors.New("person sweep worker claimed a different requested person")
			break
		}
		result.PeopleAttempted++
		person, personErr := w.runPerson(ctx, runID, *lease, request.Mode, profile, catalog, now)
		if personErr != nil {
			if firstErr == nil {
				firstErr = personErr
			}
			continue
		}
		result.PeopleSucceeded++
		result.ProjectedWrites += person.ProjectedWrites
		result.Usage, err = addUsage(result.Usage, person.Usage)
		if err != nil {
			firstErr = err
			break
		}
	}
	status := RunSucceeded
	if firstErr != nil && result.PeopleSucceeded == 0 {
		status = RunFailed
	} else if firstErr != nil || result.PeopleSucceeded != result.PeopleAttempted {
		status = RunPartial
	}
	cleanupCtx, cancel := personSweepCleanupContext(ctx)
	defer cancel()
	if err := w.Store.FinishPersonSweepRun(cleanupCtx, runID, status, w.now()); err != nil {
		firstErr = errors.Join(firstErr, err)
	}
	return result, firstErr
}

func (w *Worker) RunPerson(
	ctx context.Context, runID string, lease Lease, mode RunMode,
) (PersonRunResult, error) {
	profile, catalog, resolvedAt, err := w.ready(ctx)
	if err != nil {
		return PersonRunResult{}, w.failClaim(ctx, lease, "", err)
	}
	return w.runPerson(ctx, runID, lease, mode, profile, catalog, resolvedAt)
}

func (w *Worker) runPerson(
	ctx context.Context,
	runID string,
	lease Lease,
	mode RunMode,
	profile ProviderProfile,
	catalog personfacts.Catalog,
	resolvedAt time.Time,
) (PersonRunResult, error) {
	keys := make([]CursorKey, 0, len(profile.AllowedSources))
	for _, lane := range profile.AllowedSources {
		keys = append(keys, CursorKey{PersonID: lease.PersonID, SourceLane: lane,
			ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: catalog.Fingerprint})
	}
	cursors, err := w.Store.EnsurePersonSweepCursors(ctx, keys)
	if err != nil {
		return PersonRunResult{}, w.failClaim(ctx, lease, "", err)
	}
	maxBatches := min(w.Config.Budgets.MaxRequestsPerPerson,
		int(w.Config.Budgets.MaxOutputTokensPerPerson/extractionMaxOutputTokens))
	assemblyRequest := AssemblyRequest{PersonID: lease.PersonID, Cursors: cursors, Catalog: catalog,
		Profile: profile, Now: resolvedAt, BackstopInterval: w.Config.BackstopInterval,
		ForceBackstop: mode == RunBackstop}
	buildAssembly := func(selected []Cursor, maxProgressWindows int) (Assembly, error) {
		request := assemblyRequest
		request.Cursors = selected
		return (Assembler{Source: w.Source, Context: w.Context,
			MaxBytes: w.Config.EvidenceMaxBytes,
			MaxItems: w.Config.EvidenceMaxItems, WindowLimit: w.Config.ChangeBatchSize,
			MaxBatches: maxBatches, MaxProgressWindows: maxProgressWindows,
			ContextPerTarget:     w.Config.ContextPerTarget,
			HistoricalMessageCap: w.Config.HistoricalMessageCap}).Build(ctx, request)
	}
	assembly, err := buildAssembly(cursors, 0)
	if err != nil && !errors.Is(err, ErrNoChangedSeed) {
		return PersonRunResult{}, w.failClaim(ctx, lease, "", err)
	}
	deferredCursorWork := false
	if len(assembly.Batches) > maxBatches {
		bounded := false
		for index := range cursors {
			candidate, candidateErr := buildAssembly(cursors[index:index+1], 0)
			if candidateErr != nil && !errors.Is(candidateErr, ErrNoChangedSeed) {
				return PersonRunResult{}, w.failClaim(ctx, lease, "", candidateErr)
			}
			if len(candidate.CursorEnvelope) == 0 {
				continue
			}
			if len(candidate.Batches) > maxBatches {
				candidate, candidateErr = buildAssembly(cursors[index:index+1], 1)
				if candidateErr != nil && !errors.Is(candidateErr, ErrNoChangedSeed) {
					return PersonRunResult{}, w.failClaim(ctx, lease, "", candidateErr)
				}
			}
			if len(candidate.Batches) > maxBatches {
				return PersonRunResult{}, w.failClaim(ctx, lease, "",
					errors.New("person sweep assembly exceeded its request batch limit"))
			}
			assembly, bounded = candidate, true
			deferredCursorWork = true
			break
		}
		if !bounded {
			return PersonRunResult{}, w.failClaim(ctx, lease, "",
				errors.New("person sweep assembly could not select bounded cursor progress"))
		}
	}
	if len(assembly.CursorEnvelope) == 0 {
		return PersonRunResult{}, w.failClaim(ctx, lease, "",
			errors.New("person sweep assembly has no cursor progress"))
	}
	sourceCursors, sourceHash, err := PersonFactSourceCursors(assembly.CursorEnvelope)
	if err != nil {
		return PersonRunResult{}, w.failClaim(ctx, lease, "", err)
	}
	advances, err := cursorAdvances(assembly.CursorEnvelope, cursors, sourceHash)
	if err != nil {
		return PersonRunResult{}, w.failClaim(ctx, lease, "", err)
	}
	attemptHash, err := personSweepAttemptEnvelopeHash(assembly.CursorEnvelope)
	if err != nil {
		return PersonRunResult{}, w.failClaim(ctx, lease, "", err)
	}
	attemptID := w.NewID()
	if attemptID == "" {
		return PersonRunResult{}, w.failClaim(ctx, lease, "",
			errors.New("person sweep worker generated an empty attempt ID"))
	}
	if err := w.Store.StartPersonSweepAttempt(ctx, StartAttempt{ID: attemptID, RunID: runID,
		PersonID: lease.PersonID, LeaseFence: lease.Fence, Mode: mode,
		CursorEnvelope: assembly.CursorEnvelope, EnvelopeHash: attemptHash, StartedAt: resolvedAt}); err != nil {
		return PersonRunResult{}, w.failClaim(ctx, lease, attemptID, err)
	}

	reservations := make([]BudgetReservation, 0, len(assembly.Batches))
	type admittedBatch struct {
		batch         PacketBatch
		prepared      PreparedStructuredRequest
		estimate      TokenUsage
		estimatedCost int64
		reservation   BudgetReservation
		callOrdinal   int
		purpose       string
	}
	admitted := make([]admittedBatch, 0, len(assembly.Batches))
	completedUsage := make([]CompletedUsage, 0, len(assembly.Batches)*2)
	completedBatches := make([]CompletedBatch, 0, len(assembly.Batches)*2)
	claims := make([]personfacts.ProposedClaim, 0)
	totalUsage := Usage{}
	providerVersion, modelVersion := "", ""
	// Prepare and reserve the whole immutable request set before the first paid
	// provider call. A later batch must never discover a run/person/day budget
	// violation after an earlier batch has already crossed the network boundary.
	for _, batch := range assembly.Batches {
		renewed, renewErr := w.Store.RenewPersonSweep(ctx, lease, w.Config.LeaseDuration)
		if renewErr != nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID, reservations,
				completedUsage, renewErr, resolvedAt)
		}
		if renewed == nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID, reservations,
				completedUsage, ErrLeaseLost, resolvedAt)
		}
		lease = *renewed
		prepared, prepareErr := w.Runner.PrepareStructured(ctx, batch.Request)
		if prepareErr != nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID, reservations,
				completedUsage, prepareErr, resolvedAt)
		}
		estimate, estimateErr := EstimateWireTokenReservation(
			prepared.WireRequest(), batch.Request.MaxOutputTokens)
		if estimateErr != nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID, reservations,
				completedUsage, estimateErr, resolvedAt)
		}
		estimatedCost, estimateErr := EstimateCostMicroUSD(estimate, w.Config.Budgets)
		if estimateErr != nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID, reservations,
				completedUsage, estimateErr, resolvedAt)
		}
		reservation, reserveErr := w.Store.ReservePersonSweepBudget(ctx, BudgetReservationRequest{
			RunID: runID, AttemptID: attemptID, BatchOrdinal: batch.Ordinal, CallOrdinal: 0,
			Purpose:  ProviderCallPurposePrimary,
			PersonID: lease.PersonID, ProviderFingerprint: profile.Fingerprint,
			UTCDate: resolvedAt.UTC().Format(time.DateOnly), InputHash: prepared.WireSHA256(),
			ItemCount: len(batch.Packet.Seeds) + len(batch.Packet.Context), EstimatedRequests: 1,
			EstimatedInputTokens: estimate.InputTokens, EstimatedOutputTokens: estimate.OutputTokens,
			EstimatedCostMicroUSD: estimatedCost, Budget: w.Config.Budgets,
		})
		if reserveErr != nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID, reservations,
				completedUsage, reserveErr, resolvedAt)
		}
		reservations = append(reservations, reservation)
		admitted = append(admitted, admittedBatch{batch: batch, prepared: prepared,
			estimate: estimate, estimatedCost: estimatedCost, reservation: reservation,
			callOrdinal: 0, purpose: ProviderCallPurposePrimary})
	}
	executions := make([]StructuredExecutionSession, len(admitted))
	primaryCalls := make([]PreparedStructuredCall, len(admitted))
	for index := range admitted {
		execution, beginErr := w.Runner.BeginStructuredExecution(ctx, admitted[index].prepared)
		if beginErr != nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID,
				reservations, completedUsage, beginErr, resolvedAt)
		}
		primaryCall, callErr := execution.PrimaryCall(admitted[index].prepared)
		if callErr != nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID,
				reservations, completedUsage, callErr, resolvedAt)
		}
		executions[index] = execution
		primaryCalls[index] = primaryCall
	}
	recordCompletedCall := func(call admittedBatch, response StructuredResponse, latency time.Duration) error {
		// Validate and account before recording anything. A response rejected
		// below is untrusted, and a completed usage record derived from it would
		// either fail failure finalization outright (stranding the attempt and
		// lease until expiry) or write untrusted values into durable history.
		// Finalizing without a record lets the store conservatively charge the
		// reservation instead.
		if response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 {
			return invalidOutputError{errors.New("provider returned negative token usage")}
		}
		if !canonicalProviderIdentity(response.ProviderVersion) ||
			!canonicalProviderIdentity(response.ModelVersion) ||
			!IsSafeProviderMetadata(response.ProviderRequestID) {
			return invalidOutputError{errors.New("provider returned unsafe identity metadata")}
		}
		if providerVersion == "" {
			providerVersion, modelVersion = response.ProviderVersion, response.ModelVersion
		} else if providerVersion != response.ProviderVersion || modelVersion != response.ModelVersion {
			return invalidOutputError{errors.New("provider call identities differ")}
		}
		accounted, actualCost, accountErr := accountCompletedProviderCall(
			response, call.estimate, call.estimatedCost, w.Config.Budgets)
		if accountErr != nil {
			return accountErr
		}
		accountedTotal, accountErr := addUsage(totalUsage, accounted)
		if accountErr != nil {
			return accountErr
		}
		completedUsage = append(completedUsage, CompletedUsage{
			BatchOrdinal: call.batch.Ordinal, CallOrdinal: call.callOrdinal, Purpose: call.purpose,
			ProviderRequestID: response.ProviderRequestID, Usage: accountableTokenUsage(response.Usage),
			UsageKnown: response.UsageKnown, Latency: latency,
		})
		completedBatches = append(completedBatches, CompletedBatch{
			Ordinal: call.batch.Ordinal, CallOrdinal: call.callOrdinal, Purpose: call.purpose,
			ReservationID: call.reservation.ID, InputHash: call.prepared.WireSHA256(),
			ProviderRequestID: response.ProviderRequestID, ProviderVersion: response.ProviderVersion,
			ModelVersion: response.ModelVersion, Usage: accountableTokenUsage(response.Usage),
			UsageKnown: response.UsageKnown, ActualCostMicroUSD: actualCost, Latency: latency,
		})
		totalUsage = accountedTotal
		return nil
	}
	for primaryIndex, primary := range admitted {
		execution := executions[primaryIndex]
		call := primary
		preparedCall := primaryCalls[primaryIndex]
		for {
			renewed, renewErr := w.Store.RenewPersonSweep(ctx, lease, w.Config.LeaseDuration)
			if renewErr != nil {
				return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
					completedUsage, renewErr, resolvedAt)
			}
			if renewed == nil {
				return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
					completedUsage, ErrLeaseLost, resolvedAt)
			}
			lease = *renewed
			started := w.now()
			var response StructuredResponse
			var runErr error
			marked := false
			lease, response, runErr = w.runPreparedWithLeaseHeartbeat(ctx, lease,
				func(markCtx context.Context) error {
					if markErr := w.Store.MarkPersonSweepBudgetStarted(markCtx, call.reservation, lease); markErr != nil {
						return markErr
					}
					marked = true
					return nil
				}, preparedCall)
			if !marked {
				_ = w.Store.ReleasePersonSweepBudget(ctx, call.reservation)
				return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
					completedUsage, runErr, resolvedAt)
			}
			latency := max(time.Duration(0), w.now().Sub(started))
			if structuredResponseCompleted(response) {
				if recordErr := recordCompletedCall(call, response, latency); recordErr != nil {
					return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
						completedUsage, recordErr, resolvedAt)
				}
			}

			var failure *ValidationFailure
			if runErr != nil {
				if !errors.As(runErr, &failure) || call.callOrdinal != 0 || failure.repair {
					return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
						completedUsage, runErr, resolvedAt)
				}
			} else {
				parsed, parseErr := ParseExtraction(response.Output, call.batch, profile)
				if parseErr == nil {
					claims = append(claims, parsed...)
					break
				}
				if call.callOrdinal != 0 {
					failure = newValidationFailure(response.Output,
						"candidate failed extraction semantics", true)
					return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
						completedUsage, failure, resolvedAt)
				}
				semanticFailure, semanticErr := execution.SemanticValidationFailure(response)
				if semanticErr != nil {
					return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
						completedUsage, semanticErr, resolvedAt)
				}
				failure = &semanticFailure
			}

			repair, repairErr := execution.PrepareRepair(*failure)
			if repairErr != nil {
				return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
					completedUsage, repairErr, resolvedAt)
			}
			repairCall, repairErr := execution.RepairCall(repair)
			if repairErr != nil {
				return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
					completedUsage, repairErr, resolvedAt)
			}
			repairEstimate, estimateErr := EstimateWireTokenReservation(
				repair.WireRequest(), primary.batch.Request.MaxOutputTokens)
			if estimateErr != nil {
				return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
					completedUsage, estimateErr, resolvedAt)
			}
			repairCost, estimateErr := EstimateCostMicroUSD(repairEstimate, w.Config.Budgets)
			if estimateErr != nil {
				return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
					completedUsage, estimateErr, resolvedAt)
			}
			repairReservation, reserveErr := w.Store.ReservePersonSweepBudget(ctx, BudgetReservationRequest{
				RunID: runID, AttemptID: attemptID, BatchOrdinal: primary.batch.Ordinal,
				CallOrdinal: 1, Purpose: ProviderCallPurposeRepair,
				PersonID: lease.PersonID, ProviderFingerprint: profile.Fingerprint,
				UTCDate: resolvedAt.UTC().Format(time.DateOnly), InputHash: repair.WireSHA256(),
				ItemCount:         len(primary.batch.Packet.Seeds) + len(primary.batch.Packet.Context),
				EstimatedRequests: 1, EstimatedInputTokens: repairEstimate.InputTokens,
				EstimatedOutputTokens: repairEstimate.OutputTokens,
				EstimatedCostMicroUSD: repairCost, Budget: w.Config.Budgets,
			})
			if reserveErr != nil {
				return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
					completedUsage, reserveErr, resolvedAt)
			}
			reservations = append(reservations, repairReservation)
			call = admittedBatch{batch: primary.batch, prepared: repair, estimate: repairEstimate,
				estimatedCost: repairCost, reservation: repairReservation,
				callOrdinal: 1, purpose: ProviderCallPurposeRepair}
			preparedCall = repairCall
		}
	}
	provider, model := string(profile.Protocol), profile.Model
	if len(completedBatches) == 0 {
		provider, providerVersion = StatusOnlyProvider, StatusOnlyProviderVersion
		model, modelVersion = StatusOnlyModel, StatusOnlyModelVersion
		claims = nil
	}
	generation := personfacts.GenerationInput{PersonID: lease.PersonID,
		SourceCursors: sourceCursors, ProgramID: ExtractionProgramID,
		ProgramVersion: ExtractionProgramVersion, ProgramFingerprint: ProgramFingerprint(),
		CatalogFingerprint: catalog.Fingerprint, Provider: provider, ProviderVersion: providerVersion,
		Model: model, ModelVersion: modelVersion, ResolvedAt: resolvedAt,
		Policy: personfacts.PolicyContext{AllowSensitive: profile.AllowSensitive,
			ProviderPolicyFingerprint: profile.Fingerprint}, Claims: claims,
		EvidenceStatusChanges: assembly.EvidenceStatusChanges}
	renewed, renewErr := w.Store.RenewPersonSweep(ctx, lease, w.Config.LeaseDuration)
	if renewErr != nil {
		return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
			completedUsage, renewErr, resolvedAt)
	}
	if renewed == nil {
		return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
			completedUsage, ErrLeaseLost, resolvedAt)
	}
	lease = *renewed
	apply, applyErr := w.Sink.ApplyPersonSweep(ctx, ApplyRequest{Lease: lease, RunID: runID,
		AttemptID: attemptID, Generation: generation, CursorEnvelope: assembly.CursorEnvelope,
		Batches: completedBatches, Usage: totalUsage, Budget: w.Config.Budgets, CursorAdvances: advances,
		DeferredCursorWork: deferredCursorWork, CompletedAt: w.now()})
	if applyErr != nil {
		return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
			completedUsage, applyErr, resolvedAt)
	}
	return PersonRunResult{PersonID: lease.PersonID, AttemptID: attemptID,
		ProjectedWrites: apply.Mutations.ProjectionRowsWritten,
		CursorAdvances:  advances, Usage: totalUsage}, nil
}

type leaseHeartbeatResult struct {
	lease Lease
	err   error
}

func (w *Worker) runPreparedWithLeaseHeartbeat(
	ctx context.Context,
	lease Lease,
	markStarted func(context.Context) error,
	call PreparedStructuredCall,
) (Lease, StructuredResponse, error) {
	heartbeatCtx, cancel := context.WithCancel(ctx)
	stop := make(chan struct{})
	result := make(chan leaseHeartbeatResult, 1)
	interval := max(time.Millisecond, w.Config.LeaseDuration/3)
	go func(current Lease) {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				result <- leaseHeartbeatResult{lease: current}
				return
			case <-ticker.C:
				renewed, err := w.Store.RenewPersonSweep(heartbeatCtx, current, w.Config.LeaseDuration)
				if err != nil || renewed == nil {
					select {
					case <-stop:
						result <- leaseHeartbeatResult{lease: current}
					default:
						if err == nil {
							err = ErrLeaseLost
						}
						result <- leaseHeartbeatResult{lease: current, err: err}
					}
					cancel()
					return
				}
				current = *renewed
			}
		}
	}(lease)
	response, runErr := call.Execute(heartbeatCtx, markStarted)
	close(stop)
	cancel()
	heartbeat := <-result
	if heartbeat.err != nil {
		return heartbeat.lease, response, heartbeat.err
	}
	return heartbeat.lease, response, runErr
}

func (w *Worker) ready(ctx context.Context) (ProviderProfile, personfacts.Catalog, time.Time, error) {
	if w.Store == nil || w.Source == nil || w.Sink == nil || w.Runner == nil || w.Catalog == nil ||
		w.Clock == nil || w.NewID == nil || strings.TrimSpace(w.WorkerID) == "" {
		return ProviderProfile{}, personfacts.Catalog{}, time.Time{}, errors.New("person sweep worker dependencies are incomplete")
	}
	profile, err := w.Config.Profile()
	if err != nil {
		return ProviderProfile{}, personfacts.Catalog{}, time.Time{}, err
	}
	if w.Config.ContextPerTarget > 0 && w.Context == nil {
		return ProviderProfile{}, personfacts.Catalog{}, time.Time{},
			errors.New("person sweep worker context retriever is required")
	}
	catalog, err := w.Catalog.BuildPersonFactCatalogContext(ctx, profile.AllowSensitive)
	if err != nil {
		return ProviderProfile{}, personfacts.Catalog{}, time.Time{}, err
	}
	return profile, catalog, w.now(), nil
}

func (w *Worker) now() time.Time { return w.Clock().UTC() }

func (w *Worker) reconcile(ctx context.Context, profile ProviderProfile,
	catalog personfacts.Catalog, now time.Time, mode RunMode,
) error {
	after := int64(0)
	for {
		page, err := w.Store.ReconcilePersonSweepWorkContext(ctx, GapRequest{
			ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: catalog.Fingerprint,
			SourceLanes: profile.AllowedSources, AfterPersonID: after,
			Limit: w.Config.WorkBatchSize, Now: now, BackstopInterval: w.Config.BackstopInterval,
			ForceBackstop: mode == RunBackstop,
		})
		if err != nil {
			return err
		}
		if page.PeopleScanned == 0 || page.NextPersonID <= after {
			return nil
		}
		after = page.NextPersonID
	}
}

func cursorAdvances(envelope []GenerationCursor, cursors []Cursor, hash string) ([]CursorAdvance, error) {
	byKey := make(map[CursorKey]Cursor, len(cursors))
	for _, cursor := range cursors {
		byKey[cursor.Key] = cursor
	}
	result := make([]CursorAdvance, 0, len(envelope))
	for _, item := range envelope {
		current, ok := byKey[item.Key]
		if !ok {
			return nil, errors.New("person sweep generation cursor has no durable cursor")
		}
		advance := CursorAdvance{Key: item.Key, Mode: item.Mode, EnvelopeHash: hash}
		switch item.Mode {
		case GenerationCursorOptimistic:
			advance.ExpectedSequence, advance.NextSequence = item.CursorFrom, item.CursorThrough
			advance.ExpectedDocumentKey, advance.NextDocumentKey = item.DocumentFromKey, item.DocumentToKey
		case GenerationCursorReconciliation:
			advance.ExpectedSequence, advance.NextSequence = current.OptimisticSequence, current.OptimisticSequence
			advance.ExpectedReconcileKey, advance.NextReconcileKey = item.ReconcileFromKey, item.ReconcileToKey
			advance.ExpectedDocumentKey, advance.NextDocumentKey = item.DocumentFromKey, item.DocumentToKey
			advance.ReconciliationDone = item.ReconcileToKey >= current.ReconcileUpperKey && item.DocumentToKey == ""
		case GenerationCursorBackstop:
			advance.ExpectedSequence, advance.NextSequence = current.OptimisticSequence, current.OptimisticSequence
			advance.ExpectedReconcileKey, advance.NextReconcileKey = item.ReconcileFromKey, item.ReconcileToKey
			advance.ExpectedDocumentKey, advance.NextDocumentKey = item.DocumentFromKey, item.DocumentToKey
			advance.ExpectedBackstopUpperKey = current.BackstopUpperKey
			advance.CapturedBackstopUpperKey = item.BackstopUpperKey
			advance.BackstopComplete = item.ReconcileToKey == item.BackstopUpperKey && item.DocumentToKey == ""
		default:
			return nil, errors.New("person sweep generation cursor has unknown mode")
		}
		result = append(result, advance)
	}
	return result, nil
}

type invalidOutputError struct{ error }

func (w *Worker) finalizeFailure(ctx context.Context, lease Lease, attemptID string,
	reservations []BudgetReservation, completed []CompletedUsage, cause error,
	startedAt time.Time,
) error {
	class, retryAfter := classifyPersonSweepFailure(cause)
	now := w.now()
	retryAt := now.Add(personSweepRetryDelay(attemptID, lease.AttemptCount, retryAfter,
		w.Config.RetryBase, w.Config.RetryMax))
	cleanupCtx, cancel := personSweepCleanupContext(ctx)
	defer cancel()
	err := w.Store.FinalizePersonSweepFailure(cleanupCtx, FailureFinalization{Lease: lease,
		AttemptID: attemptID, Class: class, RetryAt: retryAt,
		Reservations: append([]BudgetReservation(nil), reservations...),
		Completed:    append([]CompletedUsage(nil), completed...), FinalizedAt: now})
	if err != nil {
		return errors.Join(cause, err)
	}
	_ = startedAt
	return cause
}

func (w *Worker) finalizePreflightFailure(ctx context.Context, lease Lease, attemptID string,
	reservations []BudgetReservation, completed []CompletedUsage, cause error,
	startedAt time.Time,
) error {
	cleanupCtx, cancel := personSweepCleanupContext(ctx)
	defer cancel()
	var releaseErr error
	for _, reservation := range reservations {
		releaseErr = errors.Join(releaseErr,
			w.Store.ReleasePersonSweepBudget(cleanupCtx, reservation))
	}
	return w.finalizeFailure(ctx, lease, attemptID, reservations, completed,
		errors.Join(cause, releaseErr), startedAt)
}

func (w *Worker) failClaim(ctx context.Context, lease Lease, attemptID string, cause error) error {
	if w.Store == nil || w.Clock == nil {
		return cause
	}
	class, retryAfter := classifyPersonSweepFailure(cause)
	now := w.now()
	retryAt := now.Add(personSweepRetryDelay(attemptID, lease.AttemptCount, retryAfter,
		w.Config.RetryBase, w.Config.RetryMax))
	cleanupCtx, cancel := personSweepCleanupContext(ctx)
	defer cancel()
	if err := w.Store.FailPersonSweepWork(cleanupCtx, WorkFailure{Lease: lease,
		AttemptID: attemptID, Class: class, RetryAt: retryAt}); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func personSweepCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), personSweepCleanupTimeout)
}

func classifyPersonSweepFailure(err error) (FailureClass, time.Duration) {
	var provider *ProviderError
	var invalid invalidOutputError
	switch {
	case errors.Is(err, ErrPersonSweepConsentRevoked):
		return FailurePolicy, 0
	case errors.Is(err, ErrBudgetExceeded), errors.Is(err, ErrBudgetOverflow):
		return FailureBudget, 0
	case errors.Is(err, ErrLeaseLost):
		return FailureLeaseLost, 0
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return FailureTimeout, 0
	case errors.Is(err, ErrInvalidStructuredOutput), errors.As(err, &invalid):
		return FailureInvalidOutput, 0
	case errors.As(err, &provider):
		if provider.StatusCode == http.StatusTooManyRequests {
			return FailureRateLimited, provider.RetryAfter
		}
		return FailureProviderHTTP, provider.RetryAfter
	default:
		return FailureInternal, 0
	}
}

func structuredResponseCompleted(response StructuredResponse) bool {
	return len(response.Output) > 0 || response.ProviderRequestID != "" ||
		response.ProviderVersion != "" || response.ModelVersion != "" ||
		response.Usage != (TokenUsage{}) || response.UsageKnown
}

func accountableTokenUsage(usage TokenUsage) TokenUsage {
	return TokenUsage{InputTokens: max(int64(0), usage.InputTokens),
		OutputTokens: max(int64(0), usage.OutputTokens)}
}

func accountCompletedProviderCall(
	response StructuredResponse,
	reserved TokenUsage,
	reservedCost int64,
	budget BudgetConfig,
) (Usage, int64, error) {
	if response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 {
		return Usage{}, 0, errors.New("provider returned negative token usage")
	}
	if !response.UsageKnown {
		return Usage{Requests: 1, InputTokens: reserved.InputTokens,
			OutputTokens: reserved.OutputTokens, EstimatedCostMicroUSD: reservedCost}, reservedCost, nil
	}
	reconciled := TokenUsage{
		InputTokens:  max(reserved.InputTokens, response.Usage.InputTokens),
		OutputTokens: max(reserved.OutputTokens, response.Usage.OutputTokens),
	}
	reconciledCost, err := EstimateCostMicroUSD(reconciled, budget)
	if err != nil {
		return Usage{}, 0, err
	}
	return Usage{Requests: 1, InputTokens: reconciled.InputTokens,
		OutputTokens:          reconciled.OutputTokens,
		EstimatedCostMicroUSD: reconciledCost}, reconciledCost, nil
}

func personSweepRetryDelay(attemptID string, attempt int, retryAfter, base, maximum time.Duration) time.Duration {
	if base <= 0 || maximum <= 0 {
		return 0
	}
	digest := sha256.Sum256([]byte(attemptID))
	jitterNanos := binary.BigEndian.Uint64(digest[:8]) % uint64(base)
	// #nosec G115 -- modulo by a positive time.Duration bounds this to MaxInt64.
	jitter := time.Duration(jitterNanos)
	delay := base
	for i := 0; i < attempt && delay < maximum; i++ {
		if delay > maximum/2 {
			delay = maximum
		} else {
			delay *= 2
		}
	}
	if delay < maximum {
		if jitter > maximum-delay {
			delay = maximum
		} else {
			delay += jitter
		}
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	if delay > maximum || delay < 0 {
		return maximum
	}
	return delay
}

func addUsage(left, right Usage) (Usage, error) {
	if right.Requests < 0 || right.InputTokens < 0 || right.OutputTokens < 0 ||
		right.EstimatedCostMicroUSD < 0 {
		return Usage{}, ErrBudgetOverflow
	}
	result := Usage{Requests: left.Requests + right.Requests,
		InputTokens:           left.InputTokens + right.InputTokens,
		OutputTokens:          left.OutputTokens + right.OutputTokens,
		EstimatedCostMicroUSD: left.EstimatedCostMicroUSD + right.EstimatedCostMicroUSD}
	if result.Requests < left.Requests || result.InputTokens < left.InputTokens ||
		result.OutputTokens < left.OutputTokens || result.EstimatedCostMicroUSD < left.EstimatedCostMicroUSD {
		return Usage{}, ErrBudgetOverflow
	}
	return result, nil
}
