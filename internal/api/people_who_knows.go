package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/chunkmatch"
	"go.kenn.io/msgvault/internal/whoknows"
)

func (s *Server) registerWhoKnowsRoutes(api huma.API) {
	op := rawAPIV1Operation("findPeopleWhoKnow", http.MethodGet, "/people/who-knows", "Find people with archived topical evidence")
	op.Description = "Owner-only, read-only ranking by authored message evidence with cited messages and an explainable score breakdown."
	op.Parameters = []*huma.Param{queryStringParam("q", "Topic containing free text", true)}
	op.Parameters[0].Required = true
	for _, v := range []struct {
		name            string
		value, min, max int
	}{{"limit", 10, 1, 50}, {"evidence", 3, 0, 10}, {"candidates", 200, 50, 1000}, {"half_life_days", 180, 0, 3650}} {
		p := queryIntegerParam(v.name, "Bounded ranking parameter")
		p.Schema.Default = v.value
		p.Schema.Minimum = new(float64(v.min))
		p.Schema.Maximum = new(float64(v.max))
		op.Parameters = append(op.Parameters, p)
	}
	for _, v := range []struct {
		name  string
		value float64
	}{{"min_score", 0}, {"min_profile_score", .5}} {
		p := &huma.Param{Name: v.name, In: "query", Schema: &huma.Schema{Type: "number", Format: "double", Default: v.value}}
		if v.name == "min_profile_score" {
			p.Schema.Minimum = new(0.0)
			p.Schema.Maximum = new(1.0)
		}
		op.Parameters = append(op.Parameters, p)
	}
	for _, v := range []struct {
		name  string
		value bool
	}{{"include_owner", false}, {"include_unsaved", true}, {"include_organizations", true}, {"excerpts", false}} {
		p := queryBooleanParam(v.name, "Optional ranking output")
		p.Schema.Default = v.value
		op.Parameters = append(op.Parameters, p)
	}
	for _, name := range []string{"after", "before"} {
		op.Parameters = append(op.Parameters, queryStringParam(name, "YYYY-MM-DD or RFC3339; after inclusive, before exclusive for timestamps", false))
	}
	tie := queryStringParam("tiebreak", "Metric used after rounded score", false)
	tie.Schema.Enum = []any{"temperature", "last_contact", "none"}
	tie.Schema.Default = "temperature"
	op.Parameters = append(op.Parameters, tie)
	op.Responses = jsonResponsesFor[whoknows.Response](api)
	addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, op, s.handleWhoKnows)
}

func (s *Server) handleWhoKnows(w http.ResponseWriter, r *http.Request) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		s.writeWhoKnowsError(w, fmt.Errorf("%w: malformed query", whoknows.ErrInvalidParameter))
		return
	}
	topic, p, err := whoknows.ParseHTTPParams(values)
	if err != nil {
		s.writeWhoKnowsError(w, err)
		return
	}
	engine, backend, cfg := s.vectorComponents()
	if engine == nil {
		s.writeWhoKnowsError(w, vector.ErrNotEnabled)
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
	service := whoknows.Service{Retriever: engine, Data: data, KPerSignal: cfg.Search.KPerSignal, BuildScope: cfg.Embed.Scope.BuildScope(), PeopleEnabled: cfg.People.Enabled, CoverageReader: func(ctx context.Context) (vector.EmbeddingStatus, error) {
		status, _, err := s.loadEmbeddingStatus(ctx, 0)
		return status, err
	}}
	if profiles, ok := s.personSearchComponent().(whoknows.ProfileSearcher); ok {
		service.ProfileSearcher = profiles
	}
	if state := s.currentAnalyticsState(); state != nil && state.engine != nil {
		if reader, ok := state.engine.(interface {
			PeopleTemperatures(ctx context.Context, members map[int64][]int64) (map[int64]int, bool, error)
		}); ok {
			service.TemperatureReader = reader.PeopleTemperatures
		}
	}
	service.ExcerptBuilder = func(ctx context.Context, generation vector.GenerationID, queryVector []float32, messageID int64, minScore float64) (*whoknows.Excerpt, error) {
		scorer, ok := backend.(vector.ChunkScoringBackend)
		if !ok {
			return nil, errors.New("chunk scoring unavailable")
		}
		message, err := s.getMessage(ctx, messageID)
		if err != nil {
			return nil, err
		}
		if message == nil {
			return nil, errors.New("evidence message unavailable")
		}
		hits, err := scorer.ScoreMessageChunks(ctx, generation, messageID, queryVector)
		if err != nil {
			return nil, err
		}
		matches, _ := chunkmatch.Build(message.Subject, embeddingBodyText(message), cfg, hits, minScore, 1, hybridMatchSnippetLen)
		if len(matches) == 0 {
			return nil, nil //nolint:nilnil // No matching chunk is a valid absent excerpt.
		}
		match := matches[0]
		return &whoknows.Excerpt{Snippet: match.Snippet, CharOffset: match.CharOffset, Line: match.Line, Score: match.Score}, nil
	}
	response, err := service.Find(r.Context(), topic, p)
	if err != nil {
		s.writeWhoKnowsError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) writeWhoKnowsError(w http.ResponseWriter, err error) {
	if errors.Is(err, vector.ErrEmbeddingTimeout) {
		writeError(w, http.StatusServiceUnavailable, "embedding_timeout", err.Error())
		return
	}
	if s.writeIfContextError(w, err) {
		return
	}
	status, code := http.StatusServiceUnavailable, "semantic_search_unavailable"
	switch {
	case errors.Is(err, whoknows.ErrMissingFreeText):
		status, code = http.StatusBadRequest, "missing_free_text"
	case errors.Is(err, whoknows.ErrInvalidParameter):
		status, code = http.StatusBadRequest, "invalid_parameter"
	case errors.Is(err, vector.ErrNotEnabled):
		code = "vector_not_enabled"
	case errors.Is(err, vector.ErrIndexStale):
		code = "index_stale"
	case errors.Is(err, vector.ErrIndexBuilding):
		code = "index_building"
	case errors.Is(err, vector.ErrIndexScopeMismatch):
		status, code = http.StatusBadRequest, "index_scope_mismatch"
	case errors.Is(err, whoknows.ErrPersonScopeUnavailable):
		code = "person_scope_unavailable"
	}
	writeError(w, status, code, err.Error())
}
