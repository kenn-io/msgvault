package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/explorecatalog"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/savedview"
	"go.kenn.io/msgvault/internal/store"
)

// runSavedViewMaxLimit bounds one run page. It matches the files surface, the
// smallest of the three Explore page limits, so every result kind honors the
// same request contract.
const runSavedViewMaxLimit = exploreFilesMaxLimit

type RunSavedViewRequest struct {
	Limit  int    `json:"limit,omitempty" minimum:"0" maximum:"100"`
	Cursor string `json:"cursor,omitempty"`
}

// RunSavedViewResponse is one typed page produced by executing a Saved View's
// canonical definition through the Explore surface its grouping and
// presentation select. Exactly one of rows, groups, or files is populated,
// named by result_kind.
type RunSavedViewResponse struct {
	SavedView              SavedView               `json:"saved_view"`
	ResultKind             string                  `json:"result_kind" enum:"entries,groups,files"`
	Rows                   []query.EntryRow        `json:"rows,omitempty"`
	Groups                 []query.ExploreGroupRow `json:"groups,omitempty"`
	Files                  []query.ExploreFileFact `json:"files,omitempty"`
	TotalCount             *int64                  `json:"total_count,omitempty"`
	NextCursor             string                  `json:"next_cursor,omitempty"`
	CacheRevision          string                  `json:"cache_revision"`
	SearchProvenance       query.SearchProvenance  `json:"search_provenance"`
	CandidateSnapshotID    string                  `json:"candidate_snapshot_id,omitempty"`
	CandidatePoolSaturated bool                    `json:"candidate_pool_saturated,omitempty"`
	SearchDeletionScope    string                  `json:"search_deletion_scope,omitempty"`
}

func (s *Server) registerRunSavedViewRoute(api huma.API) {
	run := rawAPIV1Operation("runSavedView", http.MethodPost, "/saved-views/{id}/run",
		"Run a shared analytical Saved View through its canonical Explore definition")
	addSavedViewIDParameter(&run)
	run.RequestBody = jsonRequestBodyFor[RunSavedViewRequest](api)
	run.Responses = jsonResponsesFor[RunSavedViewResponse](api)
	addErrorResponses(api, run.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict)
	run.Responses[httpStatusKey(http.StatusServiceUnavailable)] = exploreUnavailableResponseFor(api)
	registerRawHumaRoute(api, run, s.handleRunSavedView)
}

// handleRunSavedView executes a Saved View by handing its canonical request to
// the same Explore handler the Web UI calls, so a view runs with exactly the
// validation, search resolution, and pagination that endpoint applies.
func (s *Server) handleRunSavedView(w http.ResponseWriter, r *http.Request) {
	if !s.requireSavedViewStore(w) {
		return
	}
	id, ok := savedViewID(w, r)
	if !ok {
		return
	}
	var request RunSavedViewRequest
	if !decodeExploreJSON(w, r, &request) {
		return
	}
	if request.Limit == 0 {
		request.Limit = runSavedViewMaxLimit
	}
	if request.Limit < 1 || request.Limit > runSavedViewMaxLimit {
		writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", runSavedViewMaxLimit))
		return
	}
	view, err := s.savedViewStore.GetSavedView(r.Context(), id)
	if err != nil {
		s.writeSavedViewError(w, err)
		return
	}
	prepared, err := prepareSavedView(*view)
	if err != nil {
		s.writeSavedViewError(w, err)
		return
	}
	state, predicate := prepared.state, prepared.predicate
	var page RunSavedViewResponse
	switch {
	case len(state.Grouping) > 0:
		// Grouping is a drill-down chain and the groups endpoint takes one
		// dimension per request, so a grouped view runs at its first level,
		// the same one the Web UI shows when it opens the view.
		page, ok = s.runSavedViewGroups(w, r, predicate, state.Grouping[0], request)
	case state.Presentation == explorecatalog.PresentationFiles:
		page, ok = s.runSavedViewFiles(w, r, predicate, request)
	default:
		page, ok = s.runSavedViewEntries(w, r, predicate, request)
	}
	if !ok {
		return
	}
	page.SavedView = savedViewResponse(view, nil)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, page)
}

type preparedSavedView struct {
	state     store.SavedViewStateEnvelope
	predicate ExploreHTTPRequest
}

// prepareSavedView owns API execution validation for both reads and writes.
// ExecutableState decodes and checks the stored schema and vocabulary once;
// Explore then checks predicate values and search rules. The store separately
// guards persistence. A caller displaying an incompatible view retains this
// error alongside the original JSON instead of decoding that JSON as current state.
func prepareSavedView(view store.SavedView) (preparedSavedView, error) {
	state, err := savedview.ExecutableState(view)
	if err != nil {
		return preparedSavedView{}, err
	}
	if (state.SearchMode == explorecatalog.SearchModeSemantic || state.SearchMode == explorecatalog.SearchModeHybrid) && strings.TrimSpace(state.Query) == "" {
		return preparedSavedView{}, fmt.Errorf("%w: semantic and hybrid exploration require free text", store.ErrSavedViewInvalidState)
	}
	prepared, err := prepareExplorePredicate(savedViewExplorePredicate(state))
	if err != nil {
		return preparedSavedView{}, fmt.Errorf("%w: %w", store.ErrSavedViewInvalidState, err)
	}
	if _, failure := validateExploreSearchDefinition(prepared.request, prepared.query.Context); failure != nil {
		return preparedSavedView{}, fmt.Errorf("%w: %s: %s", store.ErrSavedViewInvalidState, failure.code, failure.message)
	}
	return preparedSavedView{state: state, predicate: prepared.request}, nil
}

// savedViewExplorePredicate translates a validated v1 definition into the
// Explore predicate the Web UI would build when it opens the same view.
func savedViewExplorePredicate(state store.SavedViewStateEnvelope) ExploreHTTPRequest {
	predicate := ExploreHTTPRequest{Presentation: explorecatalog.PresentationTable}
	for _, filter := range state.Filters {
		predicate.Filters = append(predicate.Filters, ExploreFilter{
			Dimension: store.SavedViewFilterDimension(filter.Field),
			Values:    append([]string(nil), filter.Values...),
		})
	}
	if queryText := strings.TrimSpace(state.Query); queryText != "" {
		predicate.Query = queryText
		predicate.SearchMode = state.SearchMode
		if predicate.SearchMode == "" {
			predicate.SearchMode = explorecatalog.SearchModeFullText
		}
	}
	for _, sort := range state.Sort {
		predicate.Sort = append(predicate.Sort, ExploreSort{Field: sort.Field, Direction: sort.Direction})
	}
	return predicate
}

func (s *Server) runSavedViewEntries(
	w http.ResponseWriter, r *http.Request, predicate ExploreHTTPRequest, request RunSavedViewRequest,
) (RunSavedViewResponse, bool) {
	// Timeline is a client-side rendering of the same canonical entry rows.
	predicate.Limit = request.Limit
	predicate.Cursor = request.Cursor
	var result ExploreHTTPResponse
	if !s.dispatchSavedViewExplore(w, r, s.handleExplore, predicate, &result) {
		return RunSavedViewResponse{}, false
	}
	return RunSavedViewResponse{
		ResultKind: string(savedview.ResultEntries), Rows: result.Rows, TotalCount: result.TotalCount,
		NextCursor: result.NextCursor, CacheRevision: result.CacheRevision,
		SearchProvenance: result.SearchProvenance, CandidateSnapshotID: result.CandidateSnapshotID,
		CandidatePoolSaturated: result.CandidatePoolSaturated, SearchDeletionScope: result.SearchDeletionScope,
	}, true
}

func (s *Server) runSavedViewGroups(
	w http.ResponseWriter, r *http.Request, predicate ExploreHTTPRequest, dimension string, request RunSavedViewRequest,
) (RunSavedViewResponse, bool) {
	body := ExploreGroupsHTTPRequest{
		Filters: predicate.Filters, Query: predicate.Query, SearchMode: predicate.SearchMode,
		Grouping:     []ExploreGroupDimension{ExploreGroupDimension(dimension)},
		Presentation: explorecatalog.PresentationTable, Cursor: request.Cursor, Limit: request.Limit,
	}
	var result ExploreGroupsHTTPResponse
	if !s.dispatchSavedViewExplore(w, r, s.handleExploreGroups, body, &result) {
		return RunSavedViewResponse{}, false
	}
	total := result.TotalCount
	return RunSavedViewResponse{
		ResultKind: string(savedview.ResultGroups), Groups: result.Rows, TotalCount: &total,
		NextCursor: result.NextCursor, CacheRevision: result.CacheRevision,
		SearchProvenance: result.SearchProvenance, CandidateSnapshotID: result.CandidateSnapshotID,
		SearchDeletionScope: result.SearchDeletionScope,
	}, true
}

func (s *Server) runSavedViewFiles(
	w http.ResponseWriter, r *http.Request, predicate ExploreHTTPRequest, request RunSavedViewRequest,
) (RunSavedViewResponse, bool) {
	body := ExploreFilesHTTPRequest{Predicate: predicate, Cursor: request.Cursor, Limit: request.Limit}
	var result ExploreFilesHTTPResponse
	if !s.dispatchSavedViewExplore(w, r, s.handleExploreFiles, body, &result) {
		return RunSavedViewResponse{}, false
	}
	total := result.TotalCount
	return RunSavedViewResponse{
		ResultKind: string(savedview.ResultFiles), Files: result.Files, TotalCount: &total,
		NextCursor: result.NextCursor, CacheRevision: result.CacheRevision,
		SearchProvenance: result.SearchProvenance, CandidateSnapshotID: result.CandidateSnapshotID,
		SearchDeletionScope: result.SearchDeletionScope,
	}, true
}

// dispatchSavedViewExplore runs an Explore handler on a request body built
// from a Saved View and decodes its success payload into result. Any other
// status is relayed to the caller unchanged, so a Saved View run fails with
// the same error contract as the Explore endpoint it executed.
func (s *Server) dispatchSavedViewExplore(
	w http.ResponseWriter, r *http.Request, handler http.HandlerFunc, body any, result any,
) bool {
	encoded, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "saved_view_run_failed", "Saved View request could not be encoded")
		return false
	}
	internal := r.Clone(r.Context())
	internal.Body = io.NopCloser(bytes.NewReader(encoded))
	internal.ContentLength = int64(len(encoded))
	capture := &capturedResponse{header: http.Header{}, status: http.StatusOK}
	handler(capture, internal)
	if capture.status != http.StatusOK {
		copyCapturedResponse(w, capture)
		return false
	}
	if err := json.Unmarshal(capture.body.Bytes(), result); err != nil {
		writeError(w, http.StatusInternalServerError, "saved_view_run_failed", "Saved View result could not be decoded")
		return false
	}
	return true
}

// capturedResponse buffers one handler response so the Saved View run can
// wrap a success payload or relay an error verbatim.
type capturedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *capturedResponse) Header() http.Header { return c.header }

func (c *capturedResponse) WriteHeader(status int) { c.status = status }

func (c *capturedResponse) Write(data []byte) (int, error) {
	written, err := c.body.Write(data)
	if err != nil {
		return written, fmt.Errorf("buffer Saved View run response: %w", err)
	}
	return written, nil
}

func copyCapturedResponse(w http.ResponseWriter, capture *capturedResponse) {
	for name, values := range capture.header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(capture.status)
	_, _ = w.Write(capture.body.Bytes())
}
