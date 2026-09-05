package embed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// PersonDocumentStore is the source-side surface required to reconcile the
// small curated person corpus. *store.Store implements it.
type PersonDocumentStore interface {
	ListPersonSemanticDocumentsContext(ctx context.Context) ([]store.PersonSemanticDocument, error)
	LoadPersonSemanticDocumentContext(ctx context.Context, personID int64) (*store.PersonSemanticDocument, error)
}

var _ PersonDocumentStore = (*store.Store)(nil)

// PersonWorkerDeps bundles the source projection, person-owned vector
// backend, and embedding client. BatchSize defaults to 32.
type PersonWorkerDeps struct {
	Store         PersonDocumentStore
	Backend       vector.PersonBackend
	Client        SemanticClient
	Gate          vector.SemanticPersonEmbeddingGate
	BatchSize     int
	MaxInputChars int
	Recorder      operations.Recorder
	Log           *slog.Logger
}

// PersonWorker reconciles one generation to the complete current curated
// person corpus. Every run is a stable full scan; exact revisions make the
// unchanged path provider-free.
type PersonWorker struct {
	deps PersonWorkerDeps
}

type personBatchState struct {
	endpointHealthy bool
	rejected        []store.PersonSemanticDocument
	rejectionErr    error
}

const personEndpointProbeText = "msgvault semantic person health check"

// NewPersonWorker constructs a bounded full-scan person reconciler.
func NewPersonWorker(deps PersonWorkerDeps) *PersonWorker {
	if deps.BatchSize <= 0 {
		deps.BatchSize = 32
	}
	if deps.MaxInputChars <= 0 {
		deps.MaxInputChars = store.MaxPersonSemanticDocumentBytes
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	return &PersonWorker{deps: deps}
}

// RunOnce embeds missing or changed person documents, fences each provider
// response against a fresh source read, and removes vectors whose source
// person no longer exists.
func (w *PersonWorker) RunOnce(
	ctx context.Context, gen vector.GenerationID, scope operations.PassScope,
) (result RunResult, retErr error) {
	if w == nil {
		return result, errors.New("person worker is required")
	}
	pass, terminal, err := beginOperationPass(
		ctx, w.deps.Recorder, operations.KindPersonEmbedding, scope, w.deps.Log,
	)
	if err != nil {
		return result, err
	}
	if terminal != nil {
		return runResultFromOperationRun(terminal)
	}
	defer func() { pass.finish(ctx, finalRunCounters(result), retErr) }()
	return w.runOnce(ctx, gen, pass)
}

func (w *PersonWorker) runOnce(
	ctx context.Context, gen vector.GenerationID, pass *operationPass,
) (RunResult, error) {
	var result RunResult
	if w == nil || w.deps.Store == nil || w.deps.Backend == nil ||
		w.deps.Client == nil || w.deps.Gate == nil {
		return result, errors.New("person worker: store, backend, client, and gate are required")
	}
	if err := w.deps.Gate.Check(ctx); err != nil {
		if vector.SemanticPersonEmbeddingInactive(err) {
			return result, nil
		}
		return result, fmt.Errorf("person worker gate: %w", err)
	}

	documents, err := w.deps.Store.ListPersonSemanticDocumentsContext(ctx)
	if err != nil {
		return result, fmt.Errorf("list person semantic documents: %w", err)
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].PersonID < documents[j].PersonID })
	revisions, err := w.deps.Backend.ListPersonRevisions(ctx, gen)
	if errors.Is(err, vector.ErrGenerationRetired) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("list person vector revisions for generation %d: %w", gen, err)
	}

	changed := make([]store.PersonSemanticDocument, 0)
	for i, document := range documents {
		if i > 0 && document.PersonID == documents[i-1].PersonID {
			return result, fmt.Errorf("duplicate person id %d in semantic document scan", document.PersonID)
		}
		if document.Text == "" {
			continue
		}
		if revisions[document.PersonID] != document.Revision {
			changed = append(changed, document)
		}
	}
	var runErr error
	for start := 0; start < len(changed); start += w.deps.BatchSize {
		end := min(start+w.deps.BatchSize, len(changed))
		batch := changed[start:end]
		result.Claimed += len(batch)
		for _, document := range batch {
			if capPersonProviderInput(document.Text, w.deps.MaxInputChars) != document.Text {
				result.Truncated++
			}
		}
		var batchState personBatchState
		if err := w.embedPersonBatch(ctx, gen, batch, &result, &batchState); err != nil {
			if errors.Is(err, vector.ErrGenerationRetired) {
				return result, nil
			}
			if vector.SemanticPersonEmbeddingInactive(err) {
				return result, nil
			}
			runErr = errors.Join(runErr, err)
		}
		if len(batchState.rejected) > 0 {
			if !batchState.endpointHealthy {
				probeErr := w.probePersonEndpoint(ctx)
				if vector.SemanticPersonEmbeddingInactive(probeErr) {
					return result, nil
				}
				batchState.endpointHealthy = probeErr == nil
			}
			if batchState.endpointHealthy {
				if err := w.recordRejectedPersons(ctx, gen, batchState.rejected); err != nil {
					if errors.Is(err, vector.ErrGenerationRetired) {
						return result, nil
					}
					runErr = errors.Join(runErr, err)
				}
			} else {
				runErr = errors.Join(runErr, fmt.Errorf(
					"embed person documents: endpoint health not established: %w", batchState.rejectionErr,
				))
			}
		}
		pass.checkpoint(ctx, checkpointRunCounters(result))
	}

	current := documents
	if len(changed) > 0 {
		current, err = w.deps.Store.ListPersonSemanticDocumentsContext(ctx)
		if err != nil {
			return result, fmt.Errorf("refresh person semantic documents before reconciliation: %w", err)
		}
	}
	currentIDs := make([]int64, 0, len(current))
	for _, document := range current {
		if document.Text != "" {
			currentIDs = append(currentIDs, document.PersonID)
		}
	}
	if err := w.deps.Backend.DeletePersonsNotIn(ctx, gen, currentIDs); errors.Is(err, vector.ErrGenerationRetired) {
		return result, nil
	} else if err != nil {
		return result, fmt.Errorf("reconcile deleted person vectors for generation %d: %w", gen, err)
	}
	pass.checkpoint(ctx, checkpointRunCounters(result))
	return result, runErr
}

func (w *PersonWorker) probePersonEndpoint(ctx context.Context) error {
	if err := w.deps.Gate.Check(ctx); err != nil {
		return err
	}
	vectors, err := w.deps.Client.EmbedDocuments(ctx, []DocumentInput{{Chunks: []string{
		personEndpointProbeText,
	}}})
	if err != nil {
		return err
	}
	if len(vectors) != 1 || len(vectors[0]) != 1 {
		return fmt.Errorf("person endpoint probe returned %d documents", len(vectors))
	}
	return nil
}

func (w *PersonWorker) embedPersonBatch(
	ctx context.Context,
	gen vector.GenerationID,
	batch []store.PersonSemanticDocument,
	result *RunResult,
	state *personBatchState,
) error {
	if err := w.deps.Gate.Check(ctx); err != nil {
		return err
	}
	inputs := make([]DocumentInput, len(batch))
	for i, document := range batch {
		inputs[i] = DocumentInput{Chunks: []string{
			capPersonProviderInput(document.Text, w.deps.MaxInputChars),
		}}
	}
	vectors, embedErr := w.deps.Client.EmbedDocuments(ctx, inputs)
	permanent := errors.Is(embedErr, ErrPermanent4xx) || errors.Is(embedErr, ErrDocumentTooLarge)
	if permanent && len(batch) > 1 {
		var batchErr error
		for i := range batch {
			if err := w.embedPersonBatch(ctx, gen, batch[i:i+1], result, state); err != nil {
				if errors.Is(err, vector.ErrGenerationRetired) {
					return vector.ErrGenerationRetired
				}
				batchErr = errors.Join(batchErr, err)
			}
		}
		return batchErr
	}
	if permanent && len(batch) == 1 && len(vectors) == 0 {
		result.Failed++
		if errors.Is(embedErr, ErrDocumentTooLarge) {
			return w.recordRejectedPersons(ctx, gen, batch)
		}
		state.rejected = append(state.rejected, batch[0])
		state.rejectionErr = errors.Join(state.rejectionErr, embedErr)
		return nil
	}
	if len(vectors) > len(batch) || (embedErr == nil && len(vectors) != len(batch)) {
		result.Failed += len(batch)
		return fmt.Errorf("embed person documents: returned %d results for %d inputs", len(vectors), len(batch))
	}
	if len(vectors) > 0 {
		state.endpointHealthy = true
	}

	publications := make([]vector.PersonEmbedding, 0, len(vectors))
	for i, documentVectors := range vectors {
		if len(documentVectors) != 1 {
			result.Failed += len(batch)
			return fmt.Errorf("embed person %d: expected one vector, got %d", batch[i].PersonID, len(documentVectors))
		}
		current, err := w.deps.Store.LoadPersonSemanticDocumentContext(ctx, batch[i].PersonID)
		if errors.Is(err, store.ErrPersonNotFound) {
			continue
		}
		if err != nil {
			result.Failed += len(batch)
			return fmt.Errorf("reload person %d after embedding: %w", batch[i].PersonID, err)
		}
		if current.Revision != batch[i].Revision || current.RendererPolicy != batch[i].RendererPolicy {
			continue
		}
		publications = append(publications, vector.PersonEmbedding{
			PersonID: batch[i].PersonID, Revision: batch[i].Revision, Vector: documentVectors[0],
		})
	}
	if err := w.deps.Backend.UpsertPersons(ctx, gen, publications); errors.Is(err, vector.ErrGenerationRetired) {
		return vector.ErrGenerationRetired
	} else if err != nil {
		result.Failed += len(batch)
		return fmt.Errorf("upsert person vectors for generation %d: %w", gen, err)
	}
	result.Succeeded += len(publications)
	result.Failed += len(vectors) - len(publications)
	if embedErr != nil {
		result.Failed += len(batch) - len(vectors)
		return fmt.Errorf("embed person documents: %w", embedErr)
	}
	return nil
}

func (w *PersonWorker) recordRejectedPersons(
	ctx context.Context,
	gen vector.GenerationID,
	documents []store.PersonSemanticDocument,
) error {
	publications := make([]vector.PersonEmbedding, 0, len(documents))
	for _, document := range documents {
		current, err := w.deps.Store.LoadPersonSemanticDocumentContext(ctx, document.PersonID)
		if errors.Is(err, store.ErrPersonNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reload rejected person %d: %w", document.PersonID, err)
		}
		if current.Revision != document.Revision || current.RendererPolicy != document.RendererPolicy {
			continue
		}
		publications = append(publications, vector.PersonEmbedding{
			PersonID: document.PersonID, Revision: document.Revision,
		})
	}
	if err := w.deps.Backend.UpsertPersons(ctx, gen, publications); errors.Is(err, vector.ErrGenerationRetired) {
		return vector.ErrGenerationRetired
	} else if err != nil {
		return fmt.Errorf("record rejected person revisions for generation %d: %w", gen, err)
	}
	return nil
}

func capPersonProviderInput(text string, maxRunes int) string {
	runes := []rune(text)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes])
}

// GenerationRunner is the message-owned runner contract composed with person
// reconciliation. It deliberately matches scheduler.EmbedRunner without an
// embed-to-scheduler package cycle.
type GenerationRunner interface {
	RunOnce(ctx context.Context, gen vector.GenerationID, scope operations.PassScope) (RunResult, error)
	RunBackstop(ctx context.Context, gen vector.GenerationID, scope operations.PassScope) (RunResult, error)
	ReclaimStale(ctx context.Context) (int, error)
}

// GenerationWorker runs message and person maintenance inside one scheduler
// job and one generation lifecycle.
type GenerationWorker struct {
	messages      GenerationRunner
	persons       *PersonWorker
	personScanned map[vector.GenerationID]bool
}

type personBuildingGenerationReader interface {
	BuildingGeneration(ctx context.Context) (*vector.Generation, error)
}

// GenerationRunError preserves the message and person maintenance channels.
// Schedulers may continue message recovery after a person-only failure while
// direct callers still receive the underlying person error.
type GenerationRunError struct {
	Message error
	Person  error
}

func (e *GenerationRunError) Error() string {
	joined := errors.Join(e.Message, e.Person)
	if joined == nil {
		return ""
	}
	return joined.Error()
}

func (e *GenerationRunError) Unwrap() []error {
	return []error{e.Message, e.Person}
}

// NewGenerationWorker composes the existing message runner with person
// reconciliation.
func NewGenerationWorker(messages GenerationRunner, persons *PersonWorker) *GenerationWorker {
	return &GenerationWorker{
		messages: messages, persons: persons, personScanned: make(map[vector.GenerationID]bool),
	}
}

// ReclaimStale delegates message lease maintenance; the person worker is a
// revisioned scan-and-fill reconciler and owns no leases.
func (w *GenerationWorker) ReclaimStale(ctx context.Context) (int, error) {
	if w == nil || w.messages == nil {
		return 0, errors.New("generation worker: message runner is required")
	}
	return w.messages.ReclaimStale(ctx)
}

// RunOnce maintains both corpora even when one side returns an error, so a
// transient message failure cannot indefinitely starve active-person upkeep.
func (w *GenerationWorker) RunOnce(
	ctx context.Context, gen vector.GenerationID, scope operations.PassScope,
) (RunResult, error) {
	if w == nil || w.messages == nil || w.persons == nil {
		return RunResult{}, errors.New("generation worker: message and person workers are required")
	}
	messageScope, err := scope.ForKind(operations.KindMessageEmbedding)
	if err != nil {
		return RunResult{}, fmt.Errorf("message embedding pass scope: %w", err)
	}
	messageResult, messageErr := w.messages.RunOnce(ctx, gen, messageScope)
	var personResult RunResult
	var personErr error
	if w.personScanned == nil {
		w.personScanned = make(map[vector.GenerationID]bool)
	}
	// A contextual build can require many bounded message passes. Scan the
	// person corpus on its first pass and final converged pass, not on every
	// intermediate build pass. Active generations continue person maintenance
	// on every scheduler tick, including ticks with message-side failures.
	building := false
	if reader, ok := w.persons.deps.Backend.(personBuildingGenerationReader); ok {
		generation, readErr := reader.BuildingGeneration(ctx)
		building = readErr == nil && generation != nil && generation.ID == gen
	}
	if !building || messageResult.Contextual == nil ||
		messageResult.Contextual.Converged || !w.personScanned[gen] {
		personScope, scopeErr := scope.ForKind(operations.KindPersonEmbedding)
		if scopeErr != nil {
			personErr = fmt.Errorf("person embedding pass scope: %w", scopeErr)
		} else {
			personResult, personErr = w.persons.RunOnce(ctx, gen, personScope)
		}
		w.personScanned[gen] = true
	}
	return mergeGenerationResults(messageResult, personResult), newGenerationRunError(messageErr, personErr)
}

// RunPersonsOnce performs only curated-person reconciliation. The scheduler
// uses it to maintain the compatible active generation independently while a
// message rebuild drains into another generation.
func (w *GenerationWorker) RunPersonsOnce(
	ctx context.Context, gen vector.GenerationID, scope operations.PassScope,
) (RunResult, error) {
	if w == nil || w.persons == nil {
		return RunResult{}, errors.New("generation worker: person worker is required")
	}
	personScope, err := scope.ForKind(operations.KindPersonEmbedding)
	if err != nil {
		return RunResult{}, fmt.Errorf("person embedding pass scope: %w", err)
	}
	return w.persons.RunOnce(ctx, gen, personScope)
}

// RunBackstop preserves the message runner's watermark-ignoring behavior and
// performs the same idempotent person full scan used by ordinary ticks.
func (w *GenerationWorker) RunBackstop(
	ctx context.Context, gen vector.GenerationID, scope operations.PassScope,
) (RunResult, error) {
	if w == nil || w.messages == nil || w.persons == nil {
		return RunResult{}, errors.New("generation worker: message and person workers are required")
	}
	messageScope, err := scope.ForKind(operations.KindMessageEmbedding)
	if err != nil {
		return RunResult{}, fmt.Errorf("message embedding backstop scope: %w", err)
	}
	personScope, err := scope.ForKind(operations.KindPersonEmbedding)
	if err != nil {
		return RunResult{}, fmt.Errorf("person embedding backstop scope: %w", err)
	}
	messageResult, messageErr := w.messages.RunBackstop(ctx, gen, messageScope)
	personResult, personErr := w.persons.RunOnce(ctx, gen, personScope)
	return mergeGenerationResults(messageResult, personResult), newGenerationRunError(messageErr, personErr)
}

func newGenerationRunError(messageErr, personErr error) error {
	if messageErr == nil && personErr == nil {
		return nil
	}
	return &GenerationRunError{Message: messageErr, Person: personErr}
}

func mergeGenerationResults(message, person RunResult) RunResult {
	message.Claimed += person.Claimed
	message.Succeeded += person.Succeeded
	message.Failed += person.Failed
	message.Truncated += person.Truncated
	return message
}
