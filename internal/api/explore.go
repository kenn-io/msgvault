package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/explorecatalog"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

const apiSortDirectionDesc = "desc"

const (
	exploreDefaultLimit       = 100
	exploreMaxLimit           = 500
	exploreSearchModeFullText = "full_text"
	exploreSearchModeSemantic = "semantic"
	exploreSearchModeHybrid   = "hybrid"
)

type ExploreFilter struct {
	Dimension string   `json:"dimension" enum:"source,participant,domain,message_type,after,before,deletion"`
	Values    []string `json:"values" minItems:"1"`
}

type ExploreSort struct {
	Field     string `json:"field" enum:"occurred_at"`
	Direction string `json:"direction" enum:"desc"`
}

type ExploreGroupDimension string

const (
	ExploreGroupSource      ExploreGroupDimension = explorecatalog.GroupSource
	ExploreGroupParticipant ExploreGroupDimension = explorecatalog.GroupParticipant
	ExploreGroupDomain      ExploreGroupDimension = explorecatalog.GroupDomain
	ExploreGroupMessageType ExploreGroupDimension = explorecatalog.GroupMessageType
	ExploreGroupKind        ExploreGroupDimension = explorecatalog.GroupKind
	ExploreGroupYear        ExploreGroupDimension = explorecatalog.GroupYear
	ExploreGroupMonth       ExploreGroupDimension = explorecatalog.GroupMonth
)

var exploreGroupDimensions = func() []ExploreGroupDimension {
	values := explorecatalog.GroupingDimensions()
	dimensions := make([]ExploreGroupDimension, len(values))
	for i, value := range values {
		dimensions[i] = ExploreGroupDimension(value)
	}
	return dimensions
}()

type ExploreHTTPRequest struct {
	Filters             []ExploreFilter         `json:"filters,omitempty"`
	Query               string                  `json:"query,omitempty"`
	SearchMode          string                  `json:"search_mode,omitempty" enum:"full_text,semantic,hybrid"`
	Grouping            []ExploreGroupDimension `json:"grouping,omitempty"`
	Presentation        string                  `json:"presentation,omitempty" enum:"table,timeline,files"`
	Sort                []ExploreSort           `json:"sort,omitempty"`
	Cursor              string                  `json:"cursor,omitempty"`
	Limit               int                     `json:"limit,omitempty" minimum:"0" maximum:"500"`
	CandidateSnapshotID string                  `json:"candidate_snapshot_id,omitempty"`
}

type ExploreHTTPResponse struct {
	Rows                   []query.EntryRow       `json:"rows"`
	TotalCount             *int64                 `json:"total_count,omitempty"`
	CacheRevision          string                 `json:"cache_revision"`
	SearchProvenance       query.SearchProvenance `json:"search_provenance"`
	CandidateSnapshotID    string                 `json:"candidate_snapshot_id,omitempty"`
	NextCursor             string                 `json:"next_cursor,omitempty"`
	CandidatePoolSaturated bool                   `json:"candidate_pool_saturated,omitempty"`
	// SearchDeletionScope is "active" when a semantic or hybrid search
	// narrowed an unrestricted deletion context to active messages only —
	// vector candidates never include source-deleted messages, and the
	// narrowing is declared instead of silent.
	SearchDeletionScope string `json:"search_deletion_scope,omitempty"`
}

type ExploreGroupSort struct {
	Field     string `json:"field" enum:"key,count,estimated_bytes,latest_at"`
	Direction string `json:"direction" enum:"asc,desc"`
}

type ExploreGroupsHTTPRequest struct {
	Filters      []ExploreFilter         `json:"filters,omitempty"`
	Query        string                  `json:"query,omitempty"`
	SearchMode   string                  `json:"search_mode,omitempty" enum:"full_text,semantic,hybrid"`
	Grouping     []ExploreGroupDimension `json:"grouping" minItems:"1" maxItems:"1"`
	Presentation string                  `json:"presentation,omitempty" enum:"table"`
	Sort         []ExploreGroupSort      `json:"sort,omitempty" maxItems:"1"`
	Cursor       string                  `json:"cursor,omitempty"`
	Limit        int                     `json:"limit,omitempty" minimum:"0" maximum:"500"`
}

type ExploreGroupsHTTPResponse struct {
	Rows                []query.ExploreGroupRow `json:"rows"`
	TotalCount          int64                   `json:"total_count"`
	CacheRevision       string                  `json:"cache_revision"`
	SearchProvenance    query.SearchProvenance  `json:"search_provenance"`
	NextCursor          string                  `json:"next_cursor,omitempty"`
	CandidateSnapshotID string                  `json:"candidate_snapshot_id,omitempty"`
	SearchDeletionScope string                  `json:"search_deletion_scope,omitempty"`
}

type ExploreSelection struct {
	Mode                string                 `json:"mode" enum:"explicit,all_matching"`
	Predicate           ExploreHTTPRequest     `json:"predicate"`
	RowKeys             []string               `json:"row_keys,omitempty"`
	Exclusions          []string               `json:"exclusions,omitempty"`
	CacheRevision       string                 `json:"cache_revision"`
	SearchProvenance    query.SearchProvenance `json:"search_provenance"`
	CandidateSnapshotID string                 `json:"candidate_snapshot_id,omitempty"`
}

type ExplorePreflightRequest struct {
	Selection ExploreSelection `json:"selection"`
}

type ExploreUnavailableAction struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

type ExploreActionTarget struct {
	Action    string `json:"action"`
	MessageID int64  `json:"message_id"`
	Filename  string `json:"filename"`
}

type ExplorePreflightResponse struct {
	Count               int64                      `json:"count"`
	EstimatedBytes      int64                      `json:"estimated_bytes"`
	CacheRevision       string                     `json:"cache_revision"`
	SearchProvenance    query.SearchProvenance     `json:"search_provenance"`
	UnavailableActions  []ExploreUnavailableAction `json:"unavailable_actions"`
	ActionTargets       []ExploreActionTarget      `json:"action_targets"`
	OperationToken      string                     `json:"operation_token"`
	ExpiresAt           time.Time                  `json:"expires_at"`
	SearchDeletionScope string                     `json:"search_deletion_scope,omitempty"`
}

type ExploreMatchCountsRequest struct {
	Predicate ExploreHTTPRequest `json:"predicate"`
	RowKeys   []string           `json:"row_keys" minItems:"1" maxItems:"500"`
}

type ExploreRowMatchCount struct {
	RowKey string `json:"row_key"`
	Count  int64  `json:"count"`
}

type ExploreMatchCountsResponse struct {
	Counts             []ExploreRowMatchCount `json:"counts"`
	CacheRevision      string                 `json:"cache_revision"`
	LexicalRevision    string                 `json:"lexical_index_revision"`
	CanonicalQueryHash string                 `json:"canonical_query_hash"`
}

type exploreCursor struct {
	Offset           int    `json:"offset"`
	Request          string `json:"request"`
	Revision         string `json:"revision"`
	SearchRevision   string `json:"search_revision,omitempty"`
	Snapshot         string `json:"snapshot,omitempty"`
	IdentityRevision int64  `json:"identity_revision,omitempty"`

	// Timezone and CanonicalID pin a relationship timeline cursor (see
	// handleRelationshipTimeline) to the request that produced it. Unlike
	// every other explore-style cursor, a mismatch on any field here —
	// including the request hash — maps uniformly to 409 cursor_invalidated
	// rather than a 400/409 split, per that endpoint's contract.
	Timezone    string `json:"timezone,omitempty"`
	CanonicalID int64  `json:"canonical_id,omitempty"`
}

type explorePrepared struct {
	request             ExploreHTTPRequest
	query               query.ExploreRequest
	requestHash         string
	offset              int
	searchDeletionScope string
}

func (s *Server) registerExploreRoutes(api huma.API) {
	registerExploreRoute[ExploreHTTPRequest, ExploreHTTPResponse](api, "explore", "/explore", "Explore canonical archive entries", s.handleExplore)
	registerExploreRoute[ExploreGroupsHTTPRequest, ExploreGroupsHTTPResponse](api, "exploreGroups", "/explore/groups", "Group canonical archive entries", s.handleExploreGroups)
	registerExploreRoute[ExplorePreflightRequest, ExplorePreflightResponse](api, "preflightExploreSelection", "/explore/preflight", "Preflight a revision-pinned analytical selection", s.handleExplorePreflight)
	registerExploreRoute[ExploreMatchCountsRequest, ExploreMatchCountsResponse](api, "countExploreMatches", "/explore/match-counts", "Count exact lexical matches in visible rows", s.handleExploreMatchCounts)
	s.registerExploreFilesRoute(api)
}

func registerExploreRoute[Req any, Resp any](api huma.API, operationID, path, summary string, handler http.HandlerFunc) {
	op := rawAPIV1Operation(operationID, http.MethodPost, path, summary)
	op.Tags = []string{"Exploration"}
	op.RequestBody = jsonRequestBodyFor[Req](api)
	op.Responses = jsonResponsesFor[Resp](api)
	addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable)
	op.Responses[httpStatusKey(http.StatusServiceUnavailable)] = &huma.Response{
		Description: http.StatusText(http.StatusServiceUnavailable),
		Content: map[string]*huma.MediaType{
			"application/json": {Schema: &huma.Schema{AnyOf: []*huma.Schema{
				schemaFor[ExploreCacheUnavailableResponse](api),
				schemaFor[ErrorResponse](api),
			}}},
		},
	}
	registerRawHumaRoute(api, op, handler)
}

func (s *Server) handleExplore(w http.ResponseWriter, r *http.Request) {
	s.handleExploreWithScope(w, r, nil)
}

func (s *Server) handleExploreWithScope(w http.ResponseWriter, r *http.Request, scope *ExploreFilter) {
	prepared, ok := s.prepareExploreRequest(w, r, scope)
	if !ok {
		return
	}
	if prepared.request.Cursor != "" && (prepared.request.SearchMode == exploreSearchModeSemantic || prepared.request.SearchMode == exploreSearchModeHybrid) {
		cursor, err := s.decodeExploreCursor(prepared.request.Cursor)
		if err != nil || cursor.Snapshot == "" {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "semantic cursor is missing its candidate snapshot")
			return
		}
		prepared.request.CandidateSnapshotID = cursor.Snapshot
	}
	searchSpec, snapshotID, ok := s.resolveExploreSearch(r.Context(), w, prepared.request)
	if !ok {
		return
	}
	if prepared.request.Cursor != "" {
		cursor, _ := s.decodeExploreCursor(prepared.request.Cursor)
		if cursor.SearchRevision != exploreResolvedSearchRevision(searchSpec) {
			writeError(w, http.StatusConflict, "search_revision_changed", "The resolved search index revision changed; restart pagination")
			return
		}
	}
	prepared.query.Search = searchSpec
	if scope != nil {
		if err := applyIdentityScope(&prepared.query.Context, *scope); err != nil {
			writeError(w, http.StatusConflict, "identity_scope_conflict", err.Error())
			return
		}
	}
	semanticPage := searchSpec.Mode == query.SearchSemantic || searchSpec.Mode == query.SearchHybrid
	if semanticPage {
		prepared.query.Page = query.PageSpec{Limit: exploreMaxLimit}
	}
	explorer, ok := s.engine.(query.Explorer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	result, err := explorer.Explore(r.Context(), prepared.query)
	if err != nil {
		s.writeExploreError(w, err)
		return
	}
	if snapshotID != "" {
		s.enrichExploreSemanticRows(result.Rows, prepared.request, snapshotID)
	}
	semanticTotal := len(result.Rows)
	if semanticPage {
		start := min(prepared.offset, len(result.Rows))
		end := min(start+prepared.request.Limit, len(result.Rows))
		result.Rows = result.Rows[start:end]
	}
	if prepared.offset > 0 {
		cursor, err := s.decodeExploreCursor(prepared.request.Cursor)
		if err != nil || cursor.Revision != result.CacheRevision {
			writeError(w, http.StatusConflict, "archive_revision_changed", "The committed analytical cache changed; restart pagination")
			return
		}
	}
	response := ExploreHTTPResponse{
		Rows: result.Rows, CacheRevision: result.CacheRevision,
		SearchProvenance: result.SearchProvenance, CandidateSnapshotID: snapshotID,
		SearchDeletionScope: prepared.searchDeletionScope,
	}
	response.CandidatePoolSaturated = searchSpec.CandidatePoolSaturated
	if snapshotID != "" && s.exploreState != nil {
		if snapshot, found := s.exploreState.snapshot(snapshotID, exploreSnapshotRequestHash(prepared.request)); found {
			response.CandidatePoolSaturated = response.CandidatePoolSaturated || snapshot.PoolSaturated
		}
	}
	if (prepared.query.Search.Mode == query.SearchNone || prepared.query.Search.Mode == query.SearchFullText) && !response.CandidatePoolSaturated {
		response.TotalCount = &result.TotalCount
	}
	totalForPagination := int(result.TotalCount)
	if semanticPage {
		totalForPagination = semanticTotal
	}
	if nextOffset := prepared.offset + len(result.Rows); nextOffset < totalForPagination {
		response.NextCursor = s.encodeExploreCursor(exploreCursor{
			Offset: nextOffset, Request: prepared.requestHash, Revision: result.CacheRevision,
			SearchRevision: exploreResolvedSearchRevision(searchSpec), Snapshot: snapshotID,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleExploreGroups(w http.ResponseWriter, r *http.Request) {
	var request ExploreGroupsHTTPRequest
	if !decodeExploreJSON(w, r, &request) {
		return
	}
	for i := range request.Filters {
		request.Filters[i].Dimension = strings.ToLower(strings.TrimSpace(request.Filters[i].Dimension))
		for j := range request.Filters[i].Values {
			request.Filters[i].Values[j] = strings.TrimSpace(request.Filters[i].Values[j])
		}
		slices.Sort(request.Filters[i].Values)
		request.Filters[i].Values = slices.Compact(request.Filters[i].Values)
	}
	slices.SortFunc(request.Filters, func(a, b ExploreFilter) int { return strings.Compare(a.Dimension, b.Dimension) })
	request.Query = strings.TrimSpace(request.Query)
	request.SearchMode = strings.ToLower(strings.TrimSpace(request.SearchMode))
	request.Presentation = strings.ToLower(strings.TrimSpace(request.Presentation))
	if err := validateExploreSearchPair(request.Query, request.SearchMode); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_search", err.Error())
		return
	}
	if request.Presentation == "" {
		request.Presentation = "table"
	}
	if request.Presentation != "table" {
		writeError(w, http.StatusBadRequest, "invalid_presentation", "Grouped exploration supports presentation=table")
		return
	}
	if len(request.Grouping) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_grouping", "exactly one grouping dimension is required")
		return
	}
	dimension := ExploreGroupDimension(strings.ToLower(strings.TrimSpace(string(request.Grouping[0]))))
	if !slices.Contains(exploreGroupDimensions, dimension) {
		writeError(w, http.StatusBadRequest, "invalid_grouping", fmt.Sprintf("unknown grouping dimension %q", dimension))
		return
	}
	ctx, err := exploreContext(request.Filters)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	searchDeletionScope := applySemanticDeletionScope(request.SearchMode, &ctx)
	if request.Limit == 0 {
		request.Limit = exploreDefaultLimit
	}
	if request.Limit < 1 || request.Limit > exploreMaxLimit {
		writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", exploreMaxLimit))
		return
	}
	sortSpec := query.SortSpec{Field: "count", Direction: apiSortDirectionDesc}
	if len(request.Sort) > 1 {
		writeError(w, http.StatusBadRequest, "invalid_sort", "at most one group sort is supported")
		return
	}
	if len(request.Sort) == 1 {
		sortSpec = query.SortSpec{Field: strings.ToLower(request.Sort[0].Field), Direction: strings.ToLower(request.Sort[0].Direction)}
		if !slices.Contains([]string{"key", "count", "estimated_bytes", "latest_at"}, sortSpec.Field) ||
			!slices.Contains([]string{"asc", apiSortDirectionDesc}, sortSpec.Direction) {
			writeError(w, http.StatusBadRequest, "invalid_sort", "unknown group sort field or direction")
			return
		}
	}
	request.Grouping = []ExploreGroupDimension{dimension}
	requestHash := hashCanonicalValue(request, true)
	offset, ok := s.parseExploreCursor(w, request.Cursor, requestHash)
	if !ok {
		return
	}
	searchRequest := ExploreHTTPRequest{Filters: request.Filters, Query: request.Query, SearchMode: request.SearchMode}
	canonicalizeExploreRequest(&searchRequest)
	var cursor exploreCursor
	if request.Cursor != "" {
		cursor, _ = s.decodeExploreCursor(request.Cursor)
		if request.SearchMode == exploreSearchModeSemantic || request.SearchMode == exploreSearchModeHybrid {
			if cursor.Snapshot == "" {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "semantic cursor is missing its candidate snapshot")
				return
			}
			searchRequest.CandidateSnapshotID = cursor.Snapshot
		}
	}
	searchSpec, snapshotID, ok := s.resolveExploreSearch(r.Context(), w, searchRequest)
	if !ok || !requireCompleteCandidatePool(w, searchSpec) {
		return
	}
	if request.Cursor != "" && cursor.SearchRevision != exploreResolvedSearchRevision(searchSpec) {
		writeError(w, http.StatusConflict, "search_revision_changed", "The resolved search index revision changed; restart pagination")
		return
	}
	analyzer, ok := s.engine.(query.Explorer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	result, err := analyzer.ExploreGroups(r.Context(), query.ExploreGroupRequest{
		Explore: query.ExploreRequest{Context: ctx, Search: searchSpec}, Dimension: string(dimension), Sort: sortSpec,
		Page: query.PageSpec{Limit: request.Limit, Offset: offset},
	})
	if err != nil {
		s.writeExploreError(w, err)
		return
	}
	if request.Cursor != "" {
		cursor, _ := s.decodeExploreCursor(request.Cursor)
		if cursor.Revision != result.CacheRevision {
			writeError(w, http.StatusConflict, "archive_revision_changed", "The committed analytical cache changed; restart pagination")
			return
		}
	}
	response := ExploreGroupsHTTPResponse{Rows: result.Rows, TotalCount: result.TotalCount, CacheRevision: result.CacheRevision, SearchProvenance: result.SearchProvenance, CandidateSnapshotID: snapshotID, SearchDeletionScope: searchDeletionScope}
	if next := offset + len(result.Rows); next < int(result.TotalCount) {
		response.NextCursor = s.encodeExploreCursor(exploreCursor{
			Offset: next, Request: requestHash, Revision: result.CacheRevision,
			SearchRevision: exploreResolvedSearchRevision(searchSpec), Snapshot: snapshotID,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleExplorePreflight(w http.ResponseWriter, r *http.Request) {
	var request ExplorePreflightRequest
	if !decodeExploreJSON(w, r, &request) {
		return
	}
	selection := &request.Selection
	if selection.Mode != "explicit" && selection.Mode != "all_matching" {
		writeError(w, http.StatusBadRequest, "invalid_selection", "selection mode must be explicit or all_matching")
		return
	}
	predicate, err := prepareExplorePredicate(selection.Predicate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_selection_predicate", err.Error())
		return
	}
	if selection.CacheRevision == "" {
		writeError(w, http.StatusBadRequest, "invalid_selection", "cache_revision is required")
		return
	}
	if predicate.request.SearchMode == exploreSearchModeSemantic || predicate.request.SearchMode == exploreSearchModeHybrid {
		if selection.CandidateSnapshotID == "" {
			writeError(w, http.StatusBadRequest, "candidate_snapshot_required", "Semantic and hybrid preflight require the server-issued candidate snapshot")
			return
		}
		predicate.request.CandidateSnapshotID = selection.CandidateSnapshotID
	}
	searchSpec, _, ok := s.resolveExploreSearch(r.Context(), w, predicate.request)
	if !ok || !requireCompleteCandidatePool(w, searchSpec) {
		return
	}
	predicate.query.Search = searchSpec
	selectionRequest := query.ExploreSelectionRequest{Explore: predicate.query, ExcludedKeys: selection.Exclusions}
	if selection.Mode == "explicit" {
		if selection.RowKeys == nil {
			writeError(w, http.StatusBadRequest, "invalid_selection", "explicit selection requires row_keys")
			return
		}
		selectionRequest.IncludedKeys = selection.RowKeys
	}
	analyzer, ok := s.engine.(query.Explorer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	stats, err := analyzer.ExploreSelectionStats(r.Context(), selectionRequest)
	if err != nil {
		s.writeExploreError(w, err)
		return
	}
	if selection.CacheRevision != stats.CacheRevision {
		writeError(w, http.StatusConflict, "archive_revision_changed", "The committed analytical cache changed; run preflight again")
		return
	}
	if searchSpec.Mode != query.SearchNone && !sameSearchProvenance(selection.SearchProvenance, stats.SearchProvenance) {
		writeError(w, http.StatusConflict, "search_revision_changed", "The search index revision changed; run preflight again")
		return
	}
	selectionHash := hashCanonicalValue(selection, false)
	state := s.exploreState
	if state == nil {
		state = newExploreServerState(time.Now)
		s.exploreState = state
	}
	token := state.issueOperation(selectionHash, stats.Count, stats.CacheRevision)
	unavailableActions := make([]ExploreUnavailableAction, 0, 4)
	actionTargets := make([]ExploreActionTarget, 0, 1)
	if stats.DeletableCount != stats.Count {
		unavailableActions = append(unavailableActions, ExploreUnavailableAction{
			Action: "stage_deletion", Reason: "selection_contains_items_that_cannot_be_deleted_from_source",
		})
	}
	if stats.FileCount == 0 {
		unavailableActions = append(unavailableActions, ExploreUnavailableAction{
			Action: "export_files", Reason: "selection_contains_no_files",
		})
	}
	if stats.Count != 1 {
		unavailableActions = append(unavailableActions, ExploreUnavailableAction{
			Action: "export", Reason: "browser_export_requires_single_message",
		})
	} else if stats.RawExportMessageID == nil {
		unavailableActions = append(unavailableActions, ExploreUnavailableAction{
			Action: "export", Reason: "selection_has_no_exportable_raw_message",
		})
	} else {
		messageID := *stats.RawExportMessageID
		raw, rawErr := s.engine.GetMessageRaw(r.Context(), messageID)
		if rawErr != nil {
			s.logger.Warn("raw export preflight failed", "message_id", messageID, "error", rawErr)
			unavailableActions = append(unavailableActions, ExploreUnavailableAction{
				Action: "export", Reason: "raw_message_unavailable",
			})
		} else if len(raw) == 0 {
			unavailableActions = append(unavailableActions, ExploreUnavailableAction{
				Action: "export", Reason: "selection_has_no_exportable_raw_message",
			})
		} else {
			actionTargets = append(actionTargets, ExploreActionTarget{
				Action: "export", MessageID: messageID, Filename: fmt.Sprintf("message-%d.eml", messageID),
			})
		}
	}
	unavailableActions = append(unavailableActions, ExploreUnavailableAction{
		Action: "open_in_source", Reason: "trusted_source_link_unavailable",
	})
	writeJSON(w, http.StatusOK, ExplorePreflightResponse{
		Count: stats.Count, EstimatedBytes: stats.EstimatedBytes, CacheRevision: stats.CacheRevision,
		SearchProvenance: stats.SearchProvenance, UnavailableActions: unavailableActions,
		ActionTargets:  actionTargets,
		OperationToken: token, ExpiresAt: state.now().Add(exploreOperationTokenTTL),
		SearchDeletionScope: predicate.searchDeletionScope,
	})
}

func sameSearchProvenance(left, right query.SearchProvenance) bool {
	if left.LexicalIndexRevision != right.LexicalIndexRevision {
		return false
	}
	if left.VectorGeneration == nil || right.VectorGeneration == nil {
		return left.VectorGeneration == nil && right.VectorGeneration == nil
	}
	return *left.VectorGeneration == *right.VectorGeneration
}

func (s *Server) handleExploreMatchCounts(w http.ResponseWriter, r *http.Request) {
	var request ExploreMatchCountsRequest
	if !decodeExploreJSON(w, r, &request) {
		return
	}
	if (request.Predicate.SearchMode != exploreSearchModeFullText && request.Predicate.SearchMode != exploreSearchModeHybrid) || request.Predicate.Query == "" {
		writeError(w, http.StatusBadRequest, "lexical_search_required", "match counts require a full_text or hybrid predicate")
		return
	}
	if len(request.RowKeys) == 0 || len(request.RowKeys) > exploreMaxLimit {
		writeError(w, http.StatusBadRequest, "invalid_row_keys", fmt.Sprintf("row_keys must contain between 1 and %d entries", exploreMaxLimit))
		return
	}
	predicate, err := prepareExplorePredicate(request.Predicate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_match_predicate", err.Error())
		return
	}
	searchSpec, _, ok := s.resolveExploreSearch(r.Context(), w, predicate.request)
	if !ok || !requireCompleteCandidatePool(w, searchSpec) {
		return
	}
	predicate.query.Search = searchSpec
	slices.Sort(request.RowKeys)
	request.RowKeys = slices.Compact(request.RowKeys)
	analyzer, ok := s.engine.(query.Explorer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	state := s.exploreState
	if state == nil {
		state = newExploreServerState(time.Now)
		s.exploreState = state
	}
	revisionProbe, err := analyzer.ExploreSelectionStats(r.Context(), query.ExploreSelectionRequest{
		Explore: predicate.query, IncludedKeys: []string{},
	})
	if err != nil {
		s.writeExploreError(w, err)
		return
	}
	cacheRequest := predicate.request
	cacheRequest.Query = ""
	canonicalQueryHash := exploreCanonicalQueryHash(predicate.request.Query)
	cacheKey := canonicalExploreHash(cacheRequest) + "|" + canonicalQueryHash + "|" + revisionProbe.CacheRevision + "|" + searchSpec.LexicalIndexRevision + "|" + hashCanonicalValue(request.RowKeys, false)
	if cached, found := state.getMatchCounts(cacheKey); found {
		counts := make([]ExploreRowMatchCount, len(request.RowKeys))
		for i, key := range request.RowKeys {
			counts[i] = ExploreRowMatchCount{RowKey: key, Count: cached[key]}
		}
		writeJSON(w, http.StatusOK, ExploreMatchCountsResponse{Counts: counts, CacheRevision: revisionProbe.CacheRevision, LexicalRevision: searchSpec.LexicalIndexRevision, CanonicalQueryHash: canonicalQueryHash})
		return
	}
	matchRequest := predicate.query
	if searchSpec.Mode == query.SearchHybrid {
		matchRequest.Search.Mode = query.SearchFullText
		matchRequest.Search.CandidateMessageIDs = searchSpec.LexicalCandidateMessageIDs
		matchRequest.Search.LexicalCandidateMessageIDs = nil
		matchRequest.Search.VectorGeneration = nil
	}
	result, err := analyzer.ExploreMatchCounts(r.Context(), query.ExploreMatchCountsRequest{
		Explore: matchRequest, RowKeys: request.RowKeys,
	})
	if err != nil {
		s.writeExploreError(w, err)
		return
	}
	state.putMatchCounts(cacheKey, result.Counts)
	counts := make([]ExploreRowMatchCount, len(request.RowKeys))
	for i, key := range request.RowKeys {
		counts[i] = ExploreRowMatchCount{RowKey: key, Count: result.Counts[key]}
	}
	writeJSON(w, http.StatusOK, ExploreMatchCountsResponse{
		Counts: counts, CacheRevision: result.CacheRevision,
		LexicalRevision: result.SearchProvenance.LexicalIndexRevision, CanonicalQueryHash: canonicalQueryHash,
	})
}

func exploreCanonicalQueryHash(queryText string) string {
	parsed := search.Parse(queryText)
	if parsed.Err() != nil {
		return ""
	}
	return hashCanonicalValue(canonicalParsedExploreQuery(parsed), false)
}

func (s *Server) prepareExploreRequest(w http.ResponseWriter, r *http.Request, scope *ExploreFilter) (explorePrepared, bool) {
	var request ExploreHTTPRequest
	if !decodeExploreJSON(w, r, &request) {
		return explorePrepared{}, false
	}
	canonicalizeExploreRequest(&request)
	if request.Limit == 0 {
		request.Limit = exploreDefaultLimit
	}
	if request.Limit < 1 || request.Limit > exploreMaxLimit {
		writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", exploreMaxLimit))
		return explorePrepared{}, false
	}
	if err := validateExploreSearchPair(request.Query, request.SearchMode); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_search", err.Error())
		return explorePrepared{}, false
	}
	if len(request.Grouping) != 0 {
		writeError(w, http.StatusBadRequest, "invalid_grouping", "Grouping is available through /api/v1/explore/groups")
		return explorePrepared{}, false
	}
	if request.Presentation == "" {
		request.Presentation = "table"
	}
	if request.Presentation != "table" {
		writeError(w, http.StatusBadRequest, "invalid_presentation", "Entry exploration supports presentation=table")
		return explorePrepared{}, false
	}
	if len(request.Sort) == 0 {
		request.Sort = []ExploreSort{{Field: "occurred_at", Direction: apiSortDirectionDesc}}
	}
	if len(request.Sort) != 1 || request.Sort[0].Field != "occurred_at" || request.Sort[0].Direction != apiSortDirectionDesc {
		writeError(w, http.StatusBadRequest, "invalid_sort", "Entry exploration supports only occurred_at descending")
		return explorePrepared{}, false
	}
	ctx, err := exploreContext(request.Filters)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return explorePrepared{}, false
	}
	searchDeletionScope := applySemanticDeletionScope(request.SearchMode, &ctx)
	requestHash := canonicalScopedExploreHash(request, scope)
	offset := 0
	if request.Cursor != "" {
		cursor, err := s.decodeExploreCursor(request.Cursor)
		if err != nil || cursor.Offset < 0 || cursor.Request != requestHash {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor does not match the canonical request")
			return explorePrepared{}, false
		}
		offset = cursor.Offset
	}
	return explorePrepared{
		request: request, requestHash: requestHash, offset: offset,
		searchDeletionScope: searchDeletionScope,
		query: query.ExploreRequest{
			Context: ctx, Presentation: query.PresentationTable,
			Sort: []query.SortSpec{{Field: "sent_at", Direction: apiSortDirectionDesc}},
			Page: query.PageSpec{Limit: request.Limit, Offset: offset},
		},
	}, true
}

func canonicalScopedExploreHash(request ExploreHTTPRequest, scope *ExploreFilter) string {
	if scope == nil {
		return canonicalExploreHash(request)
	}
	request.Cursor = ""
	return hashCanonicalValue(struct {
		Request ExploreHTTPRequest `json:"request"`
		Scope   ExploreFilter      `json:"identity_scope"`
	}{Request: request, Scope: *scope}, false)
}

func applyIdentityScope(context *query.Context, scope ExploreFilter) error {
	switch scope.Dimension {
	case "participant":
		// One or more IDs: identity-scoped endpoints pass every member of
		// the requested participant's cluster so alias-owned activity is in
		// scope. A base-predicate participant filter narrows the cluster to
		// the members it allows rather than being overwritten.
		if len(scope.Values) == 0 {
			return errors.New("participant identity scope requires at least one exact ID")
		}
		ids := make([]int64, len(scope.Values))
		for i, value := range scope.Values {
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id < 1 {
				return errors.New("participant identity scope requires positive integer IDs")
			}
			ids[i] = id
		}
		if len(context.ParticipantIDs) > 0 {
			ids = slices.DeleteFunc(ids, func(id int64) bool { return !slices.Contains(context.ParticipantIDs, id) })
			if len(ids) == 0 {
				return errors.New("the base predicate excludes the requested participant")
			}
		}
		context.ParticipantIDs = ids
	case "domain":
		if len(scope.Values) != 1 {
			return errors.New("domain identity scope requires one exact domain")
		}
		domain := strings.ToLower(strings.TrimSpace(scope.Values[0]))
		if domain == "" {
			return errors.New("domain identity scope requires an exact domain")
		}
		compatible := slices.ContainsFunc(context.Domains, func(candidate string) bool {
			return strings.EqualFold(strings.TrimSpace(candidate), domain)
		})
		if len(context.Domains) > 0 && !compatible {
			return errors.New("the base predicate excludes the requested domain")
		}
		context.Domains = []string{domain}
	default:
		return fmt.Errorf("unsupported identity scope %q", scope.Dimension)
	}
	return nil
}

func exploreContext(filters []ExploreFilter) (query.Context, error) {
	var result query.Context
	seen := map[string]struct{}{}
	for _, filter := range filters {
		if _, ok := seen[filter.Dimension]; ok {
			return result, fmt.Errorf("filter dimension %q may appear only once", filter.Dimension)
		}
		seen[filter.Dimension] = struct{}{}
		if len(filter.Values) == 0 {
			return result, fmt.Errorf("filter dimension %q requires at least one value", filter.Dimension)
		}
		switch filter.Dimension {
		case "source", "participant":
			ids := make([]int64, len(filter.Values))
			for i, value := range filter.Values {
				id, err := strconv.ParseInt(value, 10, 64)
				if err != nil || id < 1 {
					return result, fmt.Errorf("filter dimension %q requires positive integer IDs", filter.Dimension)
				}
				ids[i] = id
			}
			if filter.Dimension == "source" {
				result.SourceIDs = ids
			} else {
				result.ParticipantIDs = ids
			}
		case "domain":
			result.Domains = append([]string(nil), filter.Values...)
		case "message_type":
			result.MessageTypes = append([]string(nil), filter.Values...)
		case "after", "before":
			if len(filter.Values) != 1 {
				return result, fmt.Errorf("filter dimension %q requires exactly one timestamp", filter.Dimension)
			}
			value, err := time.Parse(time.RFC3339, filter.Values[0])
			if err != nil {
				return result, fmt.Errorf("filter dimension %q requires an RFC3339 timestamp", filter.Dimension)
			}
			if filter.Dimension == "after" {
				result.After = &value
			} else {
				result.Before = &value
			}
		case "deletion":
			if len(filter.Values) != 1 {
				return result, errors.New("filter dimension \"deletion\" requires exactly one value")
			}
			result.Deletion = query.DeletionFilter(filter.Values[0])
			if result.Deletion != query.DeletionActive && result.Deletion != query.DeletionDeleted && result.Deletion != query.DeletionAny {
				return result, errors.New("filter dimension \"deletion\" accepts active or deleted")
			}
		default:
			return result, fmt.Errorf("unknown filter dimension %q", filter.Dimension)
		}
	}
	return result, nil
}

func validateExploreSearchPair(queryText, mode string) error {
	if (queryText == "") != (mode == "") {
		return errors.New("query and search_mode must be provided together")
	}
	if mode != "" && !slices.Contains([]string{exploreSearchModeFullText, exploreSearchModeSemantic, exploreSearchModeHybrid}, mode) {
		return errors.New("search_mode must be full_text, semantic, or hybrid")
	}
	return nil
}

func canonicalizeExploreRequest(request *ExploreHTTPRequest) {
	for i := range request.Filters {
		request.Filters[i].Dimension = strings.ToLower(strings.TrimSpace(request.Filters[i].Dimension))
		for j := range request.Filters[i].Values {
			request.Filters[i].Values[j] = strings.TrimSpace(request.Filters[i].Values[j])
		}
		slices.Sort(request.Filters[i].Values)
		request.Filters[i].Values = slices.Compact(request.Filters[i].Values)
	}
	slices.SortFunc(request.Filters, func(a, b ExploreFilter) int { return strings.Compare(a.Dimension, b.Dimension) })
	request.Query = strings.TrimSpace(request.Query)
	request.SearchMode = strings.ToLower(strings.TrimSpace(request.SearchMode))
	request.Presentation = strings.ToLower(strings.TrimSpace(request.Presentation))
	for i := range request.Grouping {
		request.Grouping[i] = ExploreGroupDimension(strings.ToLower(strings.TrimSpace(string(request.Grouping[i]))))
	}
	for i := range request.Sort {
		request.Sort[i].Field = strings.ToLower(strings.TrimSpace(request.Sort[i].Field))
		request.Sort[i].Direction = strings.ToLower(strings.TrimSpace(request.Sort[i].Direction))
	}
}

func canonicalExploreHash(request ExploreHTTPRequest) string {
	request.Cursor = ""
	data, err := json.Marshal(request)
	if err != nil {
		panic(fmt.Sprintf("marshal canonical explore request: %v", err))
	}
	return fmt.Sprintf("explore-%x", sha256.Sum256(data))
}

func hashCanonicalValue(value any, clearCursor bool) string {
	if clearCursor {
		switch request := value.(type) {
		case ExploreGroupsHTTPRequest:
			request.Cursor = ""
			value = request
		case ExploreHTTPRequest:
			request.Cursor = ""
			value = request
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("marshal canonical explore value: %v", err))
	}
	return fmt.Sprintf("explore-%x", sha256.Sum256(data))
}

func exploreResolvedSearchRevision(spec query.SearchSpec) string {
	if spec.LexicalIndexRevision != "" {
		return spec.LexicalIndexRevision
	}
	if spec.VectorGeneration != nil {
		return fmt.Sprintf("vector:%d", *spec.VectorGeneration)
	}
	return ""
}

func (s *Server) parseExploreCursor(w http.ResponseWriter, encoded, requestHash string) (int, bool) {
	if encoded == "" {
		return 0, true
	}
	cursor, err := s.decodeExploreCursor(encoded)
	if err != nil || cursor.Offset < 0 || cursor.Request != requestHash {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor does not match the canonical request")
		return 0, false
	}
	return cursor.Offset, true
}

func prepareExplorePredicate(request ExploreHTTPRequest) (explorePrepared, error) {
	canonicalizeExploreRequest(&request)
	if err := validateExploreSearchPair(request.Query, request.SearchMode); err != nil {
		return explorePrepared{}, err
	}
	if request.Cursor != "" {
		return explorePrepared{}, errors.New("selection predicates must not contain a cursor")
	}
	for _, dimension := range request.Grouping {
		if !slices.Contains(exploreGroupDimensions, dimension) {
			return explorePrepared{}, fmt.Errorf("unknown grouping dimension %q", dimension)
		}
	}
	if request.Presentation == "" {
		request.Presentation = "table"
	}
	if !slices.Contains([]string{"table", "timeline", "files"}, request.Presentation) {
		return explorePrepared{}, fmt.Errorf("unknown presentation %q", request.Presentation)
	}
	if len(request.Sort) == 0 {
		request.Sort = []ExploreSort{{Field: "occurred_at", Direction: apiSortDirectionDesc}}
	}
	if len(request.Sort) != 1 || request.Sort[0].Field != "occurred_at" || request.Sort[0].Direction != apiSortDirectionDesc {
		return explorePrepared{}, errors.New("selection predicate supports only occurred_at descending")
	}
	ctx, err := exploreContext(request.Filters)
	if err != nil {
		return explorePrepared{}, err
	}
	searchDeletionScope := applySemanticDeletionScope(request.SearchMode, &ctx)
	return explorePrepared{
		request: request, requestHash: canonicalExploreHash(request),
		searchDeletionScope: searchDeletionScope,
		query:               query.ExploreRequest{Context: ctx},
	}, nil
}

func (s *Server) resolveExploreSearch(ctx context.Context, w http.ResponseWriter, request ExploreHTTPRequest) (query.SearchSpec, string, bool) {
	if request.SearchMode == "" {
		return query.SearchSpec{}, "", true
	}
	if request.SearchMode == exploreSearchModeSemantic || request.SearchMode == exploreSearchModeHybrid {
		return s.resolveExploreVectorSearch(ctx, w, request)
	}
	if request.SearchMode != exploreSearchModeFullText {
		writeError(w, http.StatusBadRequest, "invalid_search_mode", "search_mode must be full_text, semantic, or hybrid")
		return query.SearchSpec{}, "", false
	}
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "lexical_index_unavailable", "The full-text index is unavailable")
		return query.SearchSpec{}, "", false
	}
	parsed := search.Parse(request.Query)
	if err := parsed.Err(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return query.SearchSpec{}, "", false
	}
	filters, err := exploreContext(request.Filters)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return query.SearchSpec{}, "", false
	}
	matchable := applyLexicalFilterPushdown(parsed, filters)
	candidateCap := s.lexicalCandidateCap
	if candidateCap <= 0 {
		candidateCap = query.MaxExploreCandidateMessageIDs
	}
	ids := make([]int64, 0)
	seen := make(map[int64]struct{})
	offset := 0
	var reportedTotal int64
	for matchable {
		remainingCapacity := candidateCap - len(ids)
		if remainingCapacity <= 0 {
			break
		}
		pageLimit := min(exploreMaxLimit, remainingCapacity)
		var (
			messages []APIMessage
			total    int64
			err      error
		)
		if searcher, ok := s.store.(ctxMessageSearcher); ok {
			messages, total, err = searcher.SearchMessagesQueryContext(ctx, parsed, offset, pageLimit)
		} else {
			messages, total, err = s.store.SearchMessagesQuery(parsed, offset, pageLimit)
		}
		if err != nil {
			if s.writeIfContextError(w, err) {
				return query.SearchSpec{}, "", false
			}
			writeError(w, http.StatusServiceUnavailable, "lexical_index_unavailable", "The full-text index could not resolve candidates")
			return query.SearchSpec{}, "", false
		}
		reportedTotal = total
		for _, message := range messages {
			if _, ok := seen[message.ID]; ok {
				continue
			}
			seen[message.ID] = struct{}{}
			ids = append(ids, message.ID)
		}
		offset += len(messages)
		if len(messages) == 0 || int64(offset) >= total {
			break
		}
	}
	slices.Sort(ids)
	saturated := reportedTotal > int64(offset)
	revision := "fts5:" + hashCanonicalValue(struct {
		ParsedQuery string  `json:"parsed_query"`
		IDs         []int64 `json:"ids"`
		Total       int64   `json:"total"`
		Saturated   bool    `json:"saturated"`
	}{ParsedQuery: canonicalParsedExploreQuery(parsed), IDs: ids, Total: reportedTotal, Saturated: saturated}, false)
	return query.SearchSpec{
		Mode: query.SearchFullText, Query: request.Query, CandidateMessageIDs: ids,
		LexicalIndexRevision: revision, CandidatePoolSaturated: saturated,
	}, "", true
}

// applyLexicalFilterPushdown narrows the parsed search query with the
// request filters the candidate resolvers evaluate natively — source,
// message_type, after, and before — so the bounded candidate cap applies
// to the filtered population instead of truncating it before the filters
// run. Both resolvers share it: the lexical resolver pushes the narrowed
// query into SQLite FTS5, and the vector resolver builds its backend
// filter from the narrowed query so semantic and hybrid candidates obey
// identical intersection semantics. Participant, domain, and deletion
// filters stay DuckDB-side: they need junction or derived data the
// resolvers do not model, and re-applying every filter analytically keeps
// deferred dimensions correct (pushdown only shrinks the candidate set,
// never widens results).
//
// Pushed dimensions intersect with any equivalent operator already present
// in the query text (in:, message_type:, after:, before:) so the candidate
// set never grows beyond what the parsed query alone would match. The
// returned bool is false when such an intersection is empty, meaning the
// combined predicate can match no messages and the resolver should skip
// the index entirely.
func applyLexicalFilterPushdown(parsed *search.Query, filters query.Context) bool {
	if len(filters.SourceIDs) > 0 {
		if len(parsed.AccountIDs) == 0 {
			parsed.AccountIDs = slices.Clone(filters.SourceIDs)
		} else {
			parsed.AccountIDs = slices.DeleteFunc(slices.Clone(parsed.AccountIDs), func(id int64) bool {
				return !slices.Contains(filters.SourceIDs, id)
			})
			if len(parsed.AccountIDs) == 0 {
				return false
			}
		}
	}
	if len(filters.MessageTypes) > 0 {
		// The parser lowercases message_type: values and stored types are
		// lowercase, so lowercased filter values intersect exactly.
		types := make([]string, 0, len(filters.MessageTypes))
		for _, messageType := range filters.MessageTypes {
			types = append(types, strings.ToLower(messageType))
		}
		if len(parsed.MessageTypes) > 0 {
			types = slices.DeleteFunc(types, func(messageType string) bool {
				return !slices.Contains(parsed.MessageTypes, messageType)
			})
			if len(types) == 0 {
				return false
			}
		}
		parsed.MessageTypes = types
	}
	if filters.After != nil && (parsed.AfterDate == nil || filters.After.After(*parsed.AfterDate)) {
		bound := *filters.After
		parsed.AfterDate = &bound
	}
	if filters.Before != nil && (parsed.BeforeDate == nil || filters.Before.Before(*parsed.BeforeDate)) {
		bound := *filters.Before
		parsed.BeforeDate = &bound
	}
	return true
}

func requireCompleteCandidatePool(w http.ResponseWriter, spec query.SearchSpec) bool {
	if !spec.CandidatePoolSaturated {
		return true
	}
	writeError(w, http.StatusConflict, "candidate_pool_saturated", "The search matched more items than the bounded analytical candidate pool; narrow the query before requesting exact aggregates or actions")
	return false
}

func (s *Server) resolveExploreVectorSearch(ctx context.Context, w http.ResponseWriter, request ExploreHTTPRequest) (query.SearchSpec, string, bool) {
	requestHash := exploreSnapshotRequestHash(request)
	state := s.exploreState
	if state == nil {
		state = newExploreServerState(time.Now)
		s.exploreState = state
	}
	if request.CandidateSnapshotID != "" {
		snapshot, ok := state.snapshot(request.CandidateSnapshotID, requestHash)
		if !ok {
			writeError(w, http.StatusConflict, "candidate_snapshot_expired", "The semantic candidate snapshot is missing, expired, or belongs to another predicate")
			return query.SearchSpec{}, "", false
		}
		generation := snapshot.Generation
		spec := query.SearchSpec{
			Mode: query.SearchSemantic, Query: request.Query, CandidateMessageIDs: snapshot.IDs,
			VectorGeneration: &generation, CandidatePoolSaturated: snapshot.PoolSaturated,
		}
		if request.SearchMode == exploreSearchModeHybrid {
			spec.Mode = query.SearchHybrid
			spec.LexicalIndexRevision = snapshot.LexicalRevision
			spec.LexicalCandidateMessageIDs = snapshot.LexicalIDs
		}
		return spec, request.CandidateSnapshotID, true
	}
	hybridEngine, _, _ := s.vectorComponents()
	if hybridEngine == nil {
		writeError(w, http.StatusServiceUnavailable, "vector_not_enabled", "Vector search is not configured")
		return query.SearchSpec{}, "", false
	}
	parsed := search.Parse(request.Query)
	if err := parsed.Err(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return query.SearchSpec{}, "", false
	}
	exploreCtx, err := exploreContext(request.Filters)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return query.SearchSpec{}, "", false
	}
	if len(exploreCtx.ParticipantIDs) > 0 || len(exploreCtx.Domains) > 0 {
		writeError(w, http.StatusBadRequest, "semantic_filter_unavailable", "Semantic ranking cannot safely apply participant or domain filters")
		return query.SearchSpec{}, "", false
	}
	if exploreCtx.Deletion == query.DeletionDeleted {
		writeError(w, http.StatusBadRequest, "semantic_deletion_unsupported", "Semantic and hybrid search cover active messages only; remove the deletion:deleted filter to search")
		return query.SearchSpec{}, "", false
	}
	var lexicalSpec query.SearchSpec
	if request.SearchMode == exploreSearchModeHybrid {
		var ok bool
		// Filters ride along so the lexical membership and its saturation
		// flag describe the filtered population; an unfiltered branch could
		// wrongly mark a narrow filtered result as saturated and block
		// groups, files, and preflight.
		lexicalSpec, _, ok = s.resolveExploreSearch(ctx, w, ExploreHTTPRequest{
			Query: request.Query, SearchMode: exploreSearchModeFullText, Filters: request.Filters,
		})
		if !ok {
			return query.SearchSpec{}, "", false
		}
	}
	freeText := strings.Join(parsed.TextTerms, " ")
	if freeText == "" {
		writeError(w, http.StatusBadRequest, "missing_free_text", "Semantic and hybrid exploration require free text")
		return query.SearchSpec{}, "", false
	}
	// Mirror the lexical resolver's pushdown semantics: request source and
	// message-type filters intersect with equivalent query operators, and
	// date bounds only tighten. Because the vector backends OR the values
	// within each filter dimension, appending here would broaden the
	// candidate predicate beyond what the query text alone matches. An
	// empty intersection can match nothing, so it skips the vector index
	// entirely instead of running a broadened query.
	if !applyLexicalFilterPushdown(parsed, exploreCtx) {
		return s.resolveEmptyVectorCandidates(ctx, w, request, state, requestHash, lexicalSpec)
	}
	filter, err := hybridEngine.BuildFilter(ctx, parsed)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "search_filter_unavailable", "The semantic search filter could not be resolved")
		return query.SearchSpec{}, "", false
	}
	mode := hybrid.ModeVector
	if request.SearchMode == exploreSearchModeHybrid {
		mode = hybrid.ModeHybrid
	}
	hits, meta, err := hybridEngine.Search(ctx, hybrid.SearchRequest{
		Mode: mode, FreeText: freeText, Filter: filter, Limit: exploreMaxLimit,
		SubjectTerms: parsed.TextTerms,
	})
	if err != nil {
		s.writeExploreVectorError(w, err)
		return query.SearchSpec{}, "", false
	}
	poolSaturated := meta.PoolSaturated || len(hits) >= exploreMaxLimit || lexicalSpec.CandidatePoolSaturated
	if len(hits) > exploreMaxLimit {
		hits = hits[:exploreMaxLimit]
		poolSaturated = true
	}
	ids := make([]int64, len(hits))
	snapshotHits := make([]exploreCandidateHit, len(hits))
	for i, hit := range hits {
		ids[i] = hit.MessageID
		score := hit.VectorScore
		if request.SearchMode == exploreSearchModeHybrid && !math.IsNaN(hit.RRFScore) {
			score = hit.RRFScore
		}
		snapshotHits[i] = exploreCandidateHit{MessageID: hit.MessageID, Score: score}
	}
	summaries, err := s.getMessagesSummariesByIDs(ctx, ids)
	if err == nil {
		byID := make(map[int64]string, len(summaries))
		for _, summary := range summaries {
			byID[summary.ID] = query.FlattenSnippet(summary.Snippet)
		}
		for i := range snapshotHits {
			snapshotHits[i].Excerpt = byID[snapshotHits[i].MessageID]
		}
	}
	lexicalRevision := ""
	if request.SearchMode == exploreSearchModeHybrid {
		lexicalRevision = lexicalSpec.LexicalIndexRevision
	}
	snapshotID := state.issueSnapshot(exploreCandidateSnapshot{
		RequestHash: requestHash, IDs: ids, LexicalIDs: lexicalSpec.CandidateMessageIDs, Hits: snapshotHits,
		Generation: int64(meta.Generation.ID), LexicalRevision: lexicalRevision, PoolSaturated: poolSaturated,
	})
	generation := int64(meta.Generation.ID)
	spec := query.SearchSpec{Mode: query.SearchSemantic, Query: request.Query, CandidateMessageIDs: ids, VectorGeneration: &generation, CandidatePoolSaturated: poolSaturated}
	if request.SearchMode == exploreSearchModeHybrid {
		spec.Mode = query.SearchHybrid
		spec.LexicalIndexRevision = lexicalRevision
		spec.LexicalCandidateMessageIDs = lexicalSpec.CandidateMessageIDs
	}
	return spec, snapshotID, true
}

// resolveEmptyVectorCandidates terminates a semantic or hybrid search whose
// structured predicate is unsatisfiable — a request filter intersected with
// an equivalent query operator to the empty set — without embedding the query
// or touching the vector index. The empty candidate set still carries full
// provenance: the active generation is resolved with the same fingerprint
// rules the hybrid engine applies, so pagination, preflight, and revision
// pinning treat the empty result like any other resolved candidate set.
func (s *Server) resolveEmptyVectorCandidates(
	ctx context.Context,
	w http.ResponseWriter,
	request ExploreHTTPRequest,
	state *exploreServerState,
	requestHash string,
	lexicalSpec query.SearchSpec,
) (query.SearchSpec, string, bool) {
	_, backend, cfg := s.vectorComponents()
	if backend == nil {
		writeError(w, http.StatusServiceUnavailable, "vector_not_enabled", "Vector search is not configured")
		return query.SearchSpec{}, "", false
	}
	expectedFingerprint := ""
	if cfg.Enabled {
		expectedFingerprint = cfg.GenerationFingerprint()
	}
	active, err := vector.ResolveActiveForFingerprint(ctx, backend, expectedFingerprint)
	if err != nil {
		s.writeExploreVectorError(w, err)
		return query.SearchSpec{}, "", false
	}
	generation := int64(active.ID)
	ids := make([]int64, 0)
	lexicalRevision := ""
	if request.SearchMode == exploreSearchModeHybrid {
		lexicalRevision = lexicalSpec.LexicalIndexRevision
	}
	snapshotID := state.issueSnapshot(exploreCandidateSnapshot{
		RequestHash: requestHash, IDs: ids, LexicalIDs: lexicalSpec.CandidateMessageIDs,
		Hits: make([]exploreCandidateHit, 0), Generation: generation,
		LexicalRevision: lexicalRevision, PoolSaturated: lexicalSpec.CandidatePoolSaturated,
	})
	spec := query.SearchSpec{
		Mode: query.SearchSemantic, Query: request.Query, CandidateMessageIDs: ids,
		VectorGeneration: &generation, CandidatePoolSaturated: lexicalSpec.CandidatePoolSaturated,
	}
	if request.SearchMode == exploreSearchModeHybrid {
		spec.Mode = query.SearchHybrid
		spec.LexicalIndexRevision = lexicalRevision
		spec.LexicalCandidateMessageIDs = lexicalSpec.CandidateMessageIDs
	}
	return spec, snapshotID, true
}

// applySemanticDeletionScope narrows an unrestricted deletion context to
// active-only for semantic and hybrid searches and reports the narrowed
// scope for the response. Vector generations embed live messages only and
// both vector backends exclude source-deleted rows from every query path
// (see semanticCoverageContext), so semantic candidates can never include
// archived messages; declaring the narrowing keeps the analytical context
// and the response contract explicit instead of silently returning
// active-only results under an unrestricted deletion context. A
// deletion:deleted context is left untouched — the vector resolver rejects
// it with semantic_deletion_unsupported.
func applySemanticDeletionScope(searchMode string, context *query.Context) string {
	if searchMode != exploreSearchModeSemantic && searchMode != exploreSearchModeHybrid {
		return ""
	}
	if context.Deletion == query.DeletionDeleted {
		return ""
	}
	context.Deletion = query.DeletionActive
	return string(query.DeletionActive)
}

func exploreSnapshotRequestHash(request ExploreHTTPRequest) string {
	candidateRequest := ExploreHTTPRequest{
		Filters: request.Filters, Query: request.Query, SearchMode: request.SearchMode,
	}
	canonicalizeExploreRequest(&candidateRequest)
	return hashCanonicalValue(candidateRequest, false)
}

func canonicalParsedExploreQuery(parsed *search.Query) string {
	canonical := *parsed
	canonicalStrings := func(values []string) []string {
		values = slices.Clone(values)
		slices.Sort(values)
		return slices.Compact(values)
	}
	canonical.TextTerms = canonicalStrings(parsed.TextTerms)
	canonical.FromAddrs = canonicalStrings(parsed.FromAddrs)
	canonical.ToAddrs = canonicalStrings(parsed.ToAddrs)
	canonical.CcAddrs = canonicalStrings(parsed.CcAddrs)
	canonical.BccAddrs = canonicalStrings(parsed.BccAddrs)
	canonical.SubjectTerms = canonicalStrings(parsed.SubjectTerms)
	canonical.Labels = canonicalStrings(parsed.Labels)
	canonical.MessageTypes = canonicalStrings(parsed.MessageTypes)
	canonical.AccountIDs = slices.Clone(parsed.AccountIDs)
	slices.Sort(canonical.AccountIDs)
	canonical.AccountIDs = slices.Compact(canonical.AccountIDs)
	// Exact candidate membership already captures the resolved date boundary.
	// Excluding clock-derived instants keeps an unchanged relative-date query
	// stable across cursor pages.
	canonical.BeforeDate = nil
	canonical.AfterDate = nil
	canonical.UnsupportedOperators = nil
	return hashCanonicalValue(canonical, false)
}

func (s *Server) enrichExploreSemanticRows(rows []query.EntryRow, request ExploreHTTPRequest, snapshotID string) {
	if s.exploreState == nil {
		return
	}
	snapshot, ok := s.exploreState.snapshot(snapshotID, exploreSnapshotRequestHash(request))
	if !ok {
		return
	}
	byID := make(map[int64]exploreCandidateHit, len(snapshot.Hits))
	rank := make(map[int64]int, len(snapshot.Hits))
	for i, hit := range snapshot.Hits {
		byID[hit.MessageID] = hit
		rank[hit.MessageID] = i
	}
	rowRank := make(map[string]int, len(rows))
	for i := range rows {
		bestRank := len(snapshot.Hits) + 1
		if rows[i].StrongestMatchedMessageID != nil {
			messageID := *rows[i].StrongestMatchedMessageID
			if candidateRank, found := rank[messageID]; found {
				bestRank = candidateRank
			}
			if hit, found := byID[messageID]; found {
				score := hit.Score
				rows[i].Match.SemanticScore = &score
				rows[i].Match.StrongestExcerpt = hit.Excerpt
			}
		}
		rowRank[rows[i].Key] = bestRank
	}
	slices.SortStableFunc(rows, func(a, b query.EntryRow) int {
		if difference := rowRank[a.Key] - rowRank[b.Key]; difference != 0 {
			return difference
		}
		return strings.Compare(a.Key, b.Key)
	})
}

func (s *Server) writeExploreVectorError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, vector.ErrNotEnabled):
		writeError(w, http.StatusServiceUnavailable, "vector_not_enabled", "Vector search is not configured")
	case errors.Is(err, vector.ErrIndexStale):
		writeError(w, http.StatusServiceUnavailable, "index_stale", "The vector index does not match the configured model")
	case errors.Is(err, vector.ErrIndexBuilding):
		writeError(w, http.StatusServiceUnavailable, "index_building", "The vector index is still being built")
	case errors.Is(err, vector.ErrEmbeddingTimeout):
		writeError(w, http.StatusServiceUnavailable, "embedding_timeout", "The embedding endpoint did not respond in time")
	case errors.Is(err, vector.ErrIndexScopeMismatch):
		writeError(w, http.StatusBadRequest, "index_scope_mismatch", err.Error())
	default:
		writeError(w, http.StatusServiceUnavailable, "semantic_search_unavailable", "Semantic search could not resolve candidates")
	}
}

func newExploreCursorKey() [32]byte {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return key
}

func (s *Server) encodeExploreCursor(cursor exploreCursor) string {
	data, err := json.Marshal(cursor)
	if err != nil {
		panic(fmt.Sprintf("marshal explore cursor: %v", err))
	}
	payload := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, s.exploreCursorKey[:])
	_, _ = mac.Write([]byte(payload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + signature
}

func (s *Server) decodeExploreCursor(encoded string) (exploreCursor, error) {
	payload, encodedSignature, ok := strings.Cut(encoded, ".")
	if !ok || payload == "" || encodedSignature == "" || strings.Contains(encodedSignature, ".") {
		return exploreCursor{}, errors.New("invalid explore cursor signature")
	}
	signature, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil {
		return exploreCursor{}, fmt.Errorf("decode explore cursor signature: %w", err)
	}
	mac := hmac.New(sha256.New, s.exploreCursorKey[:])
	_, _ = mac.Write([]byte(payload))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return exploreCursor{}, errors.New("invalid explore cursor signature")
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return exploreCursor{}, fmt.Errorf("decode explore cursor: %w", err)
	}
	var cursor exploreCursor
	err = json.Unmarshal(data, &cursor)
	return cursor, err
}

func decodeExploreJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body: "+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "Request body must contain one JSON object")
		return false
	}
	return true
}

func (s *Server) writeExploreError(w http.ResponseWriter, err error) {
	var unavailable *query.CacheUnavailableError
	if errors.As(err, &unavailable) || errors.Is(err, query.ErrCacheUnavailable) {
		readiness := query.CacheInterrupted
		if unavailable != nil {
			readiness = unavailable.Readiness
		}
		writeExploreUnavailable(w, readiness)
		return
	}
	if errors.Is(err, query.ErrInvalidExploreRequest) {
		writeError(w, http.StatusBadRequest, "invalid_explore_request", err.Error())
		return
	}
	if s.writeIfContextError(w, err) {
		return
	}
	s.logger.Error("exploration failed", "error", err)
	writeError(w, http.StatusInternalServerError, "explore_failed", "Analytical exploration failed")
}

type ExploreCacheUnavailableResponse struct {
	Error          string               `json:"error"`
	Message        string               `json:"message"`
	Readiness      query.CacheReadiness `json:"readiness" enum:"absent,interrupted,stale_schema,drifted"`
	RecoveryAction string               `json:"recovery_action"`
}

func writeExploreUnavailable(w http.ResponseWriter, readiness query.CacheReadiness) {
	writeJSON(w, http.StatusServiceUnavailable, ExploreCacheUnavailableResponse{
		Error: "analytical_cache_unavailable", Message: "The committed analytical cache is unavailable",
		Readiness: readiness, RecoveryAction: "Run msgvault build-cache --full-rebuild and retry",
	})
}
