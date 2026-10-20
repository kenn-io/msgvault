package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/whoknows"
)

func (s *Server) registerWhoKnowsRoutes(api huma.API) {
	op := rawAPIV1Operation("findPeopleWhoKnow", http.MethodGet, "/people/who-knows",
		"Find people who wrote about a topic")
	op.Description = "Ranks saved people, then unsaved contacts, by how many matching messages they wrote " +
		"and how recent those are. Each entry cites its messages. limits lists each way the search came up " +
		"short: pool_full when more messages may match beyond the search pool, partial_index when the " +
		"vector index leaves out some message types or accounts or has not embedded a message yet, and " +
		"declared_truncated when declared topics were searched only in part. Both lists empty with no " +
		"limits means nobody wrote about the topic. " +
		"A message's From address is not a verified identity unless its citation has sender_authenticated."
	op.Parameters = []*huma.Param{
		queryStringParam("topic", "Plain words; search operators are refused", true),
		queryIntegerParam("limit", "Saved people to return, 1-50; default 10"),
		queryIntegerParam("evidence", "Citations per person, 1-10; default 3"),
		queryNumberParam("min_score", "Lowest cosine similarity a message needs to count, above 0 and at most 1; default 0.5"),
		queryStringParam("after", "Messages at or after this time (RFC3339 or YYYY-MM-DD)", false),
		queryStringParam("before", "Messages before this time (RFC3339 or YYYY-MM-DD)", false),
	}
	op.Responses = jsonResponsesFor[whoknows.Response](api)
	addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, op, s.handleWhoKnows)
}

func whoKnowsRequest(r *http.Request) (whoknows.Request, error) {
	request := whoknows.Request{Topic: r.URL.Query().Get("topic")}
	for _, field := range []struct {
		name   string
		target **int
	}{{"limit", &request.Limit}, {"evidence", &request.Evidence}} {
		value, ok, err := queryInt(r, field.name)
		if err != nil {
			return request, err
		}
		if ok {
			*field.target = &value
		}
	}
	minScore, ok, err := queryFloat(r, "min_score")
	if err != nil {
		return request, err
	}
	if ok {
		request.MinScore = &minScore
	}
	for _, field := range []struct {
		name   string
		target **time.Time
	}{{"after", &request.After}, {"before", &request.Before}} {
		value, ok, err := queryDate(r, field.name)
		if err != nil {
			return request, err
		}
		if ok {
			*field.target = &value
		}
	}
	return request, nil
}

func (s *Server) handleWhoKnows(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := whoKnowsRequest(r)
	if err != nil {
		s.rejectBadParam(w, err)
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
	// The archive store always has the who-knows reads.
	data, _ := s.store.(whoknows.DataReader)
	service := whoknows.Service{
		Retriever: engine, Data: data, Index: backend, BuildScope: cfg.Embed.Scope.BuildScope(),
	}
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
	}
	// An embedding timeout wraps context.DeadlineExceeded, so keep it out of
	// the generic context mapping.
	if !errors.Is(err, vector.ErrEmbeddingTimeout) && s.writeIfContextError(w, err) {
		return
	}
	s.writeVectorSearchError(w, err, "who-knows")
}
