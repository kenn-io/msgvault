package peoplesweep

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

type AssemblySource interface {
	LoadPersonSweepWindow(ctx context.Context, request WindowRequest) (PersonWindow, error)
	LoadPersonFactState(ctx context.Context, personID int64, catalog personfacts.Catalog) (PersonFactState, error)
	BuildPersonSweepEvidenceStatusChanges(ctx context.Context, personID int64, changes []ArchiveChange) ([]personfacts.EvidenceStatusChange, error)
}

type PersonFactState struct {
	Current    []ProjectedValue
	Unresolved []personfacts.Claim
}

type AssemblyRequest struct {
	PersonID         int64
	Cursors          []Cursor
	Catalog          personfacts.Catalog
	Profile          ProviderProfile
	Now              time.Time
	BackstopInterval time.Duration
	ForceBackstop    bool
}

type Assembly struct {
	Windows               []PersonWindow
	CursorEnvelope        []GenerationCursor
	EvidenceStatusChanges []personfacts.EvidenceStatusChange
	Packet                EvidencePacket
	Batches               []PacketBatch
}

type Assembler struct {
	Source               AssemblySource
	Context              ContextRetriever
	MaxBytes             int
	MaxItems             int
	WindowLimit          int
	MaxBatches           int
	MaxProgressWindows   int
	ContextPerTarget     int
	HistoricalMessageCap int
}

func (a Assembler) Build(ctx context.Context, request AssemblyRequest) (Assembly, error) {
	if a.Source == nil || request.PersonID <= 0 || len(request.Cursors) == 0 {
		return Assembly{}, errors.New("person sweep assembly requires source, person, and cursors")
	}
	if a.MaxBytes <= 0 || a.MaxItems <= 0 || a.ContextPerTarget < 0 {
		return Assembly{}, errors.New("person sweep assembly limits are invalid")
	}
	if request.Now.IsZero() || request.BackstopInterval <= 0 {
		return Assembly{}, errors.New("person sweep backstop selection requires current time and positive interval")
	}
	request.Now = request.Now.UTC()
	windowLimit := a.WindowLimit
	if windowLimit <= 0 {
		windowLimit = a.MaxItems
	}
	after, before, err := contextDateBounds(request.Profile.SourceSince, request.Profile.SourceUntil)
	if err != nil {
		return Assembly{}, err
	}

	basePacket, err := packetForProfile(EvidencePacket{
		PersonID: request.PersonID, ProgramID: ExtractionProgramID,
		ProgramVersion: ExtractionProgramVersion, Catalog: request.Catalog,
	}, request.Profile)
	if err != nil {
		return Assembly{}, err
	}
	cursors := append([]Cursor(nil), request.Cursors...)
	sort.Slice(cursors, func(i, j int) bool {
		if cursors[i].Key.SourceLane != cursors[j].Key.SourceLane {
			return cursors[i].Key.SourceLane < cursors[j].Key.SourceLane
		}
		if cursors[i].Key.ProgramFingerprint != cursors[j].Key.ProgramFingerprint {
			return cursors[i].Key.ProgramFingerprint < cursors[j].Key.ProgramFingerprint
		}
		return cursors[i].Key.CatalogFingerprint < cursors[j].Key.CatalogFingerprint
	})

	result := Assembly{}
	allChanges := make([]ArchiveChange, 0)
	seedByID := make(map[string]EvidenceItem)
	progressWindows := 0
	appendBackstop := func(cursor Cursor) error {
		backstop, loadErr := a.loadWindow(ctx, WindowRequest{
			PersonID: request.PersonID, Lane: cursor.Key.SourceLane,
			Mode: GenerationCursorBackstop, ReconcileAfter: cursor.BackstopAfterKey,
			BackstopUpper: cursor.BackstopUpperKey, DocumentAfter: cursor.BackstopDocumentKey,
			Limit: windowLimit,
		})
		if loadErr != nil {
			return loadErr
		}
		capturedUpper := backstop.CapturedUpperKey
		if capturedUpper == "" ||
			(cursor.BackstopUpperKey != "" && capturedUpper != cursor.BackstopUpperKey) ||
			!sweepCursorCoordinateAdvanced(cursor.BackstopAfterKey, cursor.BackstopDocumentKey,
				backstop.NextReconcileKey, backstop.NextDocumentKey) ||
			backstop.NextReconcileKey > capturedUpper ||
			backstop.ReconciliationDone != (backstop.NextReconcileKey == capturedUpper &&
				backstop.NextDocumentKey == "") {
			return errors.New("person sweep backstop returned invalid bounded progress")
		}
		result.Windows = append(result.Windows, backstop)
		allChanges = append(allChanges, backstop.Changes...)
		if err := appendAssemblySeeds(seedByID, backstop.Seeds, request.Profile, after, before); err != nil {
			return err
		}
		// A completed backstop page can also cover the outstanding bounded
		// reconciliation interval. Advancing both coordinates is safe because
		// they are fenced in the same attempt and backed by the same evidence
		// window; it avoids paying for the identical source page twice.
		reconciliationAlreadyCovered := false
		for _, progress := range result.CursorEnvelope {
			if progress.Key == cursor.Key && progress.Mode == GenerationCursorReconciliation {
				reconciliationAlreadyCovered = true
				break
			}
		}
		if !reconciliationAlreadyCovered && !cursor.ReconciliationComplete &&
			cursor.ReconcileDocumentKey == "" && backstop.NextDocumentKey == "" &&
			cursor.ReconcileAfterKey < cursor.ReconcileUpperKey &&
			cursor.BackstopAfterKey <= cursor.ReconcileAfterKey &&
			backstop.NextReconcileKey >= cursor.ReconcileUpperKey {
			result.CursorEnvelope = append(result.CursorEnvelope, GenerationCursor{
				Key: cursor.Key, Mode: GenerationCursorReconciliation,
				ReconcileFromKey: cursor.ReconcileAfterKey,
				ReconcileToKey:   cursor.ReconcileUpperKey,
			})
		}
		result.CursorEnvelope = append(result.CursorEnvelope, GenerationCursor{
			Key: cursor.Key, Mode: GenerationCursorBackstop,
			ReconcileFromKey: cursor.BackstopAfterKey,
			ReconcileToKey:   backstop.NextReconcileKey,
			DocumentFromKey:  cursor.BackstopDocumentKey,
			DocumentToKey:    backstop.NextDocumentKey,
			BackstopUpperKey: capturedUpper,
		})
		return nil
	}
cursorLoop:
	for _, cursor := range cursors {
		if err := validateAssemblyCursor(request, cursor); err != nil {
			return Assembly{}, err
		}
		if request.ForceBackstop {
			if err := appendBackstop(cursor); err != nil {
				return Assembly{}, err
			}
			progressWindows++
			if a.MaxProgressWindows > 0 && progressWindows >= a.MaxProgressWindows {
				break cursorLoop
			}
			continue
		}
		optimistic, err := a.loadWindow(ctx, WindowRequest{
			PersonID: request.PersonID, Lane: cursor.Key.SourceLane,
			Mode: GenerationCursorOptimistic, AfterSequence: cursor.OptimisticSequence,
			DocumentAfter: cursor.OptimisticDocumentKey, Limit: windowLimit,
		})
		if err != nil {
			return Assembly{}, err
		}
		result.Windows = append(result.Windows, optimistic)
		allChanges = append(allChanges, optimistic.Changes...)
		if err := appendAssemblySeeds(seedByID, optimistic.Seeds, request.Profile, after, before); err != nil {
			return Assembly{}, err
		}
		if sweepSequenceCoordinateAdvanced(cursor.OptimisticSequence, cursor.OptimisticDocumentKey,
			optimistic.NextSequence, optimistic.NextDocumentKey) {
			result.CursorEnvelope = append(result.CursorEnvelope, GenerationCursor{
				Key: cursor.Key, Mode: GenerationCursorOptimistic,
				CursorFrom: cursor.OptimisticSequence, CursorThrough: optimistic.NextSequence,
				DocumentFromKey: cursor.OptimisticDocumentKey,
				DocumentToKey:   optimistic.NextDocumentKey,
			})
			progressWindows++
			if a.MaxProgressWindows > 0 && progressWindows >= a.MaxProgressWindows {
				break cursorLoop
			}
		}

		if !cursor.ReconciliationComplete && cursor.ReconcileAfterKey != cursor.ReconcileUpperKey {
			reconciliation, loadErr := a.loadWindow(ctx, WindowRequest{
				PersonID: request.PersonID, Lane: cursor.Key.SourceLane,
				Mode:           GenerationCursorReconciliation,
				ReconcileAfter: cursor.ReconcileAfterKey, ReconcileUpper: cursor.ReconcileUpperKey,
				DocumentAfter: cursor.ReconcileDocumentKey, Limit: windowLimit,
			})
			if loadErr != nil {
				return Assembly{}, loadErr
			}
			result.Windows = append(result.Windows, reconciliation)
			allChanges = append(allChanges, reconciliation.Changes...)
			if err := appendAssemblySeeds(seedByID, reconciliation.Seeds, request.Profile, after, before); err != nil {
				return Assembly{}, err
			}
			if sweepCursorCoordinateAdvanced(
				cursor.ReconcileAfterKey, cursor.ReconcileDocumentKey,
				reconciliation.NextReconcileKey, reconciliation.NextDocumentKey) {
				result.CursorEnvelope = append(result.CursorEnvelope, GenerationCursor{
					Key: cursor.Key, Mode: GenerationCursorReconciliation,
					ReconcileFromKey: cursor.ReconcileAfterKey,
					ReconcileToKey:   reconciliation.NextReconcileKey,
					DocumentFromKey:  cursor.ReconcileDocumentKey,
					DocumentToKey:    reconciliation.NextDocumentKey,
				})
				progressWindows++
				if a.MaxProgressWindows > 0 && progressWindows >= a.MaxProgressWindows {
					break cursorLoop
				}
			}
		}

		if cursor.BackstopUpperKey != "" ||
			assemblyBackstopDue(cursor.LastBackstopAt, request.Now, request.BackstopInterval) {
			if err := appendBackstop(cursor); err != nil {
				return Assembly{}, err
			}
			progressWindows++
			if a.MaxProgressWindows > 0 && progressWindows >= a.MaxProgressWindows {
				break cursorLoop
			}
		}
	}

	result.EvidenceStatusChanges, err = a.Source.BuildPersonSweepEvidenceStatusChanges(
		ctx, request.PersonID, canonicalArchiveChanges(allChanges))
	if err != nil {
		return Assembly{}, fmt.Errorf("build person sweep evidence status changes: %w", err)
	}
	result.EvidenceStatusChanges = canonicalStatusChanges(result.EvidenceStatusChanges)
	result.CursorEnvelope = canonicalGenerationCursors(result.CursorEnvelope)
	result.Packet = basePacket
	result.Packet.Seeds = evidenceMapValues(seedByID)

	state, err := a.Source.LoadPersonFactState(ctx, request.PersonID, basePacket.Catalog)
	if err != nil {
		return Assembly{}, fmt.Errorf("load person fact state: %w", err)
	}
	result.Packet.CurrentProjection = state.Current
	result.Packet.UnresolvedClaims = state.Unresolved
	result.Packet, err = packetForProfile(result.Packet, request.Profile)
	if err != nil {
		return Assembly{}, err
	}

	if len(result.Packet.Seeds) == 0 {
		if len(result.EvidenceStatusChanges) == 0 {
			return result, ErrNoChangedSeed
		}
		return result, nil
	}
	if len(result.Packet.Catalog.Targets) == 0 {
		return result, nil
	}
	if a.Context != nil && a.ContextPerTarget > 0 {
		contextByID := make(map[string]EvidenceItem)
		seedIDs := make(map[string]struct{}, len(result.Packet.Seeds))
		for _, seed := range result.Packet.Seeds {
			seedIDs[packetEvidenceID(seed)] = struct{}{}
		}
		for _, target := range result.Packet.Catalog.Targets {
			items, retrieveErr := a.Context.RetrievePersonSweepContext(ctx, ContextRequest{
				PersonID: request.PersonID, Target: target,
				SourceClasses: append([]SourceClass(nil), request.Profile.AllowedSources...),
				SourceSince:   request.Profile.SourceSince, SourceUntil: request.Profile.SourceUntil,
				HistoricalCandidateLimit: a.HistoricalMessageCap, Limit: a.ContextPerTarget,
			})
			if retrieveErr != nil {
				return Assembly{}, fmt.Errorf("retrieve context for target %q: %w", target.Key, retrieveErr)
			}
			for _, item := range items {
				allowed, validateErr := assemblyEvidenceAllowed(item, request.Profile, after, before)
				if validateErr != nil {
					return Assembly{}, validateErr
				}
				if !allowed {
					continue
				}
				id := packetEvidenceID(item)
				if _, isSeed := seedIDs[id]; !isSeed {
					contextByID[id] = item
				}
			}
		}
		result.Packet.Context = evidenceMapValues(contextByID)
	}
	result.Batches, err = PartitionEvidencePacket(result.Packet, a.MaxBytes, a.MaxItems)
	if err != nil {
		return Assembly{}, err
	}
	for a.MaxBatches > 0 && len(result.Batches) > a.MaxBatches && len(result.Packet.Context) > 0 {
		result.Packet.Context = result.Packet.Context[:len(result.Packet.Context)-1]
		result.Batches, err = PartitionEvidencePacket(result.Packet, a.MaxBytes, a.MaxItems)
		if err != nil {
			return Assembly{}, err
		}
	}
	return result, nil
}

func (a Assembler) loadWindow(ctx context.Context, request WindowRequest) (PersonWindow, error) {
	window, err := a.Source.LoadPersonSweepWindow(ctx, request)
	if err != nil {
		return PersonWindow{}, fmt.Errorf("load %s person sweep window for %s: %w", request.Mode, request.Lane, err)
	}
	return window, nil
}

func validateAssemblyCursor(request AssemblyRequest, cursor Cursor) error {
	if cursor.Key.PersonID != request.PersonID || cursor.Key.SourceLane == "" ||
		cursor.Key.ProgramFingerprint != ProgramFingerprint() ||
		cursor.Key.CatalogFingerprint != request.Catalog.Fingerprint {
		return errors.New("person sweep assembly cursor does not match person, program, or catalog")
	}
	return nil
}

func assemblyBackstopDue(last *time.Time, now time.Time, interval time.Duration) bool {
	return last == nil || !last.Add(interval).After(now)
}

func appendAssemblySeeds(
	destination map[string]EvidenceItem,
	items []EvidenceItem,
	profile ProviderProfile,
	after, before *time.Time,
) error {
	for _, item := range items {
		allowed, err := assemblyEvidenceAllowed(item, profile, after, before)
		if err != nil {
			return err
		}
		if !allowed {
			continue
		}
		destination[packetEvidenceID(item)] = item
	}
	return nil
}

func assemblyEvidenceAllowed(
	item EvidenceItem,
	profile ProviderProfile,
	after, before *time.Time,
) (bool, error) {
	if err := ValidatePersonSweepEvidenceItem(item); err != nil {
		return false, fmt.Errorf("invalid host metadata: %w", err)
	}
	exactSubject := item.SubjectPersonID != nil && *item.SubjectPersonID == item.PersonID
	return !item.Tombstone && exactSubject && item.Excerpt != "" &&
		contextAllowsItem(item, profile.AllowedSources, after, before), nil
}

func evidenceMapValues(items map[string]EvidenceItem) []EvidenceItem {
	values := make([]EvidenceItem, 0, len(items))
	for _, item := range items {
		values = append(values, item)
	}
	sort.Slice(values, func(i, j int) bool { return evidenceLess(values[i], values[j]) })
	return values
}

func canonicalArchiveChanges(changes []ArchiveChange) []ArchiveChange {
	canonical := append([]ArchiveChange(nil), changes...)
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].Sequence != canonical[j].Sequence {
			return canonical[i].Sequence < canonical[j].Sequence
		}
		if canonical[i].SourceLane != canonical[j].SourceLane {
			return canonical[i].SourceLane < canonical[j].SourceLane
		}
		return canonical[i].MessageID < canonical[j].MessageID
	})
	return canonical
}

func canonicalStatusChanges(changes []personfacts.EvidenceStatusChange) []personfacts.EvidenceStatusChange {
	canonical := append([]personfacts.EvidenceStatusChange(nil), changes...)
	sort.Slice(canonical, func(i, j int) bool {
		left, right := canonical[i], canonical[j]
		if left.EvidenceKey != right.EvidenceKey {
			return left.EvidenceKey < right.EvidenceKey
		}
		if left.SourceVersion != right.SourceVersion {
			return left.SourceVersion < right.SourceVersion
		}
		if left.Supported != right.Supported {
			return !left.Supported
		}
		return left.Reason < right.Reason
	})
	return deduplicateSortedStatusChanges(canonical)
}

func deduplicateSortedStatusChanges(changes []personfacts.EvidenceStatusChange) []personfacts.EvidenceStatusChange {
	if len(changes) < 2 {
		return changes
	}
	out := changes[:1]
	for _, change := range changes[1:] {
		prior := out[len(out)-1]
		if prior.EvidenceKey == change.EvidenceKey && prior.SourceVersion == change.SourceVersion &&
			prior.Supported == change.Supported && prior.Reason == change.Reason {
			continue
		}
		out = append(out, change)
	}
	return out
}

func canonicalGenerationCursors(cursors []GenerationCursor) []GenerationCursor {
	canonical := append([]GenerationCursor(nil), cursors...)
	sort.Slice(canonical, func(i, j int) bool {
		left, right := canonical[i], canonical[j]
		if left.Key.SourceLane != right.Key.SourceLane {
			return left.Key.SourceLane < right.Key.SourceLane
		}
		if left.Mode != right.Mode {
			return cursorModeOrder(left.Mode) < cursorModeOrder(right.Mode)
		}
		if left.CursorFrom != right.CursorFrom {
			return left.CursorFrom < right.CursorFrom
		}
		if left.ReconcileFromKey != right.ReconcileFromKey {
			return left.ReconcileFromKey < right.ReconcileFromKey
		}
		return left.DocumentFromKey < right.DocumentFromKey
	})
	return canonical
}

func sweepSequenceCoordinateAdvanced(from int64, fromDocument string, to int64, toDocument string) bool {
	return to > from || (to == from && toDocument > fromDocument)
}

func sweepCursorCoordinateAdvanced(from, fromDocument, to, toDocument string) bool {
	return to > from || (to == from && toDocument > fromDocument)
}

func cursorModeOrder(mode GenerationCursorMode) int {
	switch mode {
	case GenerationCursorOptimistic:
		return 0
	case GenerationCursorReconciliation:
		return 1
	case GenerationCursorBackstop:
		return 2
	default:
		return 3
	}
}
