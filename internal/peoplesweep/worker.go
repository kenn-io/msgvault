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
	// Brief is the version this attempt generated, or nil when the attempt ran
	// no brief call or the call produced nothing storable.
	Brief *BriefResult
	// BriefFailureClass records why a brief call produced no version while the
	// attempt itself succeeded. It is empty when no brief was attempted or the
	// brief succeeded, and it licenses the apply to reconcile a brief
	// reservation that never completed.
	BriefFailureClass FailureClass
	// BriefRetryAt retains the work item after a brief-only failure.
	BriefRetryAt time.Time
	CompletedAt  time.Time
}

type ApplyMutationMetadata struct {
	// BriefVersion is the person brief version this apply stored, or zero.
	BriefVersion               int
	BriefEvidenceRowsInserted  int
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
	CompleteIdlePersonSweep(ctx context.Context, lease Lease, programFingerprint, catalogFingerprint string) error
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
	// Brief selects how this run treats the person brief step. The zero value
	// is auto, which follows the configured cadence.
	Brief BriefMode
}
type RunResult struct {
	RunID                                             string
	PeopleAttempted, PeopleSucceeded, ProjectedWrites int
	Usage                                             Usage
	// People carries one entry per person this run applied, in claim order, so
	// a caller that asked for one person gets that person's attempt ID and
	// brief outcome without querying the attempt journal. A person whose
	// attempt failed has no entry; the returned error describes that case.
	People []PersonRunResult
}
type PersonRunResult struct {
	PersonID        int64
	AttemptID       string
	ProjectedWrites int
	// BriefVersion is the version number this attempt stored, or zero when it
	// stored none.
	BriefVersion int
	// BriefFailureClass is why a brief call produced no version while the
	// attempt succeeded. Empty when no brief ran or the brief was stored.
	BriefFailureClass FailureClass
	CursorAdvances    []CursorAdvance
	Usage             Usage
}

type Worker struct {
	Config  Config
	Store   WorkStore
	Source  AssemblySource
	Context ContextRetriever
	Sink    ClaimSink
	Runner  StructuredRunner
	Catalog CatalogSource
	// Brief and Archive carry the person brief step. Both nil turns the step
	// off entirely, which is what a caller that predates the brief gets.
	Brief    BriefStore
	Archive  BriefArchive
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
	if !request.Brief.valid() {
		return RunResult{}, errors.New("person sweep worker requires a valid brief mode")
	}
	// A manual forced brief that the lane, the profile policy, or the person's
	// enrollment already rules out can never produce one, so it is refused
	// before the run row exists and before reconciliation publishes any work.
	// The caller gets a typed error to map to a 4xx.
	if w.forcedSinglePersonBrief(request) {
		if err := w.refuseForcedBrief(ctx, request.PersonID, profile); err != nil {
			return RunResult{}, err
		}
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
	if err := w.reconcile(ctx, profile, catalog, now, request); err != nil {
		cleanupCtx, cancel := personSweepCleanupContext(ctx)
		defer cancel()
		finishErr := w.Store.FinishPersonSweepRun(cleanupCtx, runID, RunFailed, w.now())
		return result, errors.Join(err, finishErr)
	}
	// A forced brief for one person must produce a claim even when the journal
	// has no dirty work: a successful sweep deletes the work row, so nothing
	// would otherwise be claimable and the manual request would silently do
	// nothing. Enrollment was already required above.
	if w.forcedSinglePersonBrief(request) {
		if _, err := w.Brief.EnsurePersonSweepWork(ctx, request.PersonID, true); err != nil {
			cleanupCtx, cancel := personSweepCleanupContext(ctx)
			defer cancel()
			finishErr := w.Store.FinishPersonSweepRun(cleanupCtx, runID, RunFailed, w.now())
			return result, errors.Join(err, finishErr)
		}
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
			// A forced brief published its own work above, so an empty claim
			// here means another worker holds the person's lease. The run has
			// not generated anything, and a 200 that says "no new version"
			// would be a lie; refuse with a retryable typed error instead of
			// waiting on the other worker's lease.
			if w.forcedSinglePersonBrief(request) && result.PeopleAttempted == 0 {
				firstErr = fmt.Errorf("person %d: %w", request.PersonID, ErrPersonBriefBusy)
			}
			break
		}
		if request.PersonID > 0 && lease.PersonID != request.PersonID {
			firstErr = errors.New("person sweep worker claimed a different requested person")
			break
		}
		result.PeopleAttempted++
		person, personErr := w.runPerson(ctx, runID, *lease, request.Mode, request.Brief,
			profile, catalog, now)
		if personErr != nil {
			if firstErr == nil {
				firstErr = personErr
			}
			continue
		}
		result.PeopleSucceeded++
		result.People = append(result.People, person)
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
	return w.RunPersonBrief(ctx, runID, lease, mode, BriefModeAuto)
}

// RunPersonBrief runs one leased person with an explicit brief mode. It is the
// single-person entry point a manual "generate this person's brief now" takes.
func (w *Worker) RunPersonBrief(
	ctx context.Context, runID string, lease Lease, mode RunMode, brief BriefMode,
) (PersonRunResult, error) {
	profile, catalog, resolvedAt, err := w.ready(ctx)
	if err != nil {
		return PersonRunResult{}, w.failClaim(ctx, lease, "", err)
	}
	if !brief.valid() {
		return PersonRunResult{}, w.failClaim(ctx, lease, "",
			errors.New("person sweep worker requires a valid brief mode"))
	}
	return w.runPerson(ctx, runID, lease, mode, brief, profile, catalog, resolvedAt)
}

func (w *Worker) runPerson(
	ctx context.Context,
	runID string,
	lease Lease,
	mode RunMode,
	briefMode BriefMode,
	profile ProviderProfile,
	catalog personfacts.Catalog,
	resolvedAt time.Time,
) (PersonRunResult, error) {
	plan, err := w.planPersonBrief(ctx, lease.PersonID, briefMode, profile, resolvedAt)
	if err != nil {
		return PersonRunResult{}, w.failClaim(ctx, lease, "", err)
	}
	keys := make([]CursorKey, 0, len(profile.AllowedSources))
	for _, lane := range profile.AllowedSources {
		keys = append(keys, CursorKey{PersonID: lease.PersonID, SourceLane: lane,
			ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: catalog.Fingerprint})
	}
	cursors, err := w.Store.EnsurePersonSweepCursors(ctx, keys)
	if err != nil {
		return PersonRunResult{}, w.failClaim(ctx, lease, "", err)
	}
	// The brief does not shrink the extraction batch budget: a smaller batch
	// cap can make an assembly unschedulable outright, and a brief that cannot
	// be reserved defers to the next run instead of failing the attempt.
	maxBatches := min(w.Config.Budgets.MaxRequestsPerPerson,
		int(w.Config.Budgets.MaxOutputTokensPerPerson/extractionMaxOutputTokens))
	assemblyRequest := AssemblyRequest{PersonID: lease.PersonID, Cursors: cursors, Catalog: catalog,
		Profile: profile, Now: resolvedAt, BackstopInterval: w.Config.BackstopInterval,
		ForceBackstop: mode == RunBackstop}
	buildAssembly := func(selected []Cursor, maxProgressWindows int, forceBackstop bool) (Assembly, error) {
		request := assemblyRequest
		request.Cursors = selected
		request.ForceBackstop = request.ForceBackstop || forceBackstop
		return (Assembler{Source: w.Source, Context: w.Context,
			MaxBytes: w.Config.EvidenceMaxBytes,
			MaxItems: w.Config.EvidenceMaxItems, WindowLimit: w.Config.ChangeBatchSize,
			MaxBatches: maxBatches, MaxProgressWindows: maxProgressWindows,
			ContextPerTarget:     w.Config.ContextPerTarget,
			HistoricalMessageCap: w.Config.HistoricalMessageCap}).Build(ctx, request)
	}
	assembly, err := buildAssembly(cursors, 0, false)
	if err != nil && !errors.Is(err, ErrNoChangedSeed) {
		return PersonRunResult{}, w.failClaim(ctx, lease, "", err)
	}
	// forcedBackstop records that the assembly below came from the brief's
	// fallback, so the batch-narrowing loop rebuilds it the same way.
	forcedBackstop := false
	if len(assembly.CursorEnvelope) == 0 && plan.run {
		// A person whose sweep is fully caught up has no cursor progress to
		// bind a generation to, and every generation must name a source range.
		// Re-reading one bounded backstop page gives the attempt a real range
		// without inventing one, which is what lets a manual brief run on a
		// person the scheduled sweep already finished. The store bounds an
		// empty archive with its zero source key, so that attempt can still
		// succeed without a provider call or a new brief version.
		if forced, forcedErr := buildAssembly(cursors, 1, true); forcedErr == nil &&
			len(forced.CursorEnvelope) > 0 {
			assembly, forcedBackstop = forced, true
		}
	}
	deferredCursorWork := false
	if len(assembly.Batches) > maxBatches {
		bounded := false
		for index := range cursors {
			candidate, candidateErr := buildAssembly(cursors[index:index+1], 0, forcedBackstop)
			if candidateErr != nil && !errors.Is(candidateErr, ErrNoChangedSeed) {
				return PersonRunResult{}, w.failClaim(ctx, lease, "", candidateErr)
			}
			if len(candidate.CursorEnvelope) == 0 {
				continue
			}
			if len(candidate.Batches) > maxBatches {
				candidate, candidateErr = buildAssembly(cursors[index:index+1], 1, forcedBackstop)
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
		if !plan.run {
			// A queued brief can become ineligible while waiting to retry. With
			// extraction caught up, finish the idle claim without an inference
			// attempt. The store preserves work that arrived during assembly.
			if err := w.Store.CompleteIdlePersonSweep(ctx, lease, ProgramFingerprint(), catalog.Fingerprint); err != nil {
				return PersonRunResult{}, w.failClaim(ctx, lease, "", err)
			}
			return PersonRunResult{PersonID: lease.PersonID}, nil
		}
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

	reservations := make([]BudgetReservation, 0, len(assembly.Batches)+2)
	admitted := make([]sweepProviderCall, 0, len(assembly.Batches))
	accounting := &sweepCallAccounting{budget: w.Config.Budgets}
	claims := make([]personfacts.ProposedClaim, 0)
	// Prepare and reserve the whole immutable request set before the first paid
	// provider call. A later batch must never discover a run/person/day budget
	// violation after an earlier batch has already crossed the network boundary.
	for _, batch := range assembly.Batches {
		renewed, renewErr := w.Store.RenewPersonSweep(ctx, lease, w.Config.LeaseDuration)
		if renewErr != nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID, reservations,
				accounting.completedUsage, renewErr, resolvedAt)
		}
		if renewed == nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID, reservations,
				accounting.completedUsage, ErrLeaseLost, resolvedAt)
		}
		lease = *renewed
		prepared, prepareErr := w.Runner.PrepareStructured(ctx, batch.Request)
		if prepareErr != nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID, reservations,
				accounting.completedUsage, prepareErr, resolvedAt)
		}
		estimate, estimateErr := EstimateWireTokenReservation(
			prepared.WireRequest(), batch.Request.MaxOutputTokens)
		if estimateErr != nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID, reservations,
				accounting.completedUsage, estimateErr, resolvedAt)
		}
		estimatedCost, estimateErr := EstimateCostMicroUSD(estimate, w.Config.Budgets)
		if estimateErr != nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID, reservations,
				accounting.completedUsage, estimateErr, resolvedAt)
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
				accounting.completedUsage, reserveErr, resolvedAt)
		}
		reservations = append(reservations, reservation)
		admitted = append(admitted, sweepProviderCall{batch: batch, prepared: prepared,
			estimate: estimate, estimatedCost: estimatedCost, reservation: reservation,
			callOrdinal: 0, purpose: ProviderCallPurposePrimary})
	}
	executions := make([]StructuredExecutionSession, len(admitted))
	primaryCalls := make([]PreparedStructuredCall, len(admitted))
	for index := range admitted {
		execution, beginErr := w.Runner.BeginStructuredExecution(ctx, admitted[index].prepared)
		if beginErr != nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID,
				reservations, accounting.completedUsage, beginErr, resolvedAt)
		}
		primaryCall, callErr := execution.PrimaryCall(admitted[index].prepared)
		if callErr != nil {
			return PersonRunResult{}, w.finalizePreflightFailure(ctx, lease, attemptID,
				reservations, accounting.completedUsage, callErr, resolvedAt)
		}
		executions[index] = execution
		primaryCalls[index] = primaryCall
	}
	for primaryIndex, primary := range admitted {
		execution := executions[primaryIndex]
		call := primary
		preparedCall := primaryCalls[primaryIndex]
		for {
			renewed, renewErr := w.Store.RenewPersonSweep(ctx, lease, w.Config.LeaseDuration)
			if renewErr != nil {
				return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
					accounting.completedUsage, renewErr, resolvedAt)
			}
			if renewed == nil {
				return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
					accounting.completedUsage, ErrLeaseLost, resolvedAt)
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
					accounting.completedUsage, runErr, resolvedAt)
			}
			latency := max(time.Duration(0), w.now().Sub(started))
			if structuredResponseCompleted(response) {
				if recordErr := accounting.record(call, response, latency); recordErr != nil {
					return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
						accounting.completedUsage, recordErr, resolvedAt)
				}
			}

			var failure *ValidationFailure
			if runErr != nil {
				if !errors.As(runErr, &failure) || call.callOrdinal != 0 || failure.repair {
					return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
						accounting.completedUsage, runErr, resolvedAt)
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
						accounting.completedUsage, failure, resolvedAt)
				}
				semanticFailure, semanticErr := execution.SemanticValidationFailure(response)
				if semanticErr != nil {
					return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
						accounting.completedUsage, semanticErr, resolvedAt)
				}
				failure = &semanticFailure
			}

			repair, repairErr := execution.PrepareRepair(*failure)
			if repairErr != nil {
				return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
					accounting.completedUsage, repairErr, resolvedAt)
			}
			repairCall, repairErr := execution.RepairCall(repair)
			if repairErr != nil {
				return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
					accounting.completedUsage, repairErr, resolvedAt)
			}
			repairEstimate, estimateErr := EstimateWireTokenReservation(
				repair.WireRequest(), primary.batch.Request.MaxOutputTokens)
			if estimateErr != nil {
				return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
					accounting.completedUsage, estimateErr, resolvedAt)
			}
			repairCost, estimateErr := EstimateCostMicroUSD(repairEstimate, w.Config.Budgets)
			if estimateErr != nil {
				return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
					accounting.completedUsage, estimateErr, resolvedAt)
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
					accounting.completedUsage, reserveErr, resolvedAt)
			}
			reservations = append(reservations, repairReservation)
			call = sweepProviderCall{batch: primary.batch, prepared: repair, estimate: repairEstimate,
				estimatedCost: repairCost, reservation: repairReservation,
				callOrdinal: 1, purpose: ProviderCallPurposeRepair}
			preparedCall = repairCall
		}
	}
	// The brief closes the attempt: it summarizes the person's most recent
	// stretch of interaction, so it runs after every extraction batch and takes
	// the next batch ordinal. A brief failure never rolls back the extraction.
	var brief *BriefResult
	var briefFailure FailureClass
	if plan.run {
		var fatal error
		brief, briefFailure, lease, fatal = w.runBriefCall(ctx, briefCall{
			runID: runID, attemptID: attemptID, lease: lease, profile: profile,
			catalog: catalog, resolvedAt: resolvedAt, plan: plan,
			batchOrdinal: len(assembly.Batches),
		}, accounting, &reservations)
		if fatal != nil {
			return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
				accounting.completedUsage, fatal, resolvedAt)
		}
	}

	provider, model := string(profile.Protocol), profile.Model
	providerVersion, modelVersion := accounting.providerVersion, accounting.modelVersion
	if len(accounting.completedBatches) == 0 {
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
			accounting.completedUsage, renewErr, resolvedAt)
	}
	if renewed == nil {
		return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
			accounting.completedUsage, ErrLeaseLost, resolvedAt)
	}
	lease = *renewed
	completedAt := w.now()
	var briefRetryAt time.Time
	if briefFailure != "" {
		briefRetryAt = completedAt.Add(personSweepRetryDelay(attemptID, lease.AttemptCount, 0,
			w.Config.RetryBase, w.Config.RetryMax))
	}
	apply, applyErr := w.Sink.ApplyPersonSweep(ctx, ApplyRequest{Lease: lease, RunID: runID,
		AttemptID: attemptID, Generation: generation, CursorEnvelope: assembly.CursorEnvelope,
		Batches: accounting.completedBatches, Usage: accounting.totalUsage,
		Budget: w.Config.Budgets, CursorAdvances: advances,
		DeferredCursorWork: deferredCursorWork, Brief: brief, BriefFailureClass: briefFailure,
		BriefRetryAt: briefRetryAt, CompletedAt: completedAt})
	if applyErr != nil {
		return PersonRunResult{}, w.finalizeFailure(ctx, lease, attemptID, reservations,
			accounting.completedUsage, applyErr, resolvedAt)
	}
	return PersonRunResult{PersonID: lease.PersonID, AttemptID: attemptID,
		ProjectedWrites: apply.Mutations.ProjectionRowsWritten, BriefVersion: apply.Mutations.BriefVersion,
		BriefFailureClass: briefFailure,
		CursorAdvances:    advances, Usage: accounting.totalUsage}, nil
}

type leaseHeartbeatResult struct {
	lease Lease
	err   error
}

// errLeaseHeartbeat distinguishes renewal failures from provider call errors.
// Even a transient store error leaves lease ownership uncertain for this attempt.
var errLeaseHeartbeat = errors.New("person sweep lease heartbeat failed")

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
		return heartbeat.lease, response, fmt.Errorf("%w: %w", errLeaseHeartbeat, heartbeat.err)
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
	catalog personfacts.Catalog, now time.Time, request RunRequest,
) error {
	after := int64(0)
	for {
		page, err := w.Store.ReconcilePersonSweepWorkContext(ctx, GapRequest{
			ProgramFingerprint: ProgramFingerprint(), CatalogFingerprint: catalog.Fingerprint,
			SourceLanes: profile.AllowedSources, AfterPersonID: after,
			Limit: w.Config.WorkBatchSize, Now: now, BackstopInterval: w.Config.BackstopInterval,
			ForceBackstop: request.Mode == RunBackstop,
		})
		if err != nil {
			return err
		}
		if page.PeopleScanned == 0 || page.NextPersonID <= after {
			return nil
		}
		// Brief cadence and enrollment can become due without an extraction
		// cursor gap. Reuse this tracked-person page to publish eligible work,
		// leaving the bounded backstop fallback to the person actually claimed.
		if w.Config.Brief.IsEnabled() && w.Brief != nil && w.Archive != nil &&
			request.Brief.normalized() != BriefModeSkip && !w.forcedSinglePersonBrief(request) {
			for _, personID := range page.PersonIDs {
				if request.PersonID > 0 && personID != request.PersonID {
					continue
				}
				plan, err := w.planPersonBrief(ctx, personID, request.Brief, profile, now)
				if err != nil {
					return fmt.Errorf("plan person %d brief work: %w", personID, err)
				}
				if plan.run {
					// An enrolled person with no admissible text never produces a
					// version. Check the bounded window before publishing work so
					// such a person does not repeat backstop extraction every tick.
					_, err := BuildBriefWindow(ctx, w.Archive,
						w.briefWindowRequest(personID, catalog, profile, plan, 0))
					if errors.Is(err, ErrNoBriefEvidence) {
						continue
					}
					if ctx.Err() != nil {
						return ctx.Err()
					}
					// Other window failures belong to this person's attempt. The
					// normal brief call records them without blocking extraction
					// for everyone else in the run.
					if _, err := w.Brief.EnsurePersonSweepWork(ctx, personID, false); err != nil {
						return fmt.Errorf("publish person %d brief work: %w", personID, err)
					}
				}
			}
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

// sweepProviderCall is one admitted provider call: the batch it summarizes, the
// exact prepared request, the reservation it holds, and its call coordinate.
type sweepProviderCall struct {
	batch         PacketBatch
	prepared      PreparedStructuredRequest
	estimate      TokenUsage
	estimatedCost int64
	reservation   BudgetReservation
	callOrdinal   int
	purpose       string
}

// sweepCallAccounting is the usage every call in one attempt contributes to.
// The extraction batches and the brief share it so the attempt reports one
// provider identity and one usage total.
type sweepCallAccounting struct {
	budget           BudgetConfig
	completedUsage   []CompletedUsage
	completedBatches []CompletedBatch
	totalUsage       Usage
	providerVersion  string
	modelVersion     string
}

// record validates and accounts a completed call before recording anything. A
// response rejected below is untrusted, and a completed usage record derived
// from it would either fail failure finalization outright (stranding the
// attempt and lease until expiry) or write untrusted values into durable
// history. Finalizing without a record lets the store conservatively charge the
// reservation instead.
func (a *sweepCallAccounting) record(
	call sweepProviderCall, response StructuredResponse, latency time.Duration,
) error {
	if response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 {
		return invalidOutputError{errors.New("provider returned negative token usage")}
	}
	if !canonicalProviderIdentity(response.ProviderVersion) ||
		!canonicalProviderIdentity(response.ModelVersion) ||
		!IsSafeProviderMetadata(response.ProviderRequestID) {
		return invalidOutputError{errors.New("provider returned unsafe identity metadata")}
	}
	if a.providerVersion == "" {
		a.providerVersion, a.modelVersion = response.ProviderVersion, response.ModelVersion
	} else if a.providerVersion != response.ProviderVersion || a.modelVersion != response.ModelVersion {
		return invalidOutputError{errors.New("provider call identities differ")}
	}
	accounted, actualCost, err := accountCompletedProviderCall(
		response, call.estimate, call.estimatedCost, a.budget)
	if err != nil {
		return err
	}
	accountedTotal, err := addUsage(a.totalUsage, accounted)
	if err != nil {
		return err
	}
	a.completedUsage = append(a.completedUsage, CompletedUsage{
		BatchOrdinal: call.batch.Ordinal, CallOrdinal: call.callOrdinal, Purpose: call.purpose,
		ProviderRequestID: response.ProviderRequestID, Usage: accountableTokenUsage(response.Usage),
		UsageKnown: response.UsageKnown, Latency: latency,
	})
	a.completedBatches = append(a.completedBatches, CompletedBatch{
		Ordinal: call.batch.Ordinal, CallOrdinal: call.callOrdinal, Purpose: call.purpose,
		ReservationID: call.reservation.ID, InputHash: call.prepared.WireSHA256(),
		ProviderRequestID: response.ProviderRequestID, ProviderVersion: response.ProviderVersion,
		ModelVersion: response.ModelVersion, Usage: accountableTokenUsage(response.Usage),
		UsageKnown: response.UsageKnown, ActualCostMicroUSD: actualCost, Latency: latency,
	})
	a.totalUsage = accountedTotal
	return nil
}
