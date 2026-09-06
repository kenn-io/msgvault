package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

const (
	defaultPersonNetworkDepth = 1
	minPersonNetworkDepth     = 1
	maxPersonNetworkDepth     = 3
)

// PersonNetworkStore is the narrow curated-network capability consumed by the
// HTTP route.
type PersonNetworkStore interface {
	GetPersonNetworkContext(ctx context.Context, personID int64, opts store.PersonNetworkOptions) (store.PersonNetwork, error)
}

func (s *Server) registerPersonNetworkRoutes(api huma.API) {
	get := rawAPIV1Operation("getPersonNetwork", http.MethodGet, "/people/{id}/network",
		"Get a bounded curated person network")
	get.Description = "Returns declared person relationships and employments only; archive-derived associations are excluded."
	addPersonIDParameter(&get)
	depth := queryIntegerParam("depth", "Breadth-first depth (default 1, minimum 1, maximum 3)")
	minimumDepth := float64(minPersonNetworkDepth)
	maximumDepth := float64(maxPersonNetworkDepth)
	depth.Schema.Default = defaultPersonNetworkDepth
	depth.Schema.Minimum = &minimumDepth
	depth.Schema.Maximum = &maximumDepth
	get.Parameters = append(get.Parameters,
		depth,
		queryBooleanParam("include_ended", "Include ended relationships and employment records"),
	)
	get.Responses = jsonResponsesFor[store.PersonNetwork](api)
	addErrorResponses(api, get.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, get, s.handleGetPersonNetwork)
}

func (s *Server) handleGetPersonNetwork(w http.ResponseWriter, r *http.Request) {
	networks, ok := s.personNetworkStore(w)
	if !ok {
		return
	}
	personID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	depth, ok := personNetworkDepth(w, r)
	if !ok {
		return
	}
	includeEnded, ok := relationshipBoolQuery(w, r, "include_ended")
	if !ok {
		return
	}
	graph, err := networks.GetPersonNetworkContext(r.Context(), personID,
		store.PersonNetworkOptions{Depth: depth, IncludeEnded: includeEnded})
	if err != nil {
		s.writePersonNetworkError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, graph)
}

func (s *Server) personNetworkStore(w http.ResponseWriter) (PersonNetworkStore, bool) {
	networks, ok := s.store.(PersonNetworkStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "person_network_unavailable", "Person network is unavailable")
	}
	return networks, ok
}

func personNetworkDepth(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("depth"))
	if raw == "" {
		return defaultPersonNetworkDepth, true
	}
	depth, err := strconv.Atoi(raw)
	if err != nil || depth < 1 || depth > 3 {
		writeError(w, http.StatusBadRequest, "invalid_depth", "depth must be between 1 and 3")
		return 0, false
	}
	return depth, true
}

func (s *Server) writePersonNetworkError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrPersonNetworkInvalid):
		writeError(w, http.StatusBadRequest, "invalid_depth", err.Error())
	case errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusNotFound, "person_profile_not_found", "Person profile not found")
	default:
		s.logger.Error("person network operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "person_network_failed", "Person network operation failed")
	}
}
