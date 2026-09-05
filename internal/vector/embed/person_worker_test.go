package embed

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector"
)

type personWorkerSource struct {
	mu   sync.Mutex
	docs map[int64]store.PersonSemanticDocument
}

func newPersonWorkerSource(docs ...store.PersonSemanticDocument) *personWorkerSource {
	source := &personWorkerSource{docs: make(map[int64]store.PersonSemanticDocument, len(docs))}
	for _, document := range docs {
		source.docs[document.PersonID] = document
	}
	return source
}

func (s *personWorkerSource) ListPersonSemanticDocumentsContext(context.Context) ([]store.PersonSemanticDocument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	documents := make([]store.PersonSemanticDocument, 0, len(s.docs))
	for _, document := range s.docs {
		documents = append(documents, document)
	}
	// Return a deliberate reverse ordering so the worker's stable-order
	// contract is exercised instead of being supplied by the fake.
	sort.Slice(documents, func(i, j int) bool { return documents[i].PersonID > documents[j].PersonID })
	return documents, nil
}

func (s *personWorkerSource) LoadPersonSemanticDocumentContext(
	_ context.Context, personID int64,
) (*store.PersonSemanticDocument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	document, ok := s.docs[personID]
	if !ok {
		return nil, store.ErrPersonNotFound
	}
	return &document, nil
}

func (s *personWorkerSource) put(document store.PersonSemanticDocument) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[document.PersonID] = document
}

func (s *personWorkerSource) delete(personID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.docs, personID)
}

type personWorkerBackend struct {
	mu        sync.Mutex
	revisions map[vector.GenerationID]map[int64]string
	vectors   map[vector.GenerationID]map[int64][]float32
	listErr   error
	upsertErr error
	deleteErr error
	building  *vector.Generation
}

func (b *personWorkerBackend) BuildingGeneration(context.Context) (*vector.Generation, error) {
	if b.building == nil {
		return nil, nil //nolint:nilnil // matches the production optional building-generation contract
	}
	return b.building, nil
}

func newPersonWorkerBackend() *personWorkerBackend {
	return &personWorkerBackend{
		revisions: make(map[vector.GenerationID]map[int64]string),
		vectors:   make(map[vector.GenerationID]map[int64][]float32),
	}
}

func (b *personWorkerBackend) UpsertPersons(
	_ context.Context, gen vector.GenerationID, persons []vector.PersonEmbedding,
) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.upsertErr != nil {
		return b.upsertErr
	}
	if b.revisions[gen] == nil {
		b.revisions[gen] = make(map[int64]string)
		b.vectors[gen] = make(map[int64][]float32)
	}
	for _, person := range persons {
		b.revisions[gen][person.PersonID] = person.Revision
		b.vectors[gen][person.PersonID] = slices.Clone(person.Vector)
	}
	return nil
}

func (b *personWorkerBackend) ListPersonRevisions(
	_ context.Context, gen vector.GenerationID,
) (map[int64]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.listErr != nil {
		return nil, b.listErr
	}
	return clonePersonRevisions(b.revisions[gen]), nil
}

func (b *personWorkerBackend) CountRejectedPersons(
	_ context.Context, gen vector.GenerationID,
) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var count int64
	for personID := range b.revisions[gen] {
		if len(b.vectors[gen][personID]) == 0 {
			count++
		}
	}
	return count, nil
}

func (b *personWorkerBackend) DeletePersonsNotIn(
	_ context.Context, gen vector.GenerationID, currentPersonIDs []int64,
) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deleteErr != nil {
		return b.deleteErr
	}
	keep := make(map[int64]struct{}, len(currentPersonIDs))
	for _, personID := range currentPersonIDs {
		keep[personID] = struct{}{}
	}
	for personID := range b.revisions[gen] {
		if _, ok := keep[personID]; !ok {
			delete(b.revisions[gen], personID)
			delete(b.vectors[gen], personID)
		}
	}
	return nil
}

func (b *personWorkerBackend) SearchPeople(
	context.Context, vector.GenerationID, []float32, int,
) ([]vector.PersonHit, error) {
	return nil, errors.New("not implemented")
}

func clonePersonRevisions(input map[int64]string) map[int64]string {
	return maps.Clone(input)
}

type personWorkerClient struct {
	mu              sync.Mutex
	calls           [][][]string
	failOnceAfter   int
	failureReturned bool
	malformed       bool
	onEmbed         func()
}

type poisonPersonWorkerClient struct {
	calls          int
	rejectAll      bool
	rejectFromCall int
}

func (c *poisonPersonWorkerClient) EmbedQuery(context.Context, string) ([]float32, error) {
	return nil, errors.New("unexpected query embedding")
}

func (c *poisonPersonWorkerClient) EmbedDocuments(
	_ context.Context, documents []DocumentInput,
) ([][][]float32, error) {
	c.calls++
	for _, document := range documents {
		if c.rejectAll || (c.rejectFromCall > 0 && c.calls >= c.rejectFromCall) ||
			document.Chunks[0] == "Synthetic Reject" {
			return nil, fmt.Errorf("synthetic rejection: %w", ErrPermanent4xx)
		}
	}
	results := make([][][]float32, len(documents))
	for i := range documents {
		results[i] = [][]float32{{float32(i + 1), 1}}
	}
	return results, nil
}

func (c *personWorkerClient) EmbedQuery(context.Context, string) ([]float32, error) {
	return nil, errors.New("not implemented")
}

func (c *personWorkerClient) EmbedDocuments(
	_ context.Context, documents []DocumentInput,
) ([][][]float32, error) {
	c.mu.Lock()
	inputs := make([][]string, len(documents))
	for i, document := range documents {
		inputs[i] = slices.Clone(document.Chunks)
	}
	c.calls = append(c.calls, inputs)
	onEmbed := c.onEmbed
	failAfter := c.failOnceAfter
	shouldFail := failAfter > 0 && !c.failureReturned
	if shouldFail {
		c.failureReturned = true
	}
	c.mu.Unlock()

	if onEmbed != nil {
		onEmbed()
	}
	if c.malformed {
		return nil, nil
	}
	completed := len(documents)
	if shouldFail {
		completed = min(failAfter, len(documents))
	}
	results := make([][][]float32, completed)
	for i := range completed {
		requireDocumentShape := len(documents[i].Chunks) == 1
		if !requireDocumentShape {
			return nil, errors.New("person input must contain exactly one chunk")
		}
		results[i] = [][]float32{{float32(len(documents[i].Chunks[0])), float32(i + 1)}}
	}
	if shouldFail {
		return results, errors.New("synthetic provider interruption")
	}
	return results, nil
}

func personDocument(id int64, revision, text string) store.PersonSemanticDocument {
	return store.PersonSemanticDocument{
		PersonID: id, RendererPolicy: store.PersonSemanticRendererPolicy,
		Revision: revision, Text: text,
	}
}

func newTestPersonWorker(
	source PersonDocumentStore, backend vector.PersonBackend, client SemanticClient, batchSize int,
) *PersonWorker {
	return NewPersonWorker(PersonWorkerDeps{
		Store: source, Backend: backend, Client: client, BatchSize: batchSize,
		Gate: allowSemanticPersonGate(), Recorder: newTestOperationRecorder(),
	})
}

func allowSemanticPersonGate() vector.SemanticPersonEmbeddingGate {
	return vector.SemanticPersonEmbeddingGateFunc(func(context.Context) error { return nil })
}

// TestPersonWorkerGateBlocksDisabledUnconsentedAndRevokedProviderCalls catches
// the reconciler treating vector enablement as authority for curated egress.
func TestPersonWorkerGateBlocksDisabledUnconsentedAndRevokedProviderCalls(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(personDocument(1, "rev-one", "Synthetic One"))
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{}
	gateErr := vector.ErrSemanticPersonEmbeddingsDisabled
	worker := NewPersonWorker(PersonWorkerDeps{
		Store: source, Backend: backend, Client: client, BatchSize: 1,
		Gate:     vector.SemanticPersonEmbeddingGateFunc(func(context.Context) error { return gateErr }),
		Recorder: newTestOperationRecorder(),
	})

	result, err := worker.RunOnce(t.Context(), 1, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(RunResult{}, result)
	check.Empty(client.calls)

	gateErr = vector.ErrSemanticPersonEmbeddingConsentRequired
	result, err = worker.RunOnce(t.Context(), 1, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(RunResult{}, result)
	check.Empty(client.calls)

	gateErr = nil
	_, err = worker.RunOnce(t.Context(), 1, testEmbeddingPassScope())
	must.NoError(err)
	must.Len(client.calls, 1)

	source.put(personDocument(1, "rev-two", "Synthetic Two"))
	gateErr = vector.ErrSemanticPersonEmbeddingConsentRequired
	_, err = worker.RunOnce(t.Context(), 1, testEmbeddingPassScope())
	must.NoError(err)
	check.Len(client.calls, 1, "revoked consent must make zero later provider calls")
}

// TestPersonWorkerRechecksGateBeforeEveryProviderCall catches a mid-run
// revocation being ignored for later batches or endpoint probes.
func TestPersonWorkerRechecksGateBeforeEveryProviderCall(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(
		personDocument(1, "rev-one", "Synthetic One"),
		personDocument(2, "rev-two", "Synthetic Two"),
	)
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{}
	gateErr := error(nil)
	client.onEmbed = func() { gateErr = vector.ErrSemanticPersonEmbeddingConsentRequired }
	worker := NewPersonWorker(PersonWorkerDeps{
		Store: source, Backend: backend, Client: client, BatchSize: 1,
		Gate:     vector.SemanticPersonEmbeddingGateFunc(func(context.Context) error { return gateErr }),
		Recorder: newTestOperationRecorder(),
	})

	_, err := worker.RunOnce(t.Context(), 2, testEmbeddingPassScope())
	must.NoError(err)
	check.Len(client.calls, 1)
	revisions, err := backend.ListPersonRevisions(t.Context(), 2)
	must.NoError(err)
	check.Equal(map[int64]string{1: "rev-one"}, revisions)
}

func TestPersonWorkerMissingGateFailsClosedBeforeProvider(t *testing.T) {
	client := &personWorkerClient{}
	worker := NewPersonWorker(PersonWorkerDeps{
		Store:   newPersonWorkerSource(personDocument(1, "rev-one", "Synthetic One")),
		Backend: newPersonWorkerBackend(), Client: client,
		Recorder: newTestOperationRecorder(),
	})
	_, err := worker.RunOnce(t.Context(), 1, testEmbeddingPassScope())
	require.ErrorContains(t, err, "gate")
	assert.Empty(t, client.calls)
}

// TestPersonWorkerEmbedsInitialAndChangedDocumentsInStableBatches catches a
// worker that ignores revisions, embeds in unstable order, or fails to replace
// a changed person's vector.
func TestPersonWorkerEmbedsInitialAndChangedDocumentsInStableBatches(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(
		personDocument(30, "rev-30-a", "Synthetic Thirty"),
		personDocument(10, "rev-10-a", "Synthetic Ten"),
		personDocument(20, "rev-20-a", "Synthetic Twenty"),
	)
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{}
	worker := newTestPersonWorker(source, backend, client, 2)

	result, err := worker.RunOnce(t.Context(), 7, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(3, result.Claimed)
	check.Equal(3, result.Succeeded)
	revisions, err := backend.ListPersonRevisions(t.Context(), 7)
	must.NoError(err)
	check.Equal(map[int64]string{10: "rev-10-a", 20: "rev-20-a", 30: "rev-30-a"}, revisions)
	must.Len(client.calls, 2)
	check.Equal([][]string{{"Synthetic Ten"}, {"Synthetic Twenty"}}, client.calls[0])
	check.Equal([][]string{{"Synthetic Thirty"}}, client.calls[1])

	source.put(personDocument(20, "rev-20-b", "Synthetic Twenty Updated"))
	result, err = worker.RunOnce(t.Context(), 7, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(1, result.Claimed)
	check.Equal(1, result.Succeeded)
	revisions, err = backend.ListPersonRevisions(t.Context(), 7)
	must.NoError(err)
	check.Equal("rev-20-b", revisions[20])
	must.Len(client.calls, 3)
	check.Equal([][]string{{"Synthetic Twenty Updated"}}, client.calls[2])
}

// TestPersonWorkerNoOpDoesNotCallProvider catches full rescans that re-embed
// already-current person documents instead of comparing exact revisions.
func TestPersonWorkerNoOpDoesNotCallProvider(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(personDocument(1, "rev-1", "Synthetic One"))
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{}
	worker := newTestPersonWorker(source, backend, client, 8)
	_, err := worker.RunOnce(t.Context(), 8, testEmbeddingPassScope())
	must.NoError(err)
	must.Len(client.calls, 1)

	result, err := worker.RunOnce(t.Context(), 8, testEmbeddingPassScope())
	must.NoError(err)
	check.Zero(result.Claimed)
	check.Len(client.calls, 1, "unchanged person must not call the provider again")
}

// TestPersonWorkerSkipsEmptyCanonicalDocument catches a valid person with no
// discoverability text being sent to the provider or blocking convergence.
func TestPersonWorkerSkipsEmptyCanonicalDocument(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(personDocument(2, "rev-empty", ""))
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{}
	worker := newTestPersonWorker(source, backend, client, 8)

	result, err := worker.RunOnce(t.Context(), 8, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(RunResult{}, result)
	check.Empty(client.calls, "empty semantic documents must not reach the provider")
	revisions, err := backend.ListPersonRevisions(t.Context(), 8)
	must.NoError(err)
	check.Empty(revisions)

	result, err = worker.RunOnce(t.Context(), 8, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(RunResult{}, result)
	check.Empty(client.calls, "unchanged empty document must remain provider-free")
}

func TestPersonWorkerCapsProviderInput(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(personDocument(2, "rev-capped", "Synthetic Profile"))
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{}
	worker := NewPersonWorker(PersonWorkerDeps{
		Store: source, Backend: backend, Client: client, BatchSize: 8, MaxInputChars: 9,
		Gate:     allowSemanticPersonGate(),
		Recorder: newTestOperationRecorder(),
	})

	result, err := worker.RunOnce(t.Context(), 8, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(RunResult{Claimed: 1, Succeeded: 1, Truncated: 1}, result)
	must.Len(client.calls, 1)
	check.Equal([][]string{{"Synthetic"}}, client.calls[0])
}

func TestPersonWorkerRemovesVectorWhenDocumentBecomesEmpty(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(personDocument(2, "rev-full", "Synthetic Profile"))
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{}
	worker := newTestPersonWorker(source, backend, client, 8)

	_, err := worker.RunOnce(t.Context(), 8, testEmbeddingPassScope())
	must.NoError(err)
	must.Len(client.calls, 1)
	source.put(personDocument(2, "rev-empty", ""))

	result, err := worker.RunOnce(t.Context(), 8, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(RunResult{}, result)
	check.Len(client.calls, 1, "empty transition must not call the provider")
	revisions, err := backend.ListPersonRevisions(t.Context(), 8)
	must.NoError(err)
	check.Empty(revisions, "empty transition must remove the old searchable vector")
}

func TestPersonWorkerRecordsPoisonDocumentAndContinues(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(
		personDocument(1, "rev-one", "Synthetic One"),
		personDocument(2, "rev-reject", "Synthetic Reject"),
		personDocument(3, "rev-three", "Synthetic Three"),
	)
	backend := newPersonWorkerBackend()
	client := &poisonPersonWorkerClient{}
	worker := NewPersonWorker(PersonWorkerDeps{
		Store: source, Backend: backend, Client: client, BatchSize: 3, MaxInputChars: 100,
		Gate:     allowSemanticPersonGate(),
		Recorder: newTestOperationRecorder(),
	})

	result, err := worker.RunOnce(t.Context(), 8, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(RunResult{Claimed: 3, Succeeded: 2, Failed: 1}, result)
	revisions, err := backend.ListPersonRevisions(t.Context(), 8)
	must.NoError(err)
	check.Equal(map[int64]string{1: "rev-one", 2: "rev-reject", 3: "rev-three"}, revisions)
	check.NotEmpty(backend.vectors[8][1])
	check.Empty(backend.vectors[8][2], "rejected revision must not own a searchable vector")
	check.NotEmpty(backend.vectors[8][3])
	check.Equal(4, client.calls, "rejected batch must downshift to single documents")

	result, err = worker.RunOnce(t.Context(), 8, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(RunResult{}, result)
	check.Equal(4, client.calls, "durable terminal revision must not be retried")
}

func TestPersonWorkerDoesNotHideEndpointWidePermanentFailure(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(
		personDocument(1, "rev-one", "Synthetic One"),
		personDocument(2, "rev-two", "Synthetic Two"),
		personDocument(3, "rev-three", "Synthetic Three"),
	)
	backend := newPersonWorkerBackend()
	client := &poisonPersonWorkerClient{rejectAll: true}
	worker := NewPersonWorker(PersonWorkerDeps{
		Store: source, Backend: backend, Client: client, BatchSize: 3, MaxInputChars: 100,
		Gate:     allowSemanticPersonGate(),
		Recorder: newTestOperationRecorder(),
	})

	result, err := worker.RunOnce(t.Context(), 8, testEmbeddingPassScope())
	must.ErrorIs(err, ErrPermanent4xx)
	check.Equal(RunResult{Claimed: 3, Failed: 3}, result)
	revisions, listErr := backend.ListPersonRevisions(t.Context(), 8)
	must.NoError(listErr)
	check.Empty(revisions, "an endpoint-wide failure must remain uncovered and retryable")
	check.Equal(5, client.calls, "the failed batch and synthetic health probe must remain visible")
}

func TestPersonWorkerTerminatesLonePoisonRevisionAfterHealthyProbe(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(personDocument(1, "rev-reject", "Synthetic Reject"))
	backend := newPersonWorkerBackend()
	client := &poisonPersonWorkerClient{}
	worker := newTestPersonWorker(source, backend, client, 1)

	result, err := worker.RunOnce(t.Context(), 8, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(RunResult{Claimed: 1, Failed: 1}, result)
	revisions, listErr := backend.ListPersonRevisions(t.Context(), 8)
	must.NoError(listErr)
	check.Equal(map[int64]string{1: "rev-reject"}, revisions)
	check.Empty(backend.vectors[8][1])
	check.Equal(2, client.calls, "a synthetic probe must establish endpoint health")

	result, err = worker.RunOnce(t.Context(), 8, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(RunResult{}, result)
	check.Equal(2, client.calls, "the terminal revision must not be retried")
}

// TestPersonWorkerFailureOperationUsesFinalCounters catches a nil worker error
// overriding a durable item rejection and falsely reporting success.
func TestPersonWorkerFailureOperationUsesFinalCounters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	recorder := testutil.NewTestStore(t)
	worker := NewPersonWorker(PersonWorkerDeps{
		Store:   newPersonWorkerSource(personDocument(1, "rev-reject", "Synthetic Reject")),
		Backend: newPersonWorkerBackend(), Client: &poisonPersonWorkerClient{},
		BatchSize: 1, Gate: allowSemanticPersonGate(), Recorder: recorder,
	})

	result, err := worker.RunOnce(t.Context(), 8, operations.PassScope{
		Key: "manual:person:lone-rejection", Trigger: operations.TriggerManual,
		StartedAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	assert.Equal(RunResult{Claimed: 1, Failed: 1}, result)
	runs := personOperationRuns(t, recorder)
	require.Len(runs, 1)
	assert.Equal(operations.StateFailed, runs[0].State)
	assert.Equal(int64(1), personOperationCounter(runs[0], operations.CounterAttempted))
	assert.Equal(int64(1), personOperationCounter(runs[0], operations.CounterFailed))
}

// TestPersonWorkerFailureOperationRecordsMixedOutcomeAsPartial catches useful
// publications being hidden by a sibling's permanent rejection.
func TestPersonWorkerFailureOperationRecordsMixedOutcomeAsPartial(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	recorder := testutil.NewTestStore(t)
	worker := NewPersonWorker(PersonWorkerDeps{
		Store: newPersonWorkerSource(
			personDocument(1, "rev-one", "Synthetic One"),
			personDocument(2, "rev-reject", "Synthetic Reject"),
		),
		Backend: newPersonWorkerBackend(), Client: &poisonPersonWorkerClient{},
		BatchSize: 2, Gate: allowSemanticPersonGate(), Recorder: recorder,
	})

	result, err := worker.RunOnce(t.Context(), 8, operations.PassScope{
		Key: "manual:person:mixed", Trigger: operations.TriggerManual,
		StartedAt: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	assert.Equal(RunResult{Claimed: 2, Succeeded: 1, Failed: 1}, result)
	runs := personOperationRuns(t, recorder)
	require.Len(runs, 1)
	assert.Equal(operations.StatePartial, runs[0].State)
	assert.Equal(int64(1), personOperationCounter(runs[0], operations.CounterSucceeded))
	assert.Equal(int64(1), personOperationCounter(runs[0], operations.CounterFailed))
}

func personOperationRuns(t *testing.T, recorder *store.Store) []operations.Run {
	t.Helper()
	snapshot, err := recorder.ListRuns(t.Context(), operations.Query{
		Kinds: []operations.Kind{operations.KindPersonEmbedding}, Limit: 100,
	})
	require.NoError(t, err)
	return snapshot.Runs
}

func personOperationCounter(run operations.Run, name operations.CounterName) int64 {
	for _, counter := range run.Counters {
		if counter.Name == name {
			return counter.Value
		}
	}
	return 0
}

func TestPersonWorkerScopesEndpointHealthToFailingBatch(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(
		personDocument(1, "rev-one", "Synthetic One"),
		personDocument(2, "rev-two", "Synthetic Two"),
		personDocument(3, "rev-three", "Synthetic Three"),
		personDocument(4, "rev-four", "Synthetic Four"),
	)
	backend := newPersonWorkerBackend()
	client := &poisonPersonWorkerClient{rejectFromCall: 2}
	worker := NewPersonWorker(PersonWorkerDeps{
		Store: source, Backend: backend, Client: client, BatchSize: 2, MaxInputChars: 100,
		Gate:     allowSemanticPersonGate(),
		Recorder: newTestOperationRecorder(),
	})

	result, err := worker.RunOnce(t.Context(), 8, testEmbeddingPassScope())
	must.ErrorIs(err, ErrPermanent4xx)
	check.Equal(RunResult{Claimed: 4, Succeeded: 2, Failed: 2}, result)
	revisions, listErr := backend.ListPersonRevisions(t.Context(), 8)
	must.NoError(listErr)
	check.Equal(map[int64]string{1: "rev-one", 2: "rev-two"}, revisions,
		"success in an earlier batch must not hide a later endpoint-wide failure")
	check.Equal(5, client.calls)
}

func TestPersonWorkerPreservesVectorWhenSourceProjectionCannotRender(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	var personID int64
	err := f.Store.DB().QueryRow(f.Store.Rebind(`
		INSERT INTO persons (vcard_uid, display_name) VALUES (?, ?)
		RETURNING id`),
		"urn:uuid:00000000-0000-4000-8000-000000000099", "Broken Projection").Scan(&personID)
	must.NoError(err)
	value := "distributed systems"
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: store.AttributeSlugAskMeAbout,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &value},
		Source: store.ProvenanceUser,
	})
	must.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE attribute_definitions
		SET value_type = 'record_reference', field_type = 'person', record_target = 'person'
		WHERE universal_id = ?
	`), store.AttributeUniversalIDAskMeAbout)
	must.NoError(err)

	backend := newPersonWorkerBackend()
	must.NoError(backend.UpsertPersons(t.Context(), 8, []vector.PersonEmbedding{{
		PersonID: personID, Revision: "last-good", Vector: []float32{1, 0},
	}}))
	client := &personWorkerClient{}
	worker := newTestPersonWorker(f.Store, backend, client, 8)

	_, err = worker.RunOnce(t.Context(), 8, testEmbeddingPassScope())
	must.ErrorContains(err, "render person")
	revisions, listErr := backend.ListPersonRevisions(t.Context(), 8)
	must.NoError(listErr)
	check.Equal(map[int64]string{personID: "last-good"}, revisions)
	check.Empty(client.calls)
}

func TestPersonWorkerStopsProviderCallsWhenGenerationRetiresMidRun(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(
		personDocument(1, "rev-one", "Synthetic One"),
		personDocument(2, "rev-two", "Synthetic Two"),
		personDocument(3, "rev-three", "Synthetic Three"),
	)
	backend := newPersonWorkerBackend()
	backend.upsertErr = vector.ErrGenerationRetired
	client := &personWorkerClient{}
	worker := newTestPersonWorker(source, backend, client, 1)

	_, err := worker.RunOnce(t.Context(), 9, testEmbeddingPassScope())
	must.NoError(err)
	check.Len(client.calls, 1, "retirement must stop later paid provider batches")
}

func TestPersonWorkerStopsDownshiftWhenGenerationRetires(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(
		personDocument(1, "rev-one", "Synthetic One"),
		personDocument(2, "rev-reject", "Synthetic Reject"),
	)
	backend := newPersonWorkerBackend()
	backend.upsertErr = vector.ErrGenerationRetired
	client := &poisonPersonWorkerClient{}
	worker := NewPersonWorker(PersonWorkerDeps{
		Store: source, Backend: backend, Client: client, BatchSize: 2, MaxInputChars: 100,
		Gate:     allowSemanticPersonGate(),
		Recorder: newTestOperationRecorder(),
	})

	_, err := worker.RunOnce(t.Context(), 9, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(2, client.calls,
		"retirement on the first singleton upsert must stop the rest of the downshift")
}

func TestPersonWorkerTreatsRetiredGenerationAsBenign(t *testing.T) {
	document := personDocument(1, "rev-one", "Synthetic One")
	tests := []struct {
		name    string
		prepare func(*personWorkerBackend)
	}{
		{name: "list", prepare: func(backend *personWorkerBackend) { backend.listErr = vector.ErrGenerationRetired }},
		{name: "upsert", prepare: func(backend *personWorkerBackend) { backend.upsertErr = vector.ErrGenerationRetired }},
		{name: "reconcile", prepare: func(backend *personWorkerBackend) {
			backend.revisions[9] = map[int64]string{1: document.Revision}
			backend.deleteErr = vector.ErrGenerationRetired
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := newPersonWorkerBackend()
			test.prepare(backend)
			worker := newTestPersonWorker(
				newPersonWorkerSource(document), backend, &personWorkerClient{}, 8,
			)
			_, err := worker.RunOnce(t.Context(), 9, testEmbeddingPassScope())
			require.NoError(t, err)
		})
	}
}

// TestPersonWorkerReconcilesDeletedPeople catches orphaned person vectors
// surviving after their durable source person has been deleted.
func TestPersonWorkerReconcilesDeletedPeople(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(
		personDocument(1, "rev-1", "Synthetic One"),
		personDocument(2, "rev-2", "Synthetic Two"),
	)
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{}
	worker := newTestPersonWorker(source, backend, client, 8)
	_, err := worker.RunOnce(t.Context(), 9, testEmbeddingPassScope())
	must.NoError(err)

	source.delete(1)
	result, err := worker.RunOnce(t.Context(), 9, testEmbeddingPassScope())
	must.NoError(err)
	check.Zero(result.Claimed)
	revisions, err := backend.ListPersonRevisions(t.Context(), 9)
	must.NoError(err)
	check.Equal(map[int64]string{2: "rev-2"}, revisions)
	check.Len(client.calls, 1, "deletion reconciliation must not call the provider")
}

// TestPersonWorkerDiscardsPostEmbedMutation catches publication of a provider
// response generated from a revision that changed during the request.
func TestPersonWorkerDiscardsPostEmbedMutation(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(personDocument(1, "rev-old", "Synthetic Before"))
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{}
	client.onEmbed = func() {
		source.put(personDocument(1, "rev-new", "Synthetic After"))
		client.mu.Lock()
		client.onEmbed = nil
		client.mu.Unlock()
	}
	worker := newTestPersonWorker(source, backend, client, 8)

	result, err := worker.RunOnce(t.Context(), 10, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(1, result.Claimed)
	check.Zero(result.Succeeded)
	revisions, err := backend.ListPersonRevisions(t.Context(), 10)
	must.NoError(err)
	check.Empty(revisions, "stale provider response must not be published")

	result, err = worker.RunOnce(t.Context(), 10, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(1, result.Succeeded)
	revisions, err = backend.ListPersonRevisions(t.Context(), 10)
	must.NoError(err)
	check.Equal(map[int64]string{1: "rev-new"}, revisions)
}

// TestPersonWorkerPersistsPartialPrefixAndRetriesOnlyRemainder catches a
// worker that drops successful provider-prefix results or re-embeds them on
// retry after a later document fails.
func TestPersonWorkerPersistsPartialPrefixAndRetriesOnlyRemainder(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(
		personDocument(1, "rev-1", "Synthetic One"),
		personDocument(2, "rev-2", "Synthetic Two"),
		personDocument(3, "rev-3", "Synthetic Three"),
	)
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{failOnceAfter: 2}
	worker := newTestPersonWorker(source, backend, client, 3)

	result, err := worker.RunOnce(t.Context(), 11, testEmbeddingPassScope())
	must.ErrorContains(err, "synthetic provider interruption")
	check.Equal(3, result.Claimed)
	check.Equal(2, result.Succeeded)
	revisions, listErr := backend.ListPersonRevisions(t.Context(), 11)
	must.NoError(listErr)
	check.Equal(map[int64]string{1: "rev-1", 2: "rev-2"}, revisions)

	result, err = worker.RunOnce(t.Context(), 11, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(1, result.Claimed)
	check.Equal(1, result.Succeeded)
	must.Len(client.calls, 2)
	check.Equal([][]string{{"Synthetic Three"}}, client.calls[1])
	revisions, listErr = backend.ListPersonRevisions(t.Context(), 11)
	must.NoError(listErr)
	check.Equal(map[int64]string{1: "rev-1", 2: "rev-2", 3: "rev-3"}, revisions)
}

// TestPersonWorkerPartialErrorCountsReturnedFencedDocuments catches result
// accounting that drops provider responses rejected by the source fence.
func TestPersonWorkerPartialErrorCountsReturnedFencedDocuments(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(
		personDocument(1, "rev-old", "Synthetic Before"),
		personDocument(2, "rev-2", "Synthetic Two"),
	)
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{failOnceAfter: 2}
	client.onEmbed = func() {
		source.put(personDocument(1, "rev-new", "Synthetic After"))
	}
	worker := newTestPersonWorker(source, backend, client, 2)

	result, err := worker.RunOnce(t.Context(), 12, testEmbeddingPassScope())
	must.ErrorContains(err, "synthetic provider interruption")
	check.Equal(2, result.Claimed)
	check.Equal(1, result.Succeeded)
	check.Equal(1, result.Failed)
	check.Equal(result.Claimed, result.Succeeded+result.Failed)
	revisions, listErr := backend.ListPersonRevisions(t.Context(), 12)
	must.NoError(listErr)
	check.Equal(map[int64]string{2: "rev-2"}, revisions)
}

func TestPersonWorkerAccountsMalformedProviderBatchAsFailed(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(
		personDocument(1, "rev-one", "Synthetic One"),
		personDocument(2, "rev-two", "Synthetic Two"),
	)
	backend := newPersonWorkerBackend()
	worker := newTestPersonWorker(source, backend, &personWorkerClient{malformed: true}, 2)

	result, err := worker.RunOnce(t.Context(), 12, testEmbeddingPassScope())
	must.ErrorContains(err, "returned 0 results for 2 inputs")
	check.Equal(RunResult{Claimed: 2, Failed: 2}, result)
}

func TestPersonWorkerAccountsBackendWriteFailure(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(
		personDocument(1, "rev-one", "Synthetic One"),
		personDocument(2, "rev-two", "Synthetic Two"),
	)
	backend := newPersonWorkerBackend()
	backend.upsertErr = errors.New("synthetic backend failure")
	worker := newTestPersonWorker(source, backend, &personWorkerClient{}, 2)

	result, err := worker.RunOnce(t.Context(), 12, testEmbeddingPassScope())
	must.ErrorContains(err, "synthetic backend failure")
	check.Equal(RunResult{Claimed: 2, Failed: 2}, result)
}

// TestPersonWorkerEarlyBatchErrorContinuesLaterBatches catches one failed
// document starving unrelated later batches. The retry processes only the
// unpublished document.
func TestPersonWorkerEarlyBatchErrorContinuesLaterBatches(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(
		personDocument(1, "rev-1", "Synthetic One"),
		personDocument(2, "rev-2", "Synthetic Two"),
		personDocument(3, "rev-3", "Synthetic Three"),
		personDocument(4, "rev-4", "Synthetic Four"),
	)
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{failOnceAfter: 1}
	worker := newTestPersonWorker(source, backend, client, 2)

	result, err := worker.RunOnce(t.Context(), 13, testEmbeddingPassScope())
	must.ErrorContains(err, "synthetic provider interruption")
	check.Equal(RunResult{Claimed: 4, Succeeded: 3, Failed: 1}, result)
	check.Equal(result.Claimed, result.Succeeded+result.Failed)
	revisions, listErr := backend.ListPersonRevisions(t.Context(), 13)
	must.NoError(listErr)
	check.Equal(map[int64]string{1: "rev-1", 3: "rev-3", 4: "rev-4"}, revisions)
	must.Len(client.calls, 2)
	check.Equal([][]string{{"Synthetic One"}, {"Synthetic Two"}}, client.calls[0])
	check.Equal([][]string{{"Synthetic Three"}, {"Synthetic Four"}}, client.calls[1])

	result, err = worker.RunOnce(t.Context(), 13, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal(RunResult{Claimed: 1, Succeeded: 1}, result)
	revisions, listErr = backend.ListPersonRevisions(t.Context(), 13)
	must.NoError(listErr)
	check.Equal(map[int64]string{
		1: "rev-1", 2: "rev-2", 3: "rev-3", 4: "rev-4",
	}, revisions)
	must.Len(client.calls, 3)
	check.Equal([][]string{{"Synthetic Two"}}, client.calls[2])
}

type personWorkerMessageRunner struct {
	runOnce        []vector.GenerationID
	runBackstop    []vector.GenerationID
	runOnceResult  *RunResult
	runOnceErr     error
	runBackstopErr error
}

func (r *personWorkerMessageRunner) RunOnce(
	_ context.Context, gen vector.GenerationID, _ operations.PassScope,
) (RunResult, error) {
	r.runOnce = append(r.runOnce, gen)
	if r.runOnceResult != nil {
		return *r.runOnceResult, r.runOnceErr
	}
	return RunResult{Claimed: 2, Succeeded: 2}, r.runOnceErr
}

func (r *personWorkerMessageRunner) RunBackstop(
	_ context.Context, gen vector.GenerationID, _ operations.PassScope,
) (RunResult, error) {
	r.runBackstop = append(r.runBackstop, gen)
	return RunResult{}, r.runBackstopErr
}

func (r *personWorkerMessageRunner) ReclaimStale(context.Context) (int, error) { return 0, nil }

// TestGenerationWorkerMaintainsPeopleForAnActiveGeneration catches runtime
// composition that drains only message work when the scheduler targets an
// already-active generation.
func TestGenerationWorkerMaintainsPeopleForAnActiveGeneration(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(personDocument(5, "rev-active", "Synthetic Active"))
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{}
	messages := &personWorkerMessageRunner{}
	worker := NewGenerationWorker(messages, newTestPersonWorker(source, backend, client, 8))

	result, err := worker.RunOnce(t.Context(), 42, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal([]vector.GenerationID{42}, messages.runOnce)
	check.Equal(3, result.Claimed)
	check.Equal(3, result.Succeeded)
	revisions, err := backend.ListPersonRevisions(t.Context(), 42)
	must.NoError(err)
	check.Equal(map[int64]string{5: "rev-active"}, revisions)
}

// TestGenerationWorkerMaintainsPeopleWhenMessageRunFails catches an active
// generation's person upkeep being skipped after a message-side error.
func TestGenerationWorkerMaintainsPeopleWhenMessageRunFails(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	messageErr := errors.New("synthetic message failure")
	source := newPersonWorkerSource(personDocument(6, "rev-active", "Synthetic Active"))
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{}
	messages := &personWorkerMessageRunner{runOnceErr: messageErr}
	worker := NewGenerationWorker(messages, newTestPersonWorker(source, backend, client, 8))

	result, err := worker.RunOnce(t.Context(), 43, testEmbeddingPassScope())
	must.ErrorIs(err, messageErr)
	check.Equal([]vector.GenerationID{43}, messages.runOnce)
	check.Equal(RunResult{Claimed: 3, Succeeded: 3}, result)
	revisions, listErr := backend.ListPersonRevisions(t.Context(), 43)
	must.NoError(listErr)
	check.Equal(map[int64]string{6: "rev-active"}, revisions)
}

func TestGenerationWorkerScansPeopleOnlyAtContextualBuildBoundaries(t *testing.T) {
	must := require.New(t)
	source := newPersonWorkerSource(personDocument(7, "rev-one", "Synthetic One"))
	backend := newPersonWorkerBackend()
	backend.building = &vector.Generation{ID: 44}
	client := &personWorkerClient{}
	messages := &personWorkerMessageRunner{runOnceResult: &RunResult{
		Contextual: &ContextConvergence{Converged: false},
	}}
	worker := NewGenerationWorker(messages, newTestPersonWorker(source, backend, client, 8))

	_, err := worker.RunOnce(t.Context(), 44, testEmbeddingPassScope())
	must.NoError(err)
	source.put(personDocument(7, "rev-two", "Synthetic Two"))
	_, err = worker.RunOnce(t.Context(), 44, testEmbeddingPassScope())
	must.NoError(err)
	must.Len(client.calls, 1, "intermediate contextual pass must skip an unchanged corpus scan")

	messages.runOnceResult.Contextual.Converged = true
	_, err = worker.RunOnce(t.Context(), 44, testEmbeddingPassScope())
	must.NoError(err)
	must.Len(client.calls, 2, "final contextual pass must recheck the person corpus")
}

// TestGenerationWorkerPersonPassOwnershipRecordsOnlyExecutedScans catches the
// coordinator opening person rows for skipped intermediate contextual passes.
func TestGenerationWorkerPersonPassOwnershipRecordsOnlyExecutedScans(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	recorder := testutil.NewTestStore(t)
	source := newPersonWorkerSource(personDocument(7, "rev-one", "Synthetic One"))
	backend := newPersonWorkerBackend()
	backend.building = &vector.Generation{ID: 44}
	messages := &personWorkerMessageRunner{runOnceResult: &RunResult{
		Contextual: &ContextConvergence{Converged: false},
	}}
	persons := NewPersonWorker(PersonWorkerDeps{
		Store: source, Backend: backend, Client: &personWorkerClient{}, BatchSize: 8,
		Gate: allowSemanticPersonGate(), Recorder: recorder,
	})
	worker := NewGenerationWorker(messages, persons)
	started := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	_, err := worker.RunOnce(t.Context(), 44, operations.PassScope{
		Key: "manual:generation:first", Trigger: operations.TriggerManual, StartedAt: started,
	})
	require.NoError(err)
	_, err = worker.RunOnce(t.Context(), 44, operations.PassScope{
		Key: "manual:generation:middle", Trigger: operations.TriggerManual, StartedAt: started.Add(time.Second),
	})
	require.NoError(err)
	assert.Len(personOperationRuns(t, recorder), 1, "skipped person scan must create no row")

	messages.runOnceResult.Contextual.Converged = true
	_, err = worker.RunOnce(t.Context(), 44, operations.PassScope{
		Key: "manual:generation:final", Trigger: operations.TriggerManual, StartedAt: started.Add(2 * time.Second),
	})
	require.NoError(err)
	runs := personOperationRuns(t, recorder)
	require.Len(runs, 2, "first and final executed scans each own one row")
	assert.Equal([]operations.State{operations.StateSucceeded, operations.StateSucceeded},
		[]operations.State{runs[0].State, runs[1].State})
}

func TestGenerationWorkerMaintainsActivePeopleDuringContextualFailures(t *testing.T) {
	must := require.New(t)
	source := newPersonWorkerSource(personDocument(7, "rev-one", "Synthetic One"))
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{}
	messages := &personWorkerMessageRunner{runOnceResult: &RunResult{
		Failed: 1, Contextual: &ContextConvergence{Converged: false},
	}}
	worker := NewGenerationWorker(messages, newTestPersonWorker(source, backend, client, 8))

	_, err := worker.RunOnce(t.Context(), 45, testEmbeddingPassScope())
	must.NoError(err)
	source.put(personDocument(7, "rev-two", "Synthetic Two"))
	_, err = worker.RunOnce(t.Context(), 45, testEmbeddingPassScope())
	must.NoError(err)
	must.Len(client.calls, 2, "active generations must reconcile people on every scheduler tick")
}

func TestGenerationWorkerBackstopMaintainsPeople(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	source := newPersonWorkerSource(personDocument(8, "rev-backstop", "Synthetic Backstop"))
	backend := newPersonWorkerBackend()
	client := &personWorkerClient{}
	messages := &personWorkerMessageRunner{}
	worker := NewGenerationWorker(messages, newTestPersonWorker(source, backend, client, 8))

	result, err := worker.RunBackstop(t.Context(), 45, testEmbeddingPassScope())
	must.NoError(err)
	check.Equal([]vector.GenerationID{45}, messages.runBackstop)
	check.Equal(RunResult{Claimed: 1, Succeeded: 1}, result)
}
