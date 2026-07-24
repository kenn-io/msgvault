package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/vector"
)

type filteredCoverageBackend struct {
	vector.Backend

	active      vector.Generation
	activeErr   error
	building    *vector.Generation
	buildingErr error
	embeddedIDs map[int64]struct{}
	countAll    bool
	countErr    error
	countCalls  [][]int64
	mu          sync.Mutex
}

func (b *filteredCoverageBackend) BuildingGeneration(context.Context) (*vector.Generation, error) {
	return b.building, b.buildingErr
}

func (b *filteredCoverageBackend) ActiveGeneration(context.Context) (vector.Generation, error) {
	if b.activeErr != nil {
		return vector.Generation{}, b.activeErr
	}
	return b.active, nil
}

func (b *filteredCoverageBackend) EmbeddedMessageCountForIDs(
	_ context.Context,
	_ vector.GenerationID,
	ids []int64,
) (int64, error) {
	b.mu.Lock()
	b.countCalls = append(b.countCalls, append([]int64(nil), ids...))
	b.mu.Unlock()
	if b.countErr != nil {
		return 0, b.countErr
	}
	if b.countAll {
		return int64(len(ids)), nil
	}
	var count int64
	for _, id := range ids {
		if _, ok := b.embeddedIDs[id]; ok {
			count++
		}
	}
	return count, nil
}

// coverageScanEngine fakes the analytical query layer behind coverage: a
// cheap revision probe plus one streamed coverage scan over `total`
// sequential message IDs. Both call counters let tests assert that the
// handler issues zero query-layer calls (no usable generation) or exactly
// one scan (cache hits).
type coverageScanEngine struct {
	query.Engine
	query.Explorer

	revision   string
	total      int
	scan       func(context.Context, query.ExploreCoverageRequest, func([]int64) error) (*query.ExploreCoverageResult, error)
	probeCalls int
	scanCalls  int
}

func (e *coverageScanEngine) ExploreSelectionStats(
	context.Context,
	query.ExploreSelectionRequest,
) (*query.ExploreSelectionStats, error) {
	e.probeCalls++
	return &query.ExploreSelectionStats{CacheRevision: e.revision}, nil
}

func (e *coverageScanEngine) ExploreCoverage(
	ctx context.Context,
	request query.ExploreCoverageRequest,
	visit func([]int64) error,
) (*query.ExploreCoverageResult, error) {
	e.scanCalls++
	if e.scan != nil {
		return e.scan(ctx, request, visit)
	}
	result := &query.ExploreCoverageResult{CacheRevision: e.revision}
	batch := make([]int64, 0, request.BatchSize)
	for id := int64(1); id <= int64(e.total); id++ {
		result.EligibleCount++
		batch = append(batch, id)
		if len(batch) == request.BatchSize {
			if err := visit(batch); err != nil {
				return nil, err
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := visit(batch); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func newCoverageScanServer(t *testing.T, engine query.Engine, backend vector.Backend) *Server {
	t.Helper()
	return NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  &mockStore{stats: &StoreStats{}}, Engine: engine, Backend: backend,
		VectorStatus: VectorStatusReady, Logger: testLogger(),
	})
}

func getSearchCoverage(t *testing.T, srv *Server, body string) SearchCoverageResponse {
	t.Helper()
	response := postExploreJSON(t, srv, "/api/v1/search/coverage", body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var decoded SearchCoverageResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &decoded))
	return decoded
}

func TestSearchCoverageReportsEveryNamedStateInFilteredContext(t *testing.T) {
	generation := vector.Generation{
		ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive,
	}
	tests := []struct {
		name           string
		status         VectorStatus
		backend        *filteredCoverageBackend
		stale          bool
		wantStatus     SearchCoverageStatus
		wantEligible   int64
		wantEmbedded   int64
		wantPercentage float64
		wantGeneration *int64
		wantRevision   bool
		wantActions    []SearchCoverageAction
	}{
		{name: "disabled", status: VectorStatusDisabled, wantStatus: SearchCoverageDisabled},
		{name: "initializing", status: VectorStatusInitializing, wantStatus: SearchCoverageInitializing},
		{
			name: "stale generation", status: VectorStatusReady, stale: true,
			backend:      &filteredCoverageBackend{active: generation, embeddedIDs: map[int64]struct{}{1: {}, 2: {}}},
			wantStatus:   SearchCoverageStale,
			wantEligible: 2, wantEmbedded: 2, wantPercentage: 100,
			wantGeneration: new(int64(7)), wantRevision: true,
		},
		{
			name: "incomplete", status: VectorStatusReady,
			backend:      &filteredCoverageBackend{active: generation, embeddedIDs: map[int64]struct{}{2: {}}},
			wantStatus:   SearchCoverageIncomplete,
			wantEligible: 2, wantEmbedded: 1, wantPercentage: 50,
			wantGeneration: new(int64(7)), wantRevision: true,
		},
		{
			name: "unavailable backend", status: VectorStatusReady,
			backend:    &filteredCoverageBackend{activeErr: errors.New("vector database is busy")},
			wantStatus: SearchCoverageUnavailable, wantActions: []SearchCoverageAction{SearchCoverageActionRetry},
		},
		{
			name: "ready", status: VectorStatusReady,
			backend:      &filteredCoverageBackend{active: generation, embeddedIDs: map[int64]struct{}{1: {}, 2: {}, 3: {}}},
			wantStatus:   SearchCoverageReady,
			wantEligible: 2, wantEmbedded: 2, wantPercentage: 100,
			wantGeneration: new(int64(7)), wantRevision: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			var backend vector.Backend
			if tt.backend != nil {
				backend = tt.backend
			}
			engine := newExploreDuckDBFixture(t)
			srv := NewServerWithOptions(ServerOptions{
				Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
				Store:  &mockStore{stats: &StoreStats{}}, Engine: engine, Backend: backend,
				VectorStatus: tt.status, Logger: testLogger(),
			})
			if tt.stale {
				srv.SetVectorStale("configured generation changed")
			}

			body := getSearchCoverage(t, srv, `{
				"filters":[{"dimension":"source","values":["1"]}]
			}`)

			assert.Equal(tt.wantStatus, body.Status)
			assert.Equal(tt.wantEligible, body.EligibleCount)
			assert.Equal(tt.wantEmbedded, body.EmbeddedCount)
			assert.InDelta(tt.wantPercentage, body.Percentage, 0.001)
			assert.Equal(tt.wantGeneration, body.VectorGeneration)
			if tt.wantRevision {
				assert.NotEmpty(body.CacheRevision)
			} else {
				assert.Empty(body.CacheRevision, "skipped scans must not publish a cache revision")
			}
			assert.ElementsMatch(tt.wantActions, body.Actions)
		})
	}
}

func TestSameCoverageGenerationRejectsInPlaceTopUp(t *testing.T) {
	want := vector.Generation{
		ID: 7, State: vector.GenerationActive, Fingerprint: "test:2", MessageCount: 10,
	}
	got := want
	got.MessageCount++

	assert.False(t, sameCoverageGeneration(SearchCoverageReady, want, SearchCoverageReady, &got))
}

func TestSearchCoverageIntersectsCanonicalContextWithEmbeddingScope(t *testing.T) {
	assert := assert.New(t)
	engine := newExploreDuckDBFixture(t)
	generation := vector.Generation{ID: 9, Fingerprint: "test:2", State: vector.GenerationActive}
	backend := &filteredCoverageBackend{active: generation, embeddedIDs: map[int64]struct{}{1: {}, 2: {}, 3: {}}}
	cfg := &config.Config{Server: config.ServerConfig{APIPort: 8080}}
	cfg.Vector.Embed.Scope.MessageTypes = []string{"email"}
	srv := NewServerWithOptions(ServerOptions{
		Config: cfg, Store: &mockStore{stats: &StoreStats{}}, Engine: engine, Backend: backend,
		VectorCfg: cfg.Vector, Logger: testLogger(),
	})

	body := getSearchCoverage(t, srv, `{
		"filters":[
			{"dimension":"source","values":["2"]},
			{"dimension":"deletion","values":["active"]}
		]
	}`)

	assert.Equal(int64(1), body.EligibleCount)
	assert.Equal(int64(1), body.EmbeddedCount)
	assert.Equal(SearchCoverageReady, body.Status)
}

func TestSearchCoverageOpenAPIContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	doc := OpenAPIDocument()
	op := doc.Paths["/api/v1/search/coverage"].Post
	require.NotNil(op)
	assert.Equal("getSearchCoverage", op.OperationID)
	for _, status := range []string{"200", "400", "503"} {
		assert.Contains(op.Responses, status)
	}
	schema := doc.Components.Schemas.Map()["SearchCoverageResponse"]
	require.NotNil(schema)
	assert.ElementsMatch(
		[]any{"disabled", "initializing", "stale", "incomplete", "unavailable", "ready"},
		schema.Properties["status"].Enum,
	)
	assert.False(schema.Properties["actions"].Nullable)
	assert.ElementsMatch([]any{"retry", "build_index"}, schema.Properties["actions"].Items.Enum)
}

func TestSearchCoverageComputesCountsInOneScanWithBoundedBatches(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const total = 700
	engine := &coverageScanEngine{revision: "cache:test", total: total}
	backend := &filteredCoverageBackend{
		active:   vector.Generation{ID: 7, Fingerprint: "", State: vector.GenerationActive},
		countAll: true,
	}
	srv := newCoverageScanServer(t, engine, backend)

	body := getSearchCoverage(t, srv, `{}`)
	assert.Equal(int64(total), body.EligibleCount)
	assert.Equal(int64(total), body.EmbeddedCount)
	assert.Equal(SearchCoverageReady, body.Status)
	assert.Equal("cache:test", body.CacheRevision)
	assert.Equal(1, engine.scanCalls, "coverage must resolve the population in one streamed scan")
	require.Len(backend.countCalls, 3)
	for _, ids := range backend.countCalls {
		assert.LessOrEqual(len(ids), vector.FilteredCoverageBatchSize)
	}
}

func TestSearchCoverageSkipsArchiveScanWithoutUsableGeneration(t *testing.T) {
	vectorCfg := vector.Config{
		Enabled: true,
		Embeddings: vector.EmbeddingsConfig{
			Model: "test-model", Dimension: 2, MaxInputChars: 1000,
		},
	}
	fingerprint := vectorCfg.GenerationFingerprint()
	tests := []struct {
		name       string
		status     VectorStatus
		backend    *filteredCoverageBackend
		wantStatus SearchCoverageStatus
	}{
		{name: "disabled", status: VectorStatusDisabled, wantStatus: SearchCoverageDisabled},
		{
			name: "first build in progress", status: VectorStatusInitializing,
			backend: &filteredCoverageBackend{
				activeErr: vector.ErrNoActiveGeneration,
				building:  &vector.Generation{ID: 8, Fingerprint: fingerprint, State: vector.GenerationBuilding},
			},
			wantStatus: SearchCoverageInitializing,
		},
		{
			name: "backend unavailable", status: VectorStatusReady,
			backend:    &filteredCoverageBackend{activeErr: errors.New("vector database is busy")},
			wantStatus: SearchCoverageUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			engine := &coverageScanEngine{revision: "cache:test", total: 700}
			var backend vector.Backend
			if tt.backend != nil {
				backend = tt.backend
			}
			srv := NewServerWithOptions(ServerOptions{
				Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
				Store:  &mockStore{stats: &StoreStats{}}, Engine: engine, Backend: backend,
				VectorStatus: tt.status, Logger: testLogger(),
			})

			body := getSearchCoverage(t, srv, `{}`)

			assert.Equal(tt.wantStatus, body.Status)
			assert.Zero(body.EligibleCount)
			assert.Zero(body.EmbeddedCount)
			assert.Empty(body.CacheRevision)
			assert.Zero(engine.probeCalls, "no usable generation must not probe the analytical cache")
			assert.Zero(engine.scanCalls, "no usable generation must not scan the archive")
		})
	}
}

func TestSearchCoverageServesCachedCountsUntilRevisionOrGenerationChanges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	engine := &coverageScanEngine{revision: "cache:one", total: 700}
	backend := &filteredCoverageBackend{
		active:   vector.Generation{ID: 7, Fingerprint: "", State: vector.GenerationActive},
		countAll: true,
	}
	srv := newCoverageScanServer(t, engine, backend)

	first := getSearchCoverage(t, srv, `{}`)
	require.Equal(int64(700), first.EligibleCount)
	require.Equal(1, engine.scanCalls)
	require.Len(backend.countCalls, 3)

	cached := getSearchCoverage(t, srv, `{}`)
	assert.Equal(first, cached, "an unchanged revision and generation must serve the cached counts")
	assert.Equal(1, engine.scanCalls, "a cache hit must not rescan the archive")
	assert.Len(backend.countCalls, 3, "a cache hit must not re-intersect the vector index")

	engine.revision = "cache:two"
	recomputed := getSearchCoverage(t, srv, `{}`)
	assert.Equal("cache:two", recomputed.CacheRevision)
	assert.Equal(2, engine.scanCalls, "a cache revision change must recompute coverage")

	backend.active.ID = 8
	regenerated := getSearchCoverage(t, srv, `{}`)
	require.NotNil(regenerated.VectorGeneration)
	assert.Equal(int64(8), *regenerated.VectorGeneration)
	assert.Equal(3, engine.scanCalls, "a vector generation change must recompute coverage")
}

func TestSearchCoverageCachesPerFilterContext(t *testing.T) {
	assert := assert.New(t)
	engine := newExploreDuckDBFixture(t)
	backend := &filteredCoverageBackend{
		active:   vector.Generation{ID: 7, Fingerprint: "", State: vector.GenerationActive},
		countAll: true,
	}
	srv := newCoverageScanServer(t, engine, backend)

	unfiltered := getSearchCoverage(t, srv, `{}`)
	filtered := getSearchCoverage(t, srv, `{"filters":[{"dimension":"source","values":["1"]}]}`)

	assert.Equal(int64(3), unfiltered.EligibleCount)
	assert.Equal(int64(2), filtered.EligibleCount, "a different filter context must not reuse cached counts")
}

func TestSearchCoverageRejectsGenerationActivationDuringScan(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	backend := &filteredCoverageBackend{
		active:   vector.Generation{ID: 7, Fingerprint: "model:2", State: vector.GenerationActive},
		countAll: true,
	}
	engine := &coverageScanEngine{revision: "cache:test"}
	engine.scan = func(
		_ context.Context,
		request query.ExploreCoverageRequest,
		visit func([]int64) error,
	) (*query.ExploreCoverageResult, error) {
		assert.Equal(vector.FilteredCoverageBatchSize, request.BatchSize)
		ids := make([]int64, vector.FilteredCoverageBatchSize)
		for i := range ids {
			ids[i] = int64(i + 1)
		}
		if err := visit(ids); err != nil {
			return nil, err
		}
		backend.active = vector.Generation{ID: 8, Fingerprint: "model:2", State: vector.GenerationActive}
		if err := visit([]int64{int64(vector.FilteredCoverageBatchSize + 1)}); err != nil {
			return nil, err
		}
		return &query.ExploreCoverageResult{
			EligibleCount: int64(vector.FilteredCoverageBatchSize + 1), CacheRevision: "cache:test",
		}, nil
	}
	srv := newCoverageScanServer(t, engine, backend)

	response := postExploreJSON(t, srv, "/api/v1/search/coverage", `{}`)

	assert.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	var body ErrorResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal("vector_generation_changed", body.Error)
	require.Len(backend.countCalls, 2)
	assert.Len(backend.countCalls[0], vector.FilteredCoverageBatchSize)
}

func TestSearchCoverageStopsOnScanErrorAndCancellation(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "query error", err: errors.New("coverage scan failed")},
		{name: "canceled", err: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &filteredCoverageBackend{
				active: vector.Generation{ID: 7, State: vector.GenerationActive}, countAll: true,
			}
			engine := &coverageScanEngine{revision: "cache:test"}
			engine.scan = func(
				_ context.Context,
				_ query.ExploreCoverageRequest,
				visit func([]int64) error,
			) (*query.ExploreCoverageResult, error) {
				if err := visit([]int64{1}); err != nil {
					return nil, err
				}
				return nil, tt.err
			}
			srv := newCoverageScanServer(t, engine, backend)

			response := postExploreJSON(t, srv, "/api/v1/search/coverage", `{}`)
			assert.NotEqual(t, http.StatusOK, response.Code, response.Body.String())
			assert.Equal(t, 1, engine.scanCalls)
			assert.Len(t, backend.countCalls, 1)
		})
	}
}

func TestSearchCoverageReportsUnavailableWhenIntersectionFails(t *testing.T) {
	assert := assert.New(t)
	engine := &coverageScanEngine{revision: "cache:test", total: 700}
	backend := &filteredCoverageBackend{
		active:   vector.Generation{ID: 7, State: vector.GenerationActive},
		countErr: errors.New("vectors.db is busy"),
	}
	srv := newCoverageScanServer(t, engine, backend)

	body := getSearchCoverage(t, srv, `{}`)

	assert.Equal(SearchCoverageUnavailable, body.Status)
	assert.Zero(body.EligibleCount)
	assert.Zero(body.EmbeddedCount)
	assert.Equal([]SearchCoverageAction{SearchCoverageActionRetry}, body.Actions)
	assert.Len(backend.countCalls, 1, "a failed intersection must abort the scan")
}

func TestSearchCoverageRetryDoesNotRequireCLIRunner(t *testing.T) {
	assert := assert.New(t)
	engine := newExploreDuckDBFixture(t)
	readOnlyStore := struct{ MessageStore }{MessageStore: &mockStore{stats: &StoreStats{}}}
	backend := &filteredCoverageBackend{activeErr: errors.New("vector database is busy")}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  readOnlyStore, Engine: engine, Backend: backend,
		VectorStatus: VectorStatusReady, Logger: testLogger(),
	})

	body := getSearchCoverage(t, srv, `{}`)
	assert.Equal(SearchCoverageUnavailable, body.Status)
	assert.Equal([]SearchCoverageAction{SearchCoverageActionRetry}, body.Actions)
}

func TestSearchCoverageResolvesGenerationStateAndRefreshesStale(t *testing.T) {
	vectorCfg := vector.Config{
		Enabled: true,
		Embeddings: vector.EmbeddingsConfig{
			Model: "test-model", Dimension: 2, MaxInputChars: 1000,
		},
	}
	fingerprint := vectorCfg.GenerationFingerprint()
	matching := vector.Generation{ID: 7, Fingerprint: fingerprint, State: vector.GenerationActive}
	tests := []struct {
		name       string
		status     VectorStatus
		backend    *filteredCoverageBackend
		wantStatus SearchCoverageStatus
		wantAction []SearchCoverageAction
	}{
		{
			name: "matching first build", status: VectorStatusReady,
			backend: &filteredCoverageBackend{
				activeErr: vector.ErrNoActiveGeneration,
				building:  &vector.Generation{ID: 8, Fingerprint: fingerprint, State: vector.GenerationBuilding},
			},
			wantStatus: SearchCoverageInitializing,
		},
		{
			name: "no generation", status: VectorStatusReady,
			backend: &filteredCoverageBackend{
				activeErr: vector.ErrNoActiveGeneration,
			},
			wantStatus: SearchCoverageUnavailable,
			wantAction: []SearchCoverageAction{SearchCoverageActionRetry, SearchCoverageActionBuildIndex},
		},
		{
			name: "different first build", status: VectorStatusReady,
			backend: &filteredCoverageBackend{
				activeErr: vector.ErrNoActiveGeneration,
				building:  &vector.Generation{ID: 8, Fingerprint: "different", State: vector.GenerationBuilding},
			},
			wantStatus: SearchCoverageUnavailable,
			wantAction: []SearchCoverageAction{SearchCoverageActionRetry},
		},
		{
			name: "latched stale refreshed", status: VectorStatusStale,
			backend: &filteredCoverageBackend{
				active: matching, embeddedIDs: map[int64]struct{}{1: {}, 2: {}, 3: {}},
			},
			wantStatus: SearchCoverageReady,
		},
		{
			name: "stale active without building generation", status: VectorStatusReady,
			backend: &filteredCoverageBackend{
				active:      vector.Generation{ID: 9, Fingerprint: "different", State: vector.GenerationActive},
				embeddedIDs: map[int64]struct{}{1: {}, 2: {}, 3: {}},
			},
			wantStatus: SearchCoverageStale,
			wantAction: []SearchCoverageAction{SearchCoverageActionBuildIndex},
		},
		{
			name: "stale active with matching building generation", status: VectorStatusReady,
			backend: &filteredCoverageBackend{
				active:      vector.Generation{ID: 9, Fingerprint: "different", State: vector.GenerationActive},
				building:    &vector.Generation{ID: 10, Fingerprint: fingerprint, State: vector.GenerationBuilding},
				embeddedIDs: map[int64]struct{}{1: {}, 2: {}, 3: {}},
			},
			wantStatus: SearchCoverageStale,
			wantAction: []SearchCoverageAction{SearchCoverageActionBuildIndex},
		},
		{
			name: "stale active with mismatched building generation", status: VectorStatusReady,
			backend: &filteredCoverageBackend{
				active:      vector.Generation{ID: 9, Fingerprint: "different", State: vector.GenerationActive},
				building:    &vector.Generation{ID: 10, Fingerprint: "other", State: vector.GenerationBuilding},
				embeddedIDs: map[int64]struct{}{1: {}, 2: {}, 3: {}},
			},
			wantStatus: SearchCoverageStale,
			wantAction: []SearchCoverageAction{SearchCoverageActionRetry},
		},
		{
			name: "stale active with building lookup failure", status: VectorStatusReady,
			backend: &filteredCoverageBackend{
				active:      vector.Generation{ID: 9, Fingerprint: "different", State: vector.GenerationActive},
				buildingErr: errors.New("building generation unavailable"),
				embeddedIDs: map[int64]struct{}{1: {}, 2: {}, 3: {}},
			},
			wantStatus: SearchCoverageStale,
			wantAction: []SearchCoverageAction{SearchCoverageActionRetry},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			cfg := &config.Config{Server: config.ServerConfig{APIPort: 8080}, Vector: vectorCfg}
			srv := NewServerWithOptions(ServerOptions{
				Config: cfg, Store: &mockStore{stats: &StoreStats{}}, Engine: newExploreDuckDBFixture(t),
				Backend: tt.backend, VectorCfg: vectorCfg, VectorStatus: tt.status, Logger: testLogger(),
			})
			if tt.status == VectorStatusStale {
				srv.SetVectorStale("old status")
			}

			body := getSearchCoverage(t, srv, `{}`)
			assert.Equal(tt.wantStatus, body.Status)
			assert.ElementsMatch(tt.wantAction, body.Actions)
		})
	}
}
