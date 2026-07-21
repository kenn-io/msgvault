package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"

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
	_, _, cfg := s.vectorComponents()
	ctx = semanticCoverageContext(ctx, cfg.Embed.Scope.BuildScope())
	explorer, ok := s.engine.(query.Explorer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	s.refreshVectorStatusIfStale(r.Context())
	status, detail := s.VectorStatus()
	response := SearchCoverageResponse{
		Actions: make([]SearchCoverageAction, 0),
	}
	backendStatus := coverageStatusForVectorStatus(status)
	_, backend, _ := s.vectorComponents()
	var generation *vector.Generation
	fullBuildAvailable := false
	resolvedStatus := backendStatus
	if backend != nil && backendStatus != SearchCoverageDisabled && backendStatus != SearchCoverageUnavailable {
		backendStatus, detail, generation, fullBuildAvailable = resolveSearchCoverageGeneration(
			r.Context(), backend, cfg, backendStatus, detail,
		)
		resolvedStatus = backendStatus
	}
	counter, canCount := backend.(vector.FilteredCoverageBackend)
	if generation != nil && !canCount {
		backendStatus = SearchCoverageUnavailable
		detail = "The vector backend cannot report filtered coverage"
		generation = nil
		fullBuildAvailable = false
	}
	if generation != nil {
		generationID := int64(generation.ID)
		response.VectorGeneration = &generationID
		response.VectorFingerprint = generation.Fingerprint
	}

	var afterMessageID int64
	for {
		population, err := explorer.ExploreCoverage(r.Context(), query.ExploreCoverageRequest{
			Context: ctx, AfterMessageID: afterMessageID, Limit: vector.FilteredCoverageBatchSize,
		})
		if err != nil {
			s.writeExploreError(w, err)
			return
		}
		if response.CacheRevision == "" {
			response.CacheRevision = population.CacheRevision
		} else if response.CacheRevision != population.CacheRevision {
			writeError(w, http.StatusServiceUnavailable, "cache_changed", "Analytical cache changed while coverage was computed; retry")
			return
		}
		if err := validateCoveragePage(afterMessageID, population.MessageIDs, population.NextAfterMessageID); err != nil {
			writeError(w, http.StatusInternalServerError, "coverage_page_invalid", err.Error())
			return
		}
		response.EligibleCount += int64(len(population.MessageIDs))
		if generation != nil {
			count, err := counter.EmbeddedMessageCountForIDs(r.Context(), generation.ID, population.MessageIDs)
			if err != nil {
				response.EmbeddedCount = 0
				backendStatus = SearchCoverageUnavailable
				detail = "Filtered semantic coverage could not be read"
				generation = nil
				fullBuildAvailable = false
			} else {
				response.EmbeddedCount += count
			}
		}
		if population.NextAfterMessageID == nil {
			break
		}
		afterMessageID = *population.NextAfterMessageID
	}
	if generation != nil {
		currentStatus, _, current, _ := resolveSearchCoverageGeneration(
			r.Context(), backend, cfg, resolvedStatus, detail,
		)
		if !sameCoverageGeneration(resolvedStatus, *generation, currentStatus, current) {
			writeError(w, http.StatusServiceUnavailable, "vector_generation_changed",
				"Vector generation changed while coverage was computed; retry")
			return
		}
	}

	response.Percentage = coveragePercentage(response.EmbeddedCount, response.EligibleCount)
	response.Status, response.Detail = backendStatus, detail
	if generation != nil && backendStatus != SearchCoverageStale && response.EmbeddedCount < response.EligibleCount {
		response.Status = SearchCoverageIncomplete
	} else if generation != nil && backendStatus != SearchCoverageStale {
		response.Status = SearchCoverageReady
	}
	response.Actions = s.searchCoverageActions(response.Status, fullBuildAvailable)
	writeJSON(w, http.StatusOK, response)
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

func validateCoveragePage(afterMessageID int64, ids []int64, next *int64) error {
	previous := afterMessageID
	for _, id := range ids {
		if id <= previous {
			return fmt.Errorf("coverage page is not strictly ordered after message %d", previous)
		}
		previous = id
	}
	if next != nil && (len(ids) == 0 || *next != previous) {
		return errors.New("coverage page cursor does not match its final message")
	}
	return nil
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
