package personsearch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

type testBackend struct {
	vector.Backend
	vector.PersonBackend

	active             *vector.Generation
	building           *vector.Generation
	personHits         []vector.PersonHit
	personSearchErr    error
	personSearchGen    vector.GenerationID
	personSearchLimit  int
	personSearchLimits []int
	personSearchVector []float32
}

func (b *testBackend) ActiveGeneration(context.Context) (vector.Generation, error) {
	if b.active == nil {
		return vector.Generation{}, vector.ErrNoActiveGeneration
	}
	return *b.active, nil
}

func (b *testBackend) BuildingGeneration(context.Context) (*vector.Generation, error) {
	return b.building, nil
}

func (b *testBackend) Search(context.Context, vector.GenerationID, []float32, int, vector.Filter) ([]vector.Hit, error) {
	panic("person search must not call the message corpus")
}

func (b *testBackend) SearchPeople(
	_ context.Context, generation vector.GenerationID, queryVector []float32, limit int,
) ([]vector.PersonHit, error) {
	b.personSearchGen = generation
	b.personSearchLimit = limit
	b.personSearchLimits = append(b.personSearchLimits, limit)
	b.personSearchVector = append([]float32(nil), queryVector...)
	return b.personHits[:min(limit, len(b.personHits))], b.personSearchErr
}

type testEmbedder struct {
	calls []string
	vec   []float32
	err   error
}

type testSemanticPersonConsentSet map[string]bool

func (s testSemanticPersonConsentSet) HasActivePersonSemanticEmbeddingConsent(
	_ context.Context, fingerprint string,
) (bool, error) {
	return s[fingerprint], nil
}

func (e *testEmbedder) EmbedQuery(_ context.Context, query string) ([]float32, error) {
	e.calls = append(e.calls, query)
	return append([]float32(nil), e.vec...), e.err
}

type testStore struct {
	documents          map[int64]*store.PersonSemanticDocument
	people             map[int64]store.Person
	afterDocumentLoad  func(personID int64)
	resolvedCandidates int
	resolveErr         error
}

func (s *testStore) ResolvePersonSemanticCandidatesContext(
	_ context.Context, candidates []store.PersonSemanticCandidate,
) ([]store.Person, error) {
	if s.resolveErr != nil {
		return nil, s.resolveErr
	}
	s.resolvedCandidates += len(candidates)
	people := make([]store.Person, 0, len(candidates))
	for _, candidate := range candidates {
		document, documentOK := s.documents[candidate.PersonID]
		person, personOK := s.people[candidate.PersonID]
		if s.afterDocumentLoad != nil {
			s.afterDocumentLoad(candidate.PersonID)
		}
		if documentOK && personOK && document.Revision == candidate.Revision {
			people = append(people, person)
		}
	}
	return people, nil
}

func newTestEngine(
	backend *testBackend, data *testStore, embedder *testEmbedder,
) *Engine {
	return NewEngine(backend, data, embedder, Config{
		ExpectedFingerprint: "test:3:rperson-semantic-v1",
		Gate:                vector.SemanticPersonEmbeddingGateFunc(func(context.Context) error { return nil }),
	})
}

// TestEngineSearchGateBlocksDisabledUnconsentedAndRevokedQueryCalls catches
// query text reaching the provider without both current opt-in and consent.
func TestEngineSearchGateBlocksDisabledUnconsentedAndRevokedQueryCalls(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	backend := &testBackend{active: &vector.Generation{
		ID: 1, Fingerprint: "test:3:rperson-semantic-v1",
	}}
	embedder := &testEmbedder{vec: []float32{1, 0, 0}}
	gateErr := vector.ErrSemanticPersonEmbeddingsDisabled
	engine := NewEngine(backend, &testStore{}, embedder, Config{
		ExpectedFingerprint: "test:3:rperson-semantic-v1",
		Gate:                vector.SemanticPersonEmbeddingGateFunc(func(context.Context) error { return gateErr }),
	})

	_, err := engine.Search(t.Context(), "synthetic query", 5)
	must.ErrorIs(err, vector.ErrSemanticPersonEmbeddingsDisabled)
	check.Empty(embedder.calls)

	gateErr = vector.ErrSemanticPersonEmbeddingConsentRequired
	_, err = engine.Search(t.Context(), "synthetic query", 5)
	must.ErrorIs(err, vector.ErrSemanticPersonEmbeddingConsentRequired)
	check.Empty(embedder.calls)

	gateErr = nil
	_, err = engine.Search(t.Context(), "synthetic query", 5)
	must.NoError(err)
	must.Len(embedder.calls, 1)

	gateErr = vector.ErrSemanticPersonEmbeddingConsentRequired
	_, err = engine.Search(t.Context(), "synthetic query", 5)
	must.ErrorIs(err, vector.ErrSemanticPersonEmbeddingConsentRequired)
	check.Len(embedder.calls, 1, "revocation must apply without an engine restart")
}

// TestEngineSearchRejectsConsentThatPredatesQueryEgressDisclosure catches a
// pre-expansion grant authorizing caller free text on the real query path.
func TestEngineSearchRejectsConsentThatPredatesQueryEgressDisclosure(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	config := vector.Config{
		Enabled: true,
		Backend: "sqlite-vec",
		Embeddings: vector.EmbeddingsConfig{
			Endpoint: "https://embedding.example.test/v1", APIFormat: vector.APIFormatOpenAI,
			Model: "semantic-person-model", APIKeyEnv: "SEMANTIC_PERSON_KEY",
			Dimension: 4, BatchSize: 8,
		},
		People: vector.PeopleConfig{
			Enabled: true, RetentionPosture: "zero_data_retention", TrainingPosture: "no_training",
		},
	}
	currentProfile, err := config.SemanticPersonEmbeddingProfile()
	must.NoError(err)
	const historicalFingerprint = "f002512c98b050443ebd3c113fc18199c48ed6ef2d27d70b62328c6bbac3d250"
	must.NotEqual(historicalFingerprint, currentProfile.Fingerprint)

	consents := testSemanticPersonConsentSet{historicalFingerprint: true}
	embedder := &testEmbedder{vec: []float32{1, 0, 0}}
	engine := NewEngine(
		&testBackend{active: &vector.Generation{
			ID: 7, Fingerprint: "test:3:rperson-semantic-v1",
		}},
		&testStore{}, embedder,
		Config{
			ExpectedFingerprint: "test:3:rperson-semantic-v1",
			Gate: vector.NewExactSemanticPersonEmbeddingGate(
				func() (vector.Config, error) { return config, nil }, consents,
			),
		},
	)

	_, err = engine.Search(t.Context(), "caller supplied search text", 5)
	must.ErrorIs(err, vector.ErrSemanticPersonEmbeddingConsentRequired)
	check.Empty(embedder.calls)

	consents[currentProfile.Fingerprint] = true
	results, err := engine.Search(t.Context(), "caller supplied search text", 5)
	must.NoError(err)
	check.Empty(results)
	check.Equal([]string{"caller supplied search text"}, embedder.calls)
}

func TestEngineSearchEmbedsOnceAndHydratesPersonHitsInRankOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &testBackend{
		active: &vector.Generation{
			ID: 7, Fingerprint: "test:3:rperson-semantic-v1",
		},
		personHits: []vector.PersonHit{
			{PersonID: 22, Revision: "rev-22", Score: 0.91, Rank: 1},
			{PersonID: 11, Revision: "rev-11", Score: 0.82, Rank: 2},
		},
	}
	data := &testStore{
		documents: map[int64]*store.PersonSemanticDocument{
			22: {PersonID: 22, Revision: "rev-22"},
			11: {PersonID: 11, Revision: "rev-11"},
		},
		people: map[int64]store.Person{
			22: {ID: 22, VCardUID: "00000000-0000-4000-8000-000000000022"},
			11: {ID: 11, VCardUID: "00000000-0000-4000-8000-000000000011"},
		},
	}
	embedder := &testEmbedder{vec: []float32{1, 0, 0}}
	engine := newTestEngine(backend, data, embedder)

	results, err := engine.Search(t.Context(), "  synthetic architect  ", 8)
	require.NoError(err)
	require.Len(results, 2)
	assert.Equal([]string{"synthetic architect"}, embedder.calls)
	assert.Equal([]float32{1, 0, 0}, backend.personSearchVector)
	assert.Equal(vector.GenerationID(7), backend.personSearchGen)
	assert.Equal(8, backend.personSearchLimit)
	assert.Equal(int64(22), results[0].Person.ID)
	assert.InDelta(0.91, results[0].Score, 0.00001)
	assert.Equal(int64(11), results[1].Person.ID)
	assert.InDelta(0.82, results[1].Score, 0.00001)
}

func TestEngineSearchStopsAtLimitWithoutDuplicateResolution(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	backend := &testBackend{
		active: &vector.Generation{ID: 7, Fingerprint: "test:3:rperson-semantic-v1"},
	}
	data := &testStore{
		documents: make(map[int64]*store.PersonSemanticDocument),
		people:    make(map[int64]store.Person),
	}
	for i := int64(1); i <= 5; i++ {
		revision := fmt.Sprintf("rev-%d", i)
		backend.personHits = append(backend.personHits, vector.PersonHit{
			PersonID: i, Revision: revision, Score: 1 - float64(i)/10, Rank: int(i),
		})
		data.documents[i] = &store.PersonSemanticDocument{PersonID: i, Revision: revision}
		data.people[i] = store.Person{ID: i}
	}
	engine := newTestEngine(backend, data, &testEmbedder{vec: []float32{1, 0, 0}})

	results, err := engine.Search(t.Context(), "synthetic", 2)
	must.NoError(err)
	must.Len(results, 2)
	check.Equal([]int64{1, 2}, []int64{results[0].Person.ID, results[1].Person.ID})
	check.Equal(2, data.resolvedCandidates,
		"the first valid page must be resolved and hydrated in one snapshot")
}

func TestEngineSearchDoesNotPairOldScoreWithPostValidationRootMutation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	originalName := "Synthetic Original"
	updatedName := "Synthetic Updated"
	backend := &testBackend{
		active: &vector.Generation{
			ID: 8, Fingerprint: "test:3:rperson-semantic-v1",
		},
		personHits: []vector.PersonHit{
			{PersonID: 44, Revision: "rev-44", Score: 0.88, Rank: 1},
		},
	}
	data := &testStore{
		documents: map[int64]*store.PersonSemanticDocument{
			44: {PersonID: 44, Revision: "rev-44"},
		},
		people: map[int64]store.Person{
			44: {ID: 44, DisplayName: &originalName},
		},
	}
	data.afterDocumentLoad = func(personID int64) {
		data.people[personID] = store.Person{ID: personID, DisplayName: &updatedName}
	}
	engine := newTestEngine(backend, data, &testEmbedder{vec: []float32{1, 0, 0}})

	results, err := engine.Search(t.Context(), "synthetic query", 5)
	require.NoError(err)
	require.Len(results, 1)
	require.NotNil(results[0].Person.DisplayName)
	assert.Equal(originalName, *results[0].Person.DisplayName,
		"the root must come from the same snapshot that validated the old score")
}

func TestEngineSearchDropsStaleDeletedAndPostValidationDeletedHits(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &testBackend{
		active: &vector.Generation{
			ID: 9, Fingerprint: "test:3:rperson-semantic-v1",
		},
		personHits: []vector.PersonHit{
			{PersonID: 10, Revision: "old-revision", Score: 0.99, Rank: 1},
			{PersonID: 20, Revision: "deleted-revision", Score: 0.90, Rank: 2},
			{PersonID: 30, Revision: "rev-30", Score: 0.80, Rank: 3},
			{PersonID: 40, Revision: "rev-40", Score: 0.70, Rank: 4},
		},
	}
	data := &testStore{
		documents: map[int64]*store.PersonSemanticDocument{
			10: {PersonID: 10, Revision: "current-revision"},
			30: {PersonID: 30, Revision: "rev-30"},
			40: {PersonID: 40, Revision: "rev-40"},
		},
		people: map[int64]store.Person{
			30: {ID: 30, VCardUID: "00000000-0000-4000-8000-000000000030"},
			// Person 40 disappears after document validation and before root
			// hydration. It must not leak as a zero-value result.
		},
	}
	engine := newTestEngine(backend, data, &testEmbedder{vec: []float32{1, 0, 0}})

	results, err := engine.Search(t.Context(), "synthetic query", 10)
	require.NoError(err)
	require.Len(results, 1)
	assert.Equal(int64(30), results[0].Person.ID)
	assert.InDelta(0.80, results[0].Score, 0.00001)
}

// TestEngineSearchWidensPastInvalidTopHits catches a fenced top result
// underfilling a page even though the person corpus has a valid lower-ranked
// replacement. Widening must reuse the one query embedding.
func TestEngineSearchWidensPastInvalidTopHits(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	backend := &testBackend{
		active: &vector.Generation{
			ID: 10, Fingerprint: "test:3:rperson-semantic-v1",
		},
		personHits: []vector.PersonHit{
			{PersonID: 10, Revision: "stale-revision", Score: 0.99, Rank: 1},
			{PersonID: 20, Revision: "rev-20", Score: 0.90, Rank: 2},
		},
	}
	data := &testStore{
		documents: map[int64]*store.PersonSemanticDocument{
			10: {PersonID: 10, Revision: "current-revision"},
			20: {PersonID: 20, Revision: "rev-20"},
		},
		people: map[int64]store.Person{
			20: {ID: 20, VCardUID: "00000000-0000-4000-8000-000000000020"},
		},
	}
	embedder := &testEmbedder{vec: []float32{1, 0, 0}}
	engine := newTestEngine(backend, data, embedder)

	results, err := engine.Search(t.Context(), "synthetic query", 1)
	must.NoError(err)
	must.Len(results, 1)
	check.Equal(int64(20), results[0].Person.ID)
	check.InDelta(0.90, results[0].Score, 0.00001)
	check.Equal([]int{1, 2}, backend.personSearchLimits)
	check.Equal([]string{"synthetic query"}, embedder.calls)
}

// TestEngineSearchContinuesAfterWidenedRevalidationUnderfills catches the
// widening loop returning early or losing still-current candidates when a
// previously valid higher-ranked person changes before final revalidation.
func TestEngineSearchContinuesAfterWidenedRevalidationUnderfills(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	backend := &testBackend{
		active: &vector.Generation{ID: 16, Fingerprint: "test:3:rperson-semantic-v1"},
		personHits: []vector.PersonHit{
			{PersonID: 1, Revision: "rev-1", Score: 0.99, Rank: 1},
			{PersonID: 2, Revision: "stale-2", Score: 0.92, Rank: 2},
			{PersonID: 3, Revision: "rev-3", Score: 0.85, Rank: 3},
			{PersonID: 4, Revision: "stale-4", Score: 0.78, Rank: 4},
			{PersonID: 5, Revision: "rev-5", Score: 0.71, Rank: 5},
			{PersonID: 6, Revision: "stale-6", Score: 0.64, Rank: 6},
			{PersonID: 7, Revision: "stale-7", Score: 0.57, Rank: 7},
			{PersonID: 8, Revision: "stale-8", Score: 0.50, Rank: 8},
		},
	}
	data := &testStore{
		documents: map[int64]*store.PersonSemanticDocument{
			1: {PersonID: 1, Revision: "rev-1"},
			3: {PersonID: 3, Revision: "rev-3"},
			5: {PersonID: 5, Revision: "rev-5"},
		},
		people: map[int64]store.Person{
			1: {ID: 1},
			3: {ID: 3},
			5: {ID: 5},
		},
	}
	data.afterDocumentLoad = func(personID int64) {
		if personID == 3 {
			delete(data.documents, 1)
			delete(data.people, 1)
		}
	}
	engine := newTestEngine(backend, data, &testEmbedder{vec: []float32{1, 0, 0}})

	results, err := engine.Search(t.Context(), "synthetic", 2)
	must.NoError(err)
	must.Len(results, 2)
	check.Equal([]int64{3, 5}, []int64{results[0].Person.ID, results[1].Person.ID})
	check.Equal([]int{2, 4, 8}, backend.personSearchLimits,
		"revalidation underfill must widen finitely and stop once the page refills")
}

func TestEngineSearchDoesNotRevalidateRejectedPrefixes(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	backend := &testBackend{
		active: &vector.Generation{ID: 13, Fingerprint: "test:3:rperson-semantic-v1"},
	}
	data := &testStore{documents: make(map[int64]*store.PersonSemanticDocument), people: make(map[int64]store.Person)}
	for i := 1; i <= 8; i++ {
		revision := "stale"
		if i > 6 {
			revision = "current"
			data.documents[int64(i)] = &store.PersonSemanticDocument{PersonID: int64(i), Revision: revision}
			data.people[int64(i)] = store.Person{ID: int64(i)}
		}
		backend.personHits = append(backend.personHits, vector.PersonHit{
			PersonID: int64(i), Revision: revision, Score: 1 - float64(i)/10, Rank: i,
		})
	}
	engine := newTestEngine(backend, data, &testEmbedder{vec: []float32{1, 0, 0}})

	results, err := engine.Search(t.Context(), "synthetic", 2)
	must.NoError(err)
	must.Len(results, 2)
	check.Equal([]int64{7, 8}, []int64{results[0].Person.ID, results[1].Person.ID})
	check.LessOrEqual(data.resolvedCandidates, 10,
		"each rejected candidate should be rendered only once across widening rounds")
}

func TestEngineSearchUsesPersonCorpusWithoutMessageFallback(t *testing.T) {
	backend := &testBackend{
		active: &vector.Generation{
			ID: 11, Fingerprint: "test:3:rperson-semantic-v1",
		},
		personHits: nil,
	}
	engine := newTestEngine(backend, &testStore{}, &testEmbedder{vec: []float32{1, 0, 0}})

	results, err := engine.Search(t.Context(), "no person match", 5)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestEngineSearchRevalidatesMutablePersonCoverageBeforeProviderCall(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	backend := &testBackend{active: &vector.Generation{
		ID: 17, Fingerprint: "test:3:rperson-semantic-v1",
	}}
	embedder := &testEmbedder{vec: []float32{1, 0, 0}}
	checkedGeneration := vector.GenerationID(0)
	coverageReady := false
	coverageChecks := 0
	engine := NewEngine(backend, &testStore{}, embedder, Config{
		ExpectedFingerprint: "test:3:rperson-semantic-v1",
		Gate:                vector.SemanticPersonEmbeddingGateFunc(func(context.Context) error { return nil }),
		PersonCoverage: func(_ context.Context, generation vector.GenerationID) (Coverage, error) {
			coverageChecks++
			checkedGeneration = generation
			if coverageReady {
				return Coverage{}, nil
			}
			return Coverage{Mismatched: 1}, nil
		},
	})

	results, err := engine.Search(t.Context(), "synthetic query", 5)
	must.ErrorIs(err, ErrPersonCoverageIncomplete)
	must.ErrorIs(err, vector.ErrIndexBuilding)
	check.Nil(results)
	check.Equal(vector.GenerationID(17), checkedGeneration)
	check.Empty(embedder.calls, "an incomplete corpus must not incur a provider call")
	check.Empty(backend.personSearchLimits)

	coverageReady = true
	results, err = engine.Search(t.Context(), "synthetic query", 5)
	must.NoError(err)
	check.Empty(results)
	coverageReady = false
	results, err = engine.Search(t.Context(), "synthetic query", 5)
	must.ErrorIs(err, ErrPersonCoverageIncomplete)
	check.Nil(results)
	check.Equal(3, coverageChecks,
		"a successful check must not hide later corpus mutations or terminal rejections")
	check.Equal([]string{"synthetic query"}, embedder.calls)

	backend.active = &vector.Generation{ID: 18, Fingerprint: "test:3:rperson-semantic-v1"}
	coverageReady = false
	results, err = engine.Search(t.Context(), "synthetic query", 5)
	must.ErrorIs(err, ErrPersonCoverageIncomplete)
	check.Nil(results)
	check.Equal(4, coverageChecks, "a newly active generation needs its own coverage proof")
	check.Equal([]string{"synthetic query"}, embedder.calls)
}

func TestEngineSearchPropagatesPersonCoverageError(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	wantErr := errors.New("person coverage unavailable")
	embedder := &testEmbedder{vec: []float32{1, 0, 0}}
	engine := NewEngine(
		&testBackend{active: &vector.Generation{
			ID: 19, Fingerprint: "test:3:rperson-semantic-v1",
		}},
		&testStore{}, embedder,
		Config{
			ExpectedFingerprint: "test:3:rperson-semantic-v1",
			Gate:                vector.SemanticPersonEmbeddingGateFunc(func(context.Context) error { return nil }),
			PersonCoverage: func(context.Context, vector.GenerationID) (Coverage, error) {
				return Coverage{}, wantErr
			},
		},
	)

	results, err := engine.Search(t.Context(), "synthetic query", 5)
	must.ErrorIs(err, wantErr)
	must.ErrorContains(err, "check person corpus coverage")
	check.Nil(results)
	check.Empty(embedder.calls)
}

func TestEngineSearchReusesVectorGenerationErrors(t *testing.T) {
	tests := []struct {
		name    string
		backend *testBackend
		wantErr error
	}{
		{
			name:    "disabled",
			backend: &testBackend{},
			wantErr: vector.ErrNotEnabled,
		},
		{
			name:    "building",
			backend: &testBackend{building: &vector.Generation{ID: 2}},
			wantErr: vector.ErrIndexBuilding,
		},
		{
			name:    "stale",
			backend: &testBackend{active: &vector.Generation{ID: 3, Fingerprint: "old:3"}},
			wantErr: vector.ErrIndexStale,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := newTestEngine(test.backend, &testStore{}, &testEmbedder{vec: []float32{1, 0, 0}})
			_, err := engine.Search(t.Context(), "synthetic query", 5)
			require.ErrorIs(t, err, test.wantErr)
		})
	}
}

func TestEngineSearchMapsEmbeddingDeadline(t *testing.T) {
	backend := &testBackend{active: &vector.Generation{
		ID: 12, Fingerprint: "test:3:rperson-semantic-v1",
	}}
	engine := newTestEngine(backend, &testStore{}, &testEmbedder{err: context.DeadlineExceeded})

	_, err := engine.Search(t.Context(), "synthetic query", 5)
	require.Error(t, err)
	assert.ErrorIs(t, err, vector.ErrEmbeddingTimeout)
}

func TestEngineSearchPropagatesPersonBackendError(t *testing.T) {
	wantErr := errors.New("person backend unavailable")
	backend := &testBackend{
		active:          &vector.Generation{ID: 14, Fingerprint: "test:3:rperson-semantic-v1"},
		personSearchErr: wantErr,
	}
	engine := newTestEngine(backend, &testStore{}, &testEmbedder{vec: []float32{1, 0, 0}})

	results, err := engine.Search(t.Context(), "synthetic query", 5)
	require.ErrorIs(t, err, wantErr)
	require.ErrorContains(t, err, "search person corpus")
	assert.Nil(t, results)
}

func TestEngineSearchPropagatesPersonStoreError(t *testing.T) {
	wantErr := errors.New("person store unavailable")
	backend := &testBackend{
		active: &vector.Generation{ID: 15, Fingerprint: "test:3:rperson-semantic-v1"},
		personHits: []vector.PersonHit{{
			PersonID: 1, Revision: "rev-1", Score: 0.9, Rank: 1,
		}},
	}
	engine := newTestEngine(backend, &testStore{resolveErr: wantErr}, &testEmbedder{vec: []float32{1, 0, 0}})

	results, err := engine.Search(t.Context(), "synthetic query", 5)
	require.ErrorIs(t, err, wantErr)
	require.ErrorContains(t, err, "resolve final person search hits")
	assert.Nil(t, results)
}
