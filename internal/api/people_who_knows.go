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
		"and how recent those are. Each entry cites its messages. When both lists are empty, reason is " +
		"no_relevant_evidence, or search_limit when the closest messages all came from the owner or " +
		"automated senders and the search hit its limit."
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
	engine, backend, cfg := s.vectorComponents()
	if engine == nil {
		s.writeVectorUnavailable(w)
		return
	}
	if !s.vectorSearchPreflight(r.Context(), w) {
		return
	}
	data, ok := s.store.(whoknows.DataReader)
	scorer, scorerOK := backend.(whoknows.ChunkScorer)
	if !ok || !scorerOK {
		s.writeWhoKnowsError(w, whoknows.ErrChunkScoringUnavailable)
		return
	}
	service := whoknows.Service{Retriever: engine, Scorer: scorer, Data: data, BuildScope: cfg.Embed.Scope.BuildScope()}
	response, err := service.Find(r.Context(), request)
	if err != nil {
		s.writeWhoKnowsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) writeWhoKnowsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, whoknows.ErrMissingFreeText):
		writeError(w, http.StatusBadRequest, "missing_free_text", err.Error())
		return
	case errors.Is(err, whoknows.ErrInvalidParameter):
		writeError(w, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	case errors.Is(err, whoknows.ErrChunkScoringUnavailable):
		writeError(w, http.StatusServiceUnavailable, "chunk_scoring_unavailable", err.Error())
		return
	}
	// An embedding timeout wraps context.DeadlineExceeded, so keep it out of
	// the generic context mapping.
	if !errors.Is(err, vector.ErrEmbeddingTimeout) && s.writeIfContextError(w, err) {
		return
	}
	s.writeVectorSearchError(w, err, "who-knows")
}
