package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/vector"
)

type SearchCoverageStatus string

const (
	SearchCoverageDisabled     SearchCoverageStatus = "disabled"
	SearchCoverageInitializing SearchCoverageStatus = "initializing"
	SearchCoverageStale        SearchCoverageStatus = "stale"
	SearchCoverageIncomplete   SearchCoverageStatus = "incomplete"
	SearchCoverageUnavailable  SearchCoverageStatus = "unavailable"
	SearchCoverageReady        SearchCoverageStatus = "ready"
)

type SearchCoverageAction string

const (
	SearchCoverageActionRetry      SearchCoverageAction = "retry"
	SearchCoverageActionBuildIndex SearchCoverageAction = "build_index"
)

type SearchCoverageRequest struct {
	Filters []ExploreFilter `json:"filters,omitempty"`
}

type SearchCoverageResponse struct {
	EligibleCount     int64                  `json:"eligible_count"`
	EmbeddedCount     int64                  `json:"embedded_count"`
	Percentage        float64                `json:"percentage"`
	VectorGeneration  *int64                 `json:"vector_generation,omitempty"`
	VectorFingerprint string                 `json:"vector_fingerprint,omitempty"`
	CacheRevision     string                 `json:"cache_revision"`
	Status            SearchCoverageStatus   `json:"status" enum:"disabled,initializing,stale,incomplete,unavailable,ready"`
	Detail            string                 `json:"detail,omitempty"`
	Actions           []SearchCoverageAction `json:"actions"`
}

func (s *Server) registerSearchCoverageRoute(api huma.API) {
	op := rawAPIV1Operation("getSearchCoverage", http.MethodPost, "/search/coverage", "Get semantic index coverage for an analytical context")
	op.Tags = []string{"Exploration"}
	op.RequestBody = jsonRequestBodyFor[SearchCoverageRequest](api)
	op.Responses = jsonResponsesFor[SearchCoverageResponse](api)
	addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, op, s.handleSearchCoverage)
}

func (s *Server) handleSearchCoverage(w http.ResponseWriter, r *http.Request) {
	var request SearchCoverageRequest
	if !decodeExploreJSON(w, r, &request) {
		return
	}
	canonical := ExploreHTTPRequest{Filters: request.Filters}
	canonicalizeExploreRequest(&canonical)
	ctx, err := exploreContext(canonical.Filters)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	_, backend, cfg := s.vectorComponents()
	ctx = semanticCoverageContext(ctx, cfg.Embed.Scope.BuildScope())
	explorer, ok := s.engine.(query.Explorer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	response := SearchCoverageResponse{
		Actions: make([]SearchCoverageAction, 0),
	}
	coverage := s.resolveSearchCoverageState(r.Context(), backend, cfg)
	generation := coverage.generation
	if generation == nil {
		// Without a usable generation there is nothing to intersect: report
		// the state immediately instead of scanning the archive (the UI polls
		// this endpoint while semantic search is disabled or initializing).
		response.Status, response.Detail = coverage.status, coverage.detail
		response.Actions = s.searchCoverageActions(coverage.status, coverage.fullBuildAvailable)
		writeJSON(w, http.StatusOK, response)
		return
	}
	counter, _ := backend.(vector.FilteredCoverageBackend)
	generationID := int64(generation.ID)
	response.VectorGeneration = &generationID
	response.VectorFingerprint = generation.Fingerprint

	state := s.exploreState
	if state == nil {
		state = newExploreServerState(time.Now)
		s.exploreState = state
	}
	probe, err := explorer.ExploreSelectionStats(r.Context(), query.ExploreSelectionRequest{
		Explore: query.ExploreRequest{Context: ctx}, IncludedKeys: []string{},
	})
	if err != nil {
		s.writeExploreError(w, err)
		return
	}
	contextHash := hashCanonicalValue(ctx, false)
	if entry, found := state.getCoverage(searchCoverageCacheKey(contextHash, probe.CacheRevision, *generation)); found {
		response.EligibleCount, response.EmbeddedCount = entry.EligibleCount, entry.EmbeddedCount
		response.CacheRevision = probe.CacheRevision
		s.writeSearchCoverageCounts(w, response, coverage)
		return
	}

	countFailed := false
	result, err := explorer.ExploreCoverage(r.Context(), query.ExploreCoverageRequest{
		Context: ctx, BatchSize: vector.FilteredCoverageBatchSize,
	}, func(messageIDs []int64) error {
		count, countErr := counter.EmbeddedMessageCountForIDs(r.Context(), generation.ID, messageIDs)
		if countErr != nil {
			countFailed = true
			return countErr
		}
		response.EmbeddedCount += count
		return nil
	})
	if err != nil {
		if countFailed {
			response.EmbeddedCount = 0
			response.Status = SearchCoverageUnavailable
			response.Detail = "Filtered semantic coverage could not be read"
			response.Actions = s.searchCoverageActions(SearchCoverageUnavailable, false)
			writeJSON(w, http.StatusOK, response)
			return
		}
		s.writeExploreError(w, err)
		return
	}
	currentStatus, _, current, _ := resolveSearchCoverageGeneration(
		r.Context(), backend, cfg, coverage.resolvedStatus, coverage.detail,
	)
	if !sameCoverageGeneration(coverage.resolvedStatus, *generation, currentStatus, current) {
		writeError(w, http.StatusServiceUnavailable, "vector_generation_changed",
			"Vector generation changed while coverage was computed; retry")
		return
	}
	response.EligibleCount = result.EligibleCount
	response.CacheRevision = result.CacheRevision
	state.putCoverage(
		searchCoverageCacheKey(contextHash, result.CacheRevision, *generation),
		result.EligibleCount, response.EmbeddedCount,
	)
	s.writeSearchCoverageCounts(w, response, coverage)
}

// searchCoverageState is the resolved vector-side half of one coverage
// request: the reportable status, the usable generation to intersect (nil
// when no scan should run), and the status snapshot used to detect a
// generation swap after the archive scan completes.
type searchCoverageState struct {
	status             SearchCoverageStatus
	detail             string
	generation         *vector.Generation
	fullBuildAvailable bool
	resolvedStatus     SearchCoverageStatus
}

func (s *Server) resolveSearchCoverageState(
	ctx context.Context,
	backend vector.Backend,
	cfg vector.Config,
) searchCoverageState {
	s.refreshVectorStatusIfStale(ctx)
	status, detail := s.VectorStatus()
	backendStatus := coverageStatusForVectorStatus(status)
	state := searchCoverageState{status: backendStatus, detail: detail, resolvedStatus: backendStatus}
	if backend != nil && backendStatus != SearchCoverageDisabled && backendStatus != SearchCoverageUnavailable {
		state.status, state.detail, state.generation, state.fullBuildAvailable = resolveSearchCoverageGeneration(
			ctx, backend, cfg, backendStatus, detail,
		)
		state.resolvedStatus = state.status
	}
	if _, canCount := backend.(vector.FilteredCoverageBackend); state.generation != nil && !canCount {
		state.status = SearchCoverageUnavailable
		state.detail = "The vector backend cannot report filtered coverage"
		state.generation = nil
		state.fullBuildAvailable = false
	}
	return state
}

// writeSearchCoverageCounts finalizes and writes a coverage response that
// was computed (or served from cache) against a usable vector generation.
func (s *Server) writeSearchCoverageCounts(
	w http.ResponseWriter,
	response SearchCoverageResponse,
	coverage searchCoverageState,
) {
	response.Percentage = coveragePercentage(response.EmbeddedCount, response.EligibleCount)
	response.Status, response.Detail = coverage.status, coverage.detail
	if coverage.status != SearchCoverageStale && response.EmbeddedCount < response.EligibleCount {
		response.Status = SearchCoverageIncomplete
	} else if coverage.status != SearchCoverageStale {
		response.Status = SearchCoverageReady
	}
	response.Actions = s.searchCoverageActions(response.Status, coverage.fullBuildAvailable)
	writeJSON(w, http.StatusOK, response)
}

// searchCoverageCacheKey identifies one coverage computation: the canonical
// coverage context, the committed analytical cache revision it was computed
// against, and the full identity of the vector generation it intersected.
func searchCoverageCacheKey(contextHash, cacheRevision string, generation vector.Generation) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s|%d",
		contextHash, cacheRevision,
		generation.ID, generation.State, generation.Fingerprint, generation.MessageCount)
}

func resolveSearchCoverageGeneration(
	ctx context.Context,
	backend vector.Backend,
	cfg vector.Config,
	cachedStatus SearchCoverageStatus,
	detail string,
) (SearchCoverageStatus, string, *vector.Generation, bool) {
	if backend == nil {
		return SearchCoverageUnavailable, "The vector backend is unavailable", nil, false
	}
	expectedFingerprint := ""
	if cfg.Enabled {
		expectedFingerprint = cfg.GenerationFingerprint()
	}
	generation, err := vector.ResolveActiveForFingerprint(ctx, backend, expectedFingerprint)
	if err == nil {
		if cachedStatus == SearchCoverageStale {
			return SearchCoverageStale, detail, &generation, true
		}
		return SearchCoverageReady, "", &generation, false
	}
	switch {
	case errors.Is(err, vector.ErrIndexStale):
		active, activeErr := backend.ActiveGeneration(ctx)
		if activeErr != nil {
			return SearchCoverageUnavailable, "The active vector generation could not be read", nil, false
		}
		building, buildingErr := backend.BuildingGeneration(ctx)
		fullBuildAvailable := buildingErr == nil && (building == nil ||
			expectedFingerprint == "" || building.Fingerprint == expectedFingerprint)
		return SearchCoverageStale, err.Error(), &active, fullBuildAvailable
	case errors.Is(err, vector.ErrIndexBuilding):
		building, buildingErr := backend.BuildingGeneration(ctx)
		if buildingErr != nil || building == nil {
			return SearchCoverageUnavailable, "The building vector generation could not be read", nil, false
		}
		if expectedFingerprint != "" && building.Fingerprint != expectedFingerprint {
			return SearchCoverageUnavailable, "A vector build is running for a different embedding configuration", nil, false
		}
		return SearchCoverageInitializing, "The first semantic index build is in progress", nil, false
	case errors.Is(err, vector.ErrNotEnabled):
		return SearchCoverageUnavailable, "No vector generation has been initialized", nil, true
	default:
		if detail == "" {
			detail = "The vector generation state could not be read"
		}
		return SearchCoverageUnavailable, detail, nil, false
	}
}

func sameCoverageGeneration(
	wantStatus SearchCoverageStatus,
	want vector.Generation,
	gotStatus SearchCoverageStatus,
	got *vector.Generation,
) bool {
	return got != nil && gotStatus == wantStatus && got.ID == want.ID &&
		got.State == want.State && got.Fingerprint == want.Fingerprint &&
		got.MessageCount == want.MessageCount
}

func semanticCoverageContext(ctx query.Context, scope vector.BuildScope) query.Context {
	// Vector generations cover only active archive messages.
	if ctx.Deletion == query.DeletionDeleted {
		ctx.MessageTypes = []string{"\x00no-semantic-eligible-message-type"}
		ctx.Deletion = query.DeletionActive
		return ctx
	}
	ctx.Deletion = query.DeletionActive
	if scope.IsEmpty() {
		return ctx
	}
	if len(ctx.MessageTypes) == 0 {
		ctx.MessageTypes = slices.Clone(scope.MessageTypes)
		return ctx
	}
	eligible := make([]string, 0, len(ctx.MessageTypes))
	for _, messageType := range ctx.MessageTypes {
		if scope.ContainsMessageType(strings.ToLower(messageType)) {
			eligible = append(eligible, messageType)
		}
	}
	if len(eligible) == 0 {
		eligible = []string{"\x00no-semantic-eligible-message-type"}
	}
	ctx.MessageTypes = eligible
	return ctx
}

func coveragePercentage(embedded, eligible int64) float64 {
	if eligible == 0 {
		return 100
	}
	return math.Round((float64(embedded)/float64(eligible))*1000) / 10
}

func coverageStatusForVectorStatus(status VectorStatus) SearchCoverageStatus {
	switch status {
	case VectorStatusDisabled:
		return SearchCoverageDisabled
	case VectorStatusInitializing:
		return SearchCoverageInitializing
	case VectorStatusStale:
		return SearchCoverageStale
	case VectorStatusReady:
		return SearchCoverageReady
	default:
		return SearchCoverageUnavailable
	}
}

func (s *Server) searchCoverageActions(
	status SearchCoverageStatus,
	fullBuildAvailable bool,
) []SearchCoverageAction {
	actions := make([]SearchCoverageAction, 0, 2)
	if status == SearchCoverageUnavailable || (status == SearchCoverageStale && !fullBuildAvailable) {
		actions = append(actions, SearchCoverageActionRetry)
	}
	if fullBuildAvailable {
		if _, ok := s.store.(CLIRunner); ok && s.cfg != nil && s.cfg.Vector.Enabled {
			actions = append(actions, SearchCoverageActionBuildIndex)
		}
	}
	return actions
}
