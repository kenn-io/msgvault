package api

import (
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/query"
)

const exploreFilesMaxLimit = 100

type ExploreFilesHTTPRequest struct {
	Predicate ExploreHTTPRequest `json:"predicate"`
	Cursor    string             `json:"cursor,omitempty"`
	Limit     int                `json:"limit,omitempty" minimum:"0" maximum:"100"`
}

type ExploreFilesHTTPResponse struct {
	Files               []query.ExploreFileFact `json:"files"`
	TotalCount          int64                   `json:"total_count"`
	CacheRevision       string                  `json:"cache_revision"`
	SearchProvenance    query.SearchProvenance  `json:"search_provenance"`
	NextCursor          string                  `json:"next_cursor,omitempty"`
	CandidateSnapshotID string                  `json:"candidate_snapshot_id,omitempty"`
}

func (s *Server) registerExploreFilesRoute(api huma.API) {
	registerExploreRoute[ExploreFilesHTTPRequest, ExploreFilesHTTPResponse](
		api, "listExploreFiles", "/explore/files", "List bounded chronological attachment facts", s.handleExploreFiles,
	)
}

func (s *Server) handleExploreFiles(w http.ResponseWriter, r *http.Request) {
	var request ExploreFilesHTTPRequest
	if !decodeExploreJSON(w, r, &request) {
		return
	}
	predicate, err := prepareExplorePredicate(request.Predicate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_files_predicate", err.Error())
		return
	}
	if request.Limit == 0 {
		request.Limit = exploreFilesMaxLimit
	}
	if request.Limit < 1 || request.Limit > exploreFilesMaxLimit {
		writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", exploreFilesMaxLimit))
		return
	}
	canonical := request
	canonical.Cursor = ""
	requestHash := hashCanonicalValue(canonical, false)
	offset, ok := s.parseExploreCursor(w, request.Cursor, requestHash)
	if !ok {
		return
	}
	var cursor exploreCursor
	if request.Cursor != "" {
		cursor, _ = s.decodeExploreCursor(request.Cursor)
		if predicate.request.SearchMode == exploreSearchModeSemantic || predicate.request.SearchMode == exploreSearchModeHybrid {
			if cursor.Snapshot == "" {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "semantic cursor is missing its candidate snapshot")
				return
			}
			predicate.request.CandidateSnapshotID = cursor.Snapshot
		}
	}
	searchSpec, snapshotID, ok := s.resolveExploreSearch(r.Context(), w, predicate.request)
	if !ok || !requireCompleteCandidatePool(w, searchSpec) {
		return
	}
	if request.Cursor != "" && cursor.SearchRevision != exploreResolvedSearchRevision(searchSpec) {
		writeError(w, http.StatusConflict, "search_revision_changed", "The resolved search index revision changed; restart pagination")
		return
	}
	predicate.query.Search = searchSpec
	analyzer, ok := s.engine.(query.Explorer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	result, err := analyzer.ExploreFiles(r.Context(), query.ExploreFilesRequest{
		Explore: predicate.query, Page: query.PageSpec{Limit: request.Limit, Offset: offset},
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
	response := ExploreFilesHTTPResponse{
		Files: result.Files, TotalCount: result.TotalCount, CacheRevision: result.CacheRevision,
		SearchProvenance: result.SearchProvenance, CandidateSnapshotID: snapshotID,
	}
	if next := offset + len(result.Files); next < int(result.TotalCount) {
		response.NextCursor = s.encodeExploreCursor(exploreCursor{
			Offset: next, Request: requestHash, Revision: result.CacheRevision,
			SearchRevision: exploreResolvedSearchRevision(searchSpec), Snapshot: snapshotID,
		})
	}
	writeJSON(w, http.StatusOK, response)
}
