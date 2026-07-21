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

type coveragePagingEngine struct {
	query.Engine
	query.Explorer

	coverage func(context.Context, query.ExploreCoverageRequest) (*query.ExploreCoverageResponse, error)
}

func (e *coveragePagingEngine) ExploreCoverage(
	ctx context.Context,
	request query.ExploreCoverageRequest,
) (*query.ExploreCoverageResponse, error) {
	return e.coverage(ctx, request)
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
		wantEmbedded   int64
		wantPercentage float64
		wantGeneration *int64
		wantActions    []SearchCoverageAction
	}{
		{name: "disabled", status: VectorStatusDisabled, wantStatus: SearchCoverageDisabled},
		{name: "initializing", status: VectorStatusInitializing, wantStatus: SearchCoverageInitializing},
		{
			name: "stale generation", status: VectorStatusReady, stale: true,
			backend:    &filteredCoverageBackend{active: generation, embeddedIDs: map[int64]struct{}{1: {}, 2: {}}},
			wantStatus: SearchCoverageStale, wantEmbedded: 2, wantPercentage: 100, wantGeneration: new(int64(7)),
		},
		{
			name: "incomplete", status: VectorStatusReady,
			backend:    &filteredCoverageBackend{active: generation, embeddedIDs: map[int64]struct{}{2: {}}},
			wantStatus: SearchCoverageIncomplete, wantEmbedded: 1, wantPercentage: 50, wantGeneration: new(int64(7)),
		},
		{
			name: "unavailable backend", status: VectorStatusReady,
			backend:    &filteredCoverageBackend{activeErr: errors.New("vector database is busy")},
			wantStatus: SearchCoverageUnavailable, wantActions: []SearchCoverageAction{SearchCoverageActionRetry},
		},
		{
			name: "ready", status: VectorStatusReady,
			backend:    &filteredCoverageBackend{active: generation, embeddedIDs: map[int64]struct{}{1: {}, 2: {}, 3: {}}},
			wantStatus: SearchCoverageReady, wantEmbedded: 2, wantPercentage: 100, wantGeneration: new(int64(7)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
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

			response := postExploreJSON(t, srv, "/api/v1/search/coverage", `{
				"filters":[{"dimension":"source","values":["1"]}]
			}`)

			require.Equal(http.StatusOK, response.Code, response.Body.String())
			var body SearchCoverageResponse
			require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(tt.wantStatus, body.Status)
			assert.Equal(int64(2), body.EligibleCount, "source filter must define coverage denominator")
			assert.Equal(tt.wantEmbedded, body.EmbeddedCount)
			assert.InDelta(tt.wantPercentage, body.Percentage, 0.001)
			assert.Equal(tt.wantGeneration, body.VectorGeneration)
			assert.NotEmpty(body.CacheRevision)
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
	require := require.New(t)
	engine := newExploreDuckDBFixture(t)
	generation := vector.Generation{ID: 9, Fingerprint: "test:2", State: vector.GenerationActive}
	backend := &filteredCoverageBackend{active: generation, embeddedIDs: map[int64]struct{}{1: {}, 2: {}, 3: {}}}
	cfg := &config.Config{Server: config.ServerConfig{APIPort: 8080}}
	cfg.Vector.Embed.Scope.MessageTypes = []string{"email"}
	srv := NewServerWithOptions(ServerOptions{
		Config: cfg, Store: &mockStore{stats: &StoreStats{}}, Engine: engine, Backend: backend,
		VectorCfg: cfg.Vector, Logger: testLogger(),
	})

	response := postExploreJSON(t, srv, "/api/v1/search/coverage", `{
		"filters":[
			{"dimension":"source","values":["2"]},
			{"dimension":"deletion","values":["active"]}
		]
	}`)

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var body SearchCoverageResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
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

func TestSearchCoverageAccumulatesBoundedExactPages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const total = 700
	pageCalls := 0
	engine := &coveragePagingEngine{coverage: func(
		_ context.Context,
		request query.ExploreCoverageRequest,
	) (*query.ExploreCoverageResponse, error) {
		pageCalls++
		assert.Equal(vector.FilteredCoverageBatchSize, request.Limit)
		start := request.AfterMessageID + 1
		if start > total {
			return &query.ExploreCoverageResponse{CacheRevision: "cache:test"}, nil
		}
		end := min(start+int64(request.Limit)-1, total)
		ids := make([]int64, 0, end-start+1)
		for id := start; id <= end; id++ {
			ids = append(ids, id)
		}
		var next *int64
		if end < total {
			next = new(end)
		}
		return &query.ExploreCoverageResponse{
			MessageIDs: ids, NextAfterMessageID: next, CacheRevision: "cache:test",
		}, nil
	}}
	backend := &filteredCoverageBackend{
		active:   vector.Generation{ID: 7, Fingerprint: "", State: vector.GenerationActive},
		countAll: true,
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  &mockStore{stats: &StoreStats{}}, Engine: engine, Backend: backend,
		VectorStatus: VectorStatusReady, Logger: testLogger(),
	})

	response := postExploreJSON(t, srv, "/api/v1/search/coverage", `{}`)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var body SearchCoverageResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(int64(total), body.EligibleCount)
	assert.Equal(int64(total), body.EmbeddedCount)
	assert.Equal(SearchCoverageReady, body.Status)
	assert.Equal(3, pageCalls)
	require.Len(backend.countCalls, 3)
	for _, ids := range backend.countCalls {
		assert.LessOrEqual(len(ids), vector.FilteredCoverageBatchSize)
	}
}

func TestSearchCoverageRejectsGenerationActivationDuringPagination(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	backend := &filteredCoverageBackend{
		active:   vector.Generation{ID: 7, Fingerprint: "model:2", State: vector.GenerationActive},
		countAll: true,
	}
	pageCalls := 0
	engine := &coveragePagingEngine{coverage: func(
		_ context.Context,
		request query.ExploreCoverageRequest,
	) (*query.ExploreCoverageResponse, error) {
		pageCalls++
		assert.Equal(vector.FilteredCoverageBatchSize, request.Limit)
		if pageCalls == 1 {
			ids := make([]int64, vector.FilteredCoverageBatchSize)
			for i := range ids {
				ids[i] = int64(i + 1)
			}
			return &query.ExploreCoverageResponse{
				MessageIDs: ids, NextAfterMessageID: new(int64(vector.FilteredCoverageBatchSize)),
				CacheRevision: "cache:test",
			}, nil
		}
		backend.active = vector.Generation{ID: 8, Fingerprint: "model:2", State: vector.GenerationActive}
		return &query.ExploreCoverageResponse{
			MessageIDs: []int64{int64(vector.FilteredCoverageBatchSize + 1)}, CacheRevision: "cache:test",
		}, nil
	}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  &mockStore{stats: &StoreStats{}}, Engine: engine, Backend: backend,
		VectorStatus: VectorStatusReady, Logger: testLogger(),
	})

	response := postExploreJSON(t, srv, "/api/v1/search/coverage", `{}`)

	assert.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	var body ErrorResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal("vector_generation_changed", body.Error)
	assert.Equal(2, pageCalls)
	require.Len(backend.countCalls, 2)
	assert.Len(backend.countCalls[0], vector.FilteredCoverageBatchSize)
}

func TestSearchCoverageRejectsCacheRevisionDriftDuringPagination(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	pageCalls := 0
	engine := &coveragePagingEngine{coverage: func(
		_ context.Context,
		_ query.ExploreCoverageRequest,
	) (*query.ExploreCoverageResponse, error) {
		pageCalls++
		if pageCalls == 1 {
			return &query.ExploreCoverageResponse{
				MessageIDs: []int64{1}, NextAfterMessageID: new(int64(1)), CacheRevision: "cache:one",
			}, nil
		}
		return &query.ExploreCoverageResponse{MessageIDs: []int64{2}, CacheRevision: "cache:two"}, nil
	}}
	backend := &filteredCoverageBackend{
		active: vector.Generation{ID: 7, State: vector.GenerationActive}, countAll: true,
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  &mockStore{stats: &StoreStats{}}, Engine: engine, Backend: backend,
		VectorStatus: VectorStatusReady, Logger: testLogger(),
	})

	response := postExploreJSON(t, srv, "/api/v1/search/coverage", `{}`)

	assert.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	var body ErrorResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal("cache_changed", body.Error)
	assert.Equal(2, pageCalls)
	assert.Len(backend.countCalls, 1)
}

func TestSearchCoverageStopsOnPagedErrorAndCancellation(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "query error", err: errors.New("coverage page failed")},
		{name: "canceled", err: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			engine := &coveragePagingEngine{coverage: func(
				_ context.Context,
				_ query.ExploreCoverageRequest,
			) (*query.ExploreCoverageResponse, error) {
				calls++
				if calls == 2 {
					return nil, tt.err
				}
				return &query.ExploreCoverageResponse{
					MessageIDs: []int64{1}, NextAfterMessageID: new(int64(1)), CacheRevision: "cache:test",
				}, nil
			}}
			backend := &filteredCoverageBackend{
				active: vector.Generation{ID: 7, State: vector.GenerationActive}, countAll: true,
			}
			srv := NewServerWithOptions(ServerOptions{
				Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
				Store:  &mockStore{stats: &StoreStats{}}, Engine: engine, Backend: backend,
				VectorStatus: VectorStatusReady, Logger: testLogger(),
			})

			response := postExploreJSON(t, srv, "/api/v1/search/coverage", `{}`)
			assert.NotEqual(t, http.StatusOK, response.Code, response.Body.String())
			assert.Equal(t, 2, calls)
			assert.Len(t, backend.countCalls, 1)
		})
	}
}

func TestSearchCoverageRetryDoesNotRequireCLIRunner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newExploreDuckDBFixture(t)
	readOnlyStore := struct{ MessageStore }{MessageStore: &mockStore{stats: &StoreStats{}}}
	backend := &filteredCoverageBackend{activeErr: errors.New("vector database is busy")}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  readOnlyStore, Engine: engine, Backend: backend,
		VectorStatus: VectorStatusReady, Logger: testLogger(),
	})

	response := postExploreJSON(t, srv, "/api/v1/search/coverage", `{}`)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var body SearchCoverageResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
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
			require := require.New(t)
			cfg := &config.Config{Server: config.ServerConfig{APIPort: 8080}, Vector: vectorCfg}
			srv := NewServerWithOptions(ServerOptions{
				Config: cfg, Store: &mockStore{stats: &StoreStats{}}, Engine: newExploreDuckDBFixture(t),
				Backend: tt.backend, VectorCfg: vectorCfg, VectorStatus: tt.status, Logger: testLogger(),
			})
			if tt.status == VectorStatusStale {
				srv.SetVectorStale("old status")
			}

			response := postExploreJSON(t, srv, "/api/v1/search/coverage", `{}`)
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			var body SearchCoverageResponse
			require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(tt.wantStatus, body.Status)
			assert.ElementsMatch(tt.wantAction, body.Actions)
		})
	}
}
