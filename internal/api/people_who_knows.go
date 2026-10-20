package api

import (
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/whoknows"
)

func (s *Server) registerWhoKnowsRoutes(api huma.API) {
	op := rawAPIV1Operation("findPeopleWhoKnow", http.MethodPost, "/people/who-knows",
		"Find people who wrote about a topic")
	op.Description = "Ranks saved people, then unsaved contacts, by how many matching messages they wrote " +
		"and how recent those are. Each entry cites its messages. A topic nobody wrote about returns " +
		"empty lists with reason no_relevant_evidence."
	op.RequestBody = jsonRequestBodyFor[whoknows.Request](api)
	op.Responses = jsonResponsesFor[whoknows.Response](api)
	addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, op, s.handleWhoKnows)
}

func (s *Server) handleWhoKnows(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request whoknows.Request
	if !decodePersonRequest(w, r, &request) {
		return
	}
	if _, err := request.Resolve(); err != nil {
		s.writeWhoKnowsError(w, err)
		return
	}
	engine, _, cfg := s.vectorComponents()
	if engine == nil {
		s.writeVectorUnavailable(w)
		return
	}
	if !s.vectorSearchPreflight(r.Context(), w) {
		return
	}
	data, ok := s.store.(whoknows.DataReader)
	if !ok {
		s.writeWhoKnowsError(w, whoknows.ErrPersonScopeUnavailable)
		return
	}
	service := whoknows.Service{Retriever: engine, Data: data, BuildScope: cfg.Embed.Scope.BuildScope()}
	response, err := service.Find(r.Context(), request)
	if err != nil {
		s.writeWhoKnowsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) writeWhoKnowsError(w http.ResponseWriter, err error) {
	// An embedding timeout wraps context.DeadlineExceeded, so test it first.
	if errors.Is(err, vector.ErrEmbeddingTimeout) {
		writeError(w, http.StatusServiceUnavailable, "embedding_timeout",
			"the embedding endpoint did not respond in time; retry, or raise [vector.embeddings].timeout")
		return
	}
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, whoknows.ErrMissingFreeText):
		writeError(w, http.StatusBadRequest, "missing_free_text", err.Error())
	case errors.Is(err, whoknows.ErrInvalidParameter):
		writeError(w, http.StatusBadRequest, "invalid_parameter", err.Error())
	case errors.Is(err, vector.ErrIndexScopeMismatch):
		writeError(w, http.StatusBadRequest, "index_scope_mismatch", err.Error())
	case errors.Is(err, vector.ErrNotEnabled):
		writeError(w, http.StatusServiceUnavailable, "vector_not_enabled", "vector search is not configured")
	case errors.Is(err, vector.ErrIndexStale):
		writeError(w, http.StatusServiceUnavailable, "index_stale",
			"the vector index does not match configured embedding settings; align [vector.embed.scope] accounts for an existing account-scoped index, or run `msgvault embeddings build --full-rebuild`")
	case errors.Is(err, vector.ErrIndexBuilding):
		writeError(w, http.StatusServiceUnavailable, "index_building", "the initial vector index is still being built")
	case errors.Is(err, whoknows.ErrPersonScopeUnavailable):
		writeError(w, http.StatusServiceUnavailable, "person_scope_unavailable", err.Error())
	default:
		s.logger.Error("who-knows failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "who-knows failed")
	}
}
