package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
	vectordocument "go.kenn.io/msgvault/internal/vector/document"
)

const (
	documentSearchRequestsPerSecond = 1
	documentSearchRequestBurst      = 2
)

// DocumentSearchStore is the optional dedicated extracted-document search
// surface. It stays separate from MessageStore so lightweight API consumers
// do not have to implement document indexing.
type DocumentSearchStore interface {
	SearchDocuments(ctx context.Context, request store.DocumentSearchRequest) (store.DocumentSearchResponse, error)
}

// DocumentStatusStore is the optional scoped status surface for extracted
// documents. Keeping it separate lets the daemon expose the same read path as
// the CLI without expanding the core MessageStore contract.
type DocumentStatusStore interface {
	GetDocumentIndexStatusForScope(
		ctx context.Context,
		profileID string,
		extractionInputKey string,
		allowedMediaTypes []string,
		allowedMessageTypes []string,
	) (store.DocumentIndexStatus, error)
	GetActiveDocumentExtractionRebuild(
		ctx context.Context,
		profileID string,
		extractionInputKey string,
	) (store.DocumentExtractionRebuild, error)
	CountIncompleteDocumentExtractionRebuild(
		ctx context.Context,
		rebuild store.DocumentExtractionRebuild,
		allowedMediaTypes []string,
		allowedMessageTypes []string,
	) (int64, error)
}

// DocumentCurrentStatusScopeStore resolves the selected durable document
// profile without requiring the browser to receive or retain its private ID.
type DocumentCurrentStatusScopeStore interface {
	GetCurrentDocumentIndexStatusScope(ctx context.Context) (string, []string, error)
}

type DocumentVectorStatusStore interface {
	GetDocumentVectorTargetProfileID(ctx context.Context) (string, error)
	GetDocumentVectorOperationsStatus(ctx context.Context, configured store.DocumentVectorGenerationSpec, documentEgressFingerprint, queryEgressFingerprint string, generationID int64, afterToken string, limit int) (store.DocumentVectorOperationsStatus, error)
}

type DocumentVectorOperationsResponse struct {
	Enabled                              bool                                  `json:"enabled"`
	Configured                           bool                                  `json:"configured"`
	ScheduledRegistrationRequiresRestart bool                                  `json:"scheduled_registration_requires_restart,omitempty"`
	Status                               *store.DocumentVectorOperationsStatus `json:"status,omitempty"`
}

type documentOccurrenceStatusReconciler interface {
	ReconcileDocumentOccurrences(ctx context.Context) error
}

var _ DocumentSearchStore = (*store.Store)(nil)
var _ DocumentStatusStore = (*store.Store)(nil)
var _ DocumentCurrentStatusScopeStore = (*store.Store)(nil)

func (s *Server) registerDocumentSearchRoute(api huma.API) {
	registerAPIV1RawHumaJSONRouteWithErrors[store.DocumentSearchResponse](
		api, "searchDocuments", http.MethodGet, "/documents/search",
		"Search extracted document attachments",
		s.documentSearchGuard("document search", s.handleDocumentSearch),
		http.StatusBadRequest, http.StatusForbidden, http.StatusConflict,
		http.StatusNotFound, http.StatusTooManyRequests, http.StatusServiceUnavailable,
	)
	registerAPIV1RawHumaJSONRouteWithErrors[store.DocumentIndexStatusResponse](
		api, "getDocumentIndexStatus", http.MethodGet, "/documents/status",
		"Get extracted document index status",
		s.documentSearchGuard("document status", s.handleDocumentIndexStatus),
		http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests,
		http.StatusServiceUnavailable,
	)
	registerAPIV1RawHumaJSONRouteWithErrors[store.DocumentIndexStatusResponse](
		api, "getCurrentDocumentIndexStatus", http.MethodGet, "/documents/status/current",
		"Get extracted document index status for the selected durable profile",
		s.documentSearchGuard("current document status", s.handleCurrentDocumentIndexStatus),
		http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests,
		http.StatusServiceUnavailable,
	)
	registerAPIV1RawHumaJSONRouteWithErrors[DocumentVectorOperationsResponse](
		api, "getDocumentVectorStatus", http.MethodGet, "/documents/vectors/status",
		"Get document vector generation, consent, usage, and failure status",
		s.documentSearchGuard("document vector status", s.handleDocumentVectorStatus),
		http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests,
		http.StatusServiceUnavailable,
	)
}

func (s *Server) handleDocumentVectorStatus(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Vector.Enabled || !s.cfg.Attachments.Documents.Index.Embeddings.Enabled {
		writeJSON(w, http.StatusOK, DocumentVectorOperationsResponse{Enabled: false})
		return
	}
	statusStore, ok := s.store.(DocumentVectorStatusStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "document_vector_status_unavailable", "Document vector status is unavailable")
		return
	}
	generationID, _, err := queryInt64(r, "generation_id")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	limit, found, err := queryInt(r, "limit")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if !found {
		limit = 20
	}
	if limit < 1 || limit > 1000 {
		s.rejectBadParam(w, errors.New("limit must be between 1 and 1000"))
		return
	}
	target, err := statusStore.GetDocumentVectorTargetProfileID(r.Context())
	if errors.Is(err, store.ErrDocumentVectorInvalidGenerationState) {
		writeJSON(w, http.StatusOK, DocumentVectorOperationsResponse{Enabled: true, Configured: false})
		return
	}
	if err != nil {
		s.writeDocumentSearchError(w, err)
		return
	}
	generationFingerprint, err := vectordocument.Fingerprint(target, s.cfg.Vector)
	if err != nil {
		s.writeDocumentSearchError(w, err)
		return
	}
	spec := store.DocumentVectorGenerationSpec{
		Fingerprint: generationFingerprint, TargetExtractionProfileID: target,
		EmbeddingProfile: s.cfg.Attachments.Documents.Index.Embeddings.Profile,
		Model:            s.cfg.Vector.Embeddings.Model, Dimension: s.cfg.Vector.Embeddings.Dimension,
	}
	documentEgressFingerprint, err := vectordocument.EgressFingerprint(target, s.cfg.Vector)
	if err != nil {
		s.writeDocumentSearchError(w, err)
		return
	}
	queryEgressFingerprint, err := vectordocument.QueryEgressFingerprint(target, s.cfg.Vector)
	if err != nil {
		s.writeDocumentSearchError(w, err)
		return
	}
	status, err := statusStore.GetDocumentVectorOperationsStatus(r.Context(), spec, documentEgressFingerprint, queryEgressFingerprint, generationID, r.URL.Query().Get("after_token"), limit)
	if err != nil {
		s.writeDocumentSearchError(w, err)
		return
	}
	s.documentSearchMu.RLock()
	restartRequired := status.QueryConsent != nil && s.documentSearch == nil
	s.documentSearchMu.RUnlock()
	writeJSON(w, http.StatusOK, DocumentVectorOperationsResponse{
		Enabled: true, Configured: true, ScheduledRegistrationRequiresRestart: restartRequired, Status: &status,
	})
}

// documentSearchGuard protects the document reads that reconcile attachment
// state before querying. It runs inside requestSecurityMiddleware, so the
// effective authentication mode and origin are already available.
func (s *Server) documentSearchGuard(label string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.requestAuthentication(r).Mode == AuthModeLoopback &&
			s.crossOriginAmbientReadRequest(r) {
			writeError(w, http.StatusForbidden, "cross_origin_loopback",
				"Keyless loopback document requests must be same-origin; "+
					"configure an API key for cross-origin access")
			return
		}
		if !s.documentSearchRateLimiter.Allow(clientIP(r)) {
			writeRateLimitExceeded(w)
			return
		}
		if s.operationGate != nil {
			done, ok := beginGateWorkBounded(r.Context(), s.operationGate, label)
			if !ok {
				writeOperationGateBusy(w, s.operationGate)
				return
			}
			defer done()
		}
		next(w, r)
	}
}

func (s *Server) handleDocumentIndexStatus(w http.ResponseWriter, r *http.Request) {
	request, err := parseDocumentIndexStatusRequest(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	s.writeDocumentIndexStatus(w, r, request)
}

func (s *Server) handleCurrentDocumentIndexStatus(w http.ResponseWriter, r *http.Request) {
	request, err := s.currentDocumentIndexStatusRequest(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "document_status_scope_unavailable",
			"Current document status scope is unavailable")
		return
	}
	s.writeDocumentIndexStatus(w, r, request)
}

func (s *Server) writeDocumentIndexStatus(
	w http.ResponseWriter,
	r *http.Request,
	request store.DocumentIndexStatusRequest,
) {
	statusStore, ok := s.store.(DocumentStatusStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "document_status_unavailable",
			"Document attachment status is unavailable")
		return
	}
	if reconciler, ok := s.store.(documentOccurrenceStatusReconciler); ok {
		if err := reconciler.ReconcileDocumentOccurrences(r.Context()); err != nil {
			s.logger.Error("reconcile document status index", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Document status failed")
			return
		}
	}
	status, err := statusStore.GetDocumentIndexStatusForScope(
		r.Context(), request.ProfileID, request.ExtractionInputKey,
		request.AllowedMediaTypes, request.AllowedMessageTypes,
	)
	if err != nil {
		s.logger.Error("read document index status", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Document status failed")
		return
	}
	response := store.DocumentIndexStatusResponse{Status: status}
	active, err := statusStore.GetActiveDocumentExtractionRebuild(
		r.Context(), request.ProfileID, request.ExtractionInputKey,
	)
	if err == nil {
		remaining, countErr := statusStore.CountIncompleteDocumentExtractionRebuild(
			r.Context(), active, request.AllowedMediaTypes, request.AllowedMessageTypes,
		)
		if countErr != nil {
			s.logger.Error("count document rebuild status", "error", countErr)
			writeError(w, http.StatusInternalServerError, "internal_error", "Document status failed")
			return
		}
		response.ActiveRebuild = &store.DocumentIndexRebuildStatus{
			SnapshotOwners: active.SnapshotOwners, RemainingOwners: remaining,
		}
	} else if !errors.Is(err, store.ErrDocumentExtractionRebuildMissing) {
		s.logger.Error("read document rebuild status", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Document status failed")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) currentDocumentIndexStatusRequest(ctx context.Context) (store.DocumentIndexStatusRequest, error) {
	resolver, ok := s.store.(DocumentCurrentStatusScopeStore)
	if !ok {
		return store.DocumentIndexStatusRequest{}, store.ErrDocumentIndexStatusScopeUnavailable
	}
	profileID, mediaTypes, err := resolver.GetCurrentDocumentIndexStatusScope(ctx)
	if err != nil {
		return store.DocumentIndexStatusRequest{}, err
	}
	return store.DocumentIndexStatusRequest{
		ProfileID: profileID, ExtractionInputKey: "original",
		AllowedMediaTypes:   mediaTypes,
		AllowedMessageTypes: slices.Clone(s.cfg.Attachments.Documents.Scope.MessageTypes),
	}, nil
}

func parseDocumentIndexStatusRequest(r *http.Request) (store.DocumentIndexStatusRequest, error) {
	request := store.DocumentIndexStatusRequest{
		ProfileID:           r.URL.Query().Get("profile_id"),
		ExtractionInputKey:  r.URL.Query().Get("input_key"),
		AllowedMediaTypes:   append([]string(nil), r.URL.Query()["media_type"]...),
		AllowedMessageTypes: append([]string(nil), r.URL.Query()["message_type"]...),
	}
	if request.ProfileID == "" || request.ExtractionInputKey == "" || len(request.AllowedMediaTypes) == 0 {
		return request, errors.New("profile_id, input_key, and at least one media_type are required")
	}
	return request, nil
}

func (s *Server) handleDocumentSearch(w http.ResponseWriter, r *http.Request) {
	searcher, ok := s.store.(DocumentSearchStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "document_search_unavailable",
			"Document attachment search is unavailable")
		return
	}
	request, err := parseDocumentSearchRequest(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if request.PersonID > 0 || request.ParticipantID > 0 {
		reference := resolver.Reference{Kind: resolver.ReferencePerson, ID: request.PersonID}
		if request.ParticipantID > 0 {
			reference = resolver.Reference{Kind: resolver.ReferenceParticipant, ID: request.ParticipantID}
		}
		resolved, resolveErr := resolver.Resolve(r.Context(), s.store, reference, request.Directions)
		if resolveErr != nil {
			s.writePersonScopeError(w, reference, resolveErr, "document")
			return
		}
		request.Person = &resolved.Scope
	}
	s.documentSearchMu.RLock()
	service := s.documentSearch
	s.documentSearchMu.RUnlock()
	var response store.DocumentSearchResponse
	if service != nil {
		if reconciler, ok := s.store.(documentOccurrenceStatusReconciler); ok {
			if err := reconciler.ReconcileDocumentOccurrences(r.Context()); err != nil {
				s.writeDocumentSearchError(w, err)
				return
			}
		}
		response, err = service.Search(r.Context(), request)
	} else if request.SearchMode == string(vectordocument.SearchModeSemantic) || request.SearchMode == string(vectordocument.SearchModeHybrid) {
		err = vectordocument.ErrSemanticSearchUnavailable
	} else {
		response, err = searcher.SearchDocuments(r.Context(), request)
	}
	if err != nil {
		s.writeDocumentSearchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) writeDocumentSearchError(w http.ResponseWriter, err error) {
	switch {
	case s.writeIfContextError(w, err):
		return
	case errors.Is(err, store.ErrDocumentSearchInvalidCursor):
		writeError(w, http.StatusBadRequest, "invalid_cursor", "The document search cursor is invalid")
	case errors.Is(err, store.ErrDocumentSearchCursorStale):
		writeError(w, http.StatusConflict, "document_index_changed",
			"The document index changed; restart pagination")
	case errors.Is(err, store.ErrDocumentSearchInvalidRequest):
		writeError(w, http.StatusBadRequest, "invalid_document_search", err.Error())
	case errors.Is(err, store.ErrDocumentSearchUnavailable):
		writeError(w, http.StatusServiceUnavailable, "document_search_unavailable",
			"Document search requires full-text search support")
	case errors.Is(err, vectordocument.ErrSemanticSearchUnavailable):
		writeError(w, http.StatusServiceUnavailable, "semantic_search_unavailable",
			"Semantic document search is unavailable")
	default:
		s.logger.Error("document search failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Document search failed")
	}
}

func parseDocumentSearchRequest(r *http.Request) (store.DocumentSearchRequest, error) {
	request := store.DocumentSearchRequest{
		Query: r.URL.Query().Get("q"), Cursor: r.URL.Query().Get("cursor"),
		SearchMode: r.URL.Query().Get("mode"),
	}
	if request.SearchMode != "" {
		mode, err := vectordocument.ParseSearchMode(request.SearchMode)
		if err != nil {
			return request, err
		}
		request.SearchMode = string(mode)
	}
	var err error
	request.SourceIDs, _, err = queryInt64s(r, "source_id")
	if err != nil {
		return request, err
	}
	for _, raw := range r.URL.Query()["message_type"] {
		for value := range strings.SplitSeq(raw, ",") {
			request.MessageTypes = append(request.MessageTypes, strings.TrimSpace(value))
		}
	}
	if request.PageSize, _, err = queryInt(r, "limit"); err != nil {
		return request, err
	}
	var candidateFound bool
	if request.CandidateLimit, candidateFound, err = queryInt(r, "candidate_limit"); err != nil {
		return request, err
	}
	maxCandidateLimit := store.MaxLexicalDocumentSearchCandidateLimit
	if request.SearchMode == string(vectordocument.SearchModeSemantic) ||
		request.SearchMode == string(vectordocument.SearchModeHybrid) {
		maxCandidateLimit = store.MaxDocumentSearchCandidateLimit
	}
	if candidateFound && (request.CandidateLimit < 1 || request.CandidateLimit > maxCandidateLimit) {
		return request, fmt.Errorf("candidate_limit must be between 1 and %d for this mode", maxCandidateLimit)
	}
	if request.AttachmentID, _, err = queryInt64(r, "attachment_id"); err != nil {
		return request, err
	}
	if request.MessageID, _, err = queryInt64(r, "message_id"); err != nil {
		return request, err
	}
	var personPresent bool
	if request.PersonID, personPresent, err = queryInt64(r, "person_id"); err != nil {
		return request, err
	}
	if personPresent && request.PersonID <= 0 {
		return request, errors.New("person_id must be a positive integer")
	}
	var participantPresent bool
	if request.ParticipantID, participantPresent, err = queryInt64(r, "participant_id"); err != nil {
		return request, err
	}
	if participantPresent && request.ParticipantID <= 0 {
		return request, errors.New("participant_id must be a positive integer")
	}
	if request.PersonID > 0 && request.ParticipantID > 0 {
		return request, errors.New("person_id and participant_id are mutually exclusive")
	}
	for _, raw := range r.URL.Query()["direction"] {
		for value := range strings.SplitSeq(raw, ",") {
			request.Directions = append(request.Directions, personscope.Direction(strings.TrimSpace(value)))
		}
	}
	if len(request.Directions) > 0 && request.PersonID == 0 && request.ParticipantID == 0 {
		return request, errors.New("direction requires person_id or participant_id")
	}
	if _, _, err = resolver.NormalizeDirections(request.Directions); err != nil {
		return request, err
	}
	if value, ok, dateErr := queryDate(r, "after"); dateErr != nil {
		return request, dateErr
	} else if ok {
		request.After = &value
	}
	if value, ok, dateErr := queryDate(r, "before"); dateErr != nil {
		return request, dateErr
	} else if ok {
		request.Before = &value
	}
	if request.After != nil && request.Before != nil && !request.After.Before(*request.Before) {
		return request, errors.New("after must be before before")
	}
	return request, nil
}

// SetDocumentSearchService installs semantic document retrieval after the
// optional vector runtime is ready. Before installation auto/lexical remain
// available and explicit semantic/hybrid requests return a stable 503.
func (s *Server) SetDocumentSearchService(service *vectordocument.SearchService) {
	s.documentSearchMu.Lock()
	s.documentSearch = service
	s.documentSearchMu.Unlock()
}
