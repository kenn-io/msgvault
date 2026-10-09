package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

// ContactRouteStore keeps discovery separate from identity mutation capabilities.
type ContactRouteStore interface {
	FindContactCandidatesContext(ctx context.Context, query store.ContactCandidateQuery) (*store.ContactCandidatePage, error)
	GetPersonMessagingRoutesContext(ctx context.Context, query store.PersonMessagingRouteQuery) (*store.PersonMessagingRoutesPage, error)
}

var _ ContactRouteStore = (*store.Store)(nil)

func (s *Server) registerContactRouteRoutes(api huma.API) {
	candidates := rawAPIV1Operation("findContactCandidates", http.MethodGet, "/people/contact-candidates", "Find durable people by saved or archived names without selecting a duplicate")
	candidates.Parameters = []*huma.Param{contactStringParam("query", true, "Literal name tokens; all tokens must match. 1..256 UTF-8 bytes, at most 16 tokens. Wildcards are literal."), contactIntegerParam("limit", 1, 100), contactIntegerParam("after_id", 0, 0)}
	candidates.Responses = jsonResponsesFor[store.ContactCandidatePage](api)
	addErrorResponses(api, candidates.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, candidates, s.handleFindContactCandidates)

	routes := rawAPIV1Operation("getPersonMessagingRoutes", http.MethodGet, "/people/messaging-routes", "Read one person's archived messaging routes and contact evidence by stable UID")
	routes.Parameters = []*huma.Param{contactStringParam("person_uid", true, "Canonical or retired person UID. Tombstones return 410; unknown UIDs return 404."), contactStringParam("network", false, "Canonical lowercase bridge/service slug; unknown networks remain visible as unresolved."), contactIntegerParam("source_id", 0, 0), contactIntegerParam("limit", 1, 100)}
	for _, name := range []string{"after_conversation_id", "after_contact_point_id", "after_observation_id", "after_suggestion_id"} {
		routes.Parameters = append(routes.Parameters, contactIntegerParam(name, 0, 0))
	}
	routes.Responses = jsonResponsesFor[store.PersonMessagingRoutesPage](api)
	addErrorResponses(api, routes.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, routes, s.handleGetPersonMessagingRoutes)
}

func contactStringParam(name string, required bool, description string) *huma.Param {
	return &huma.Param{Name: name, In: "query", Required: required, Description: description, Schema: &huma.Schema{Type: huma.TypeString}}
}

func contactIntegerParam(name string, minimum, maximum float64) *huma.Param {
	schema := &huma.Schema{Type: huma.TypeInteger, Format: formatInt64, Minimum: &minimum}
	if maximum > 0 {
		schema.Maximum = &maximum
		schema.Default = 20
	}
	return &huma.Param{Name: name, In: "query", Schema: schema}
}

// Reject malformed and repeated query values instead of silently defaulting.
func contactQueryInteger(r *http.Request, name string, minimum, maximum int64) (int64, error) {
	values, present := r.URL.Query()[name]
	if !present {
		return 0, nil
	}
	if len(values) != 1 {
		return 0, store.ErrInvalidContactLookup
	}
	value, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || value < minimum || maximum > 0 && value > maximum {
		return 0, store.ErrInvalidContactLookup
	}
	return value, nil
}

func contactQueryString(r *http.Request, name string) (string, error) {
	values, present := r.URL.Query()[name]
	if !present {
		return "", nil
	}
	if len(values) != 1 {
		return "", store.ErrInvalidContactLookup
	}
	return values[0], nil
}

func (s *Server) handleFindContactCandidates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	name, err := contactQueryString(r, "query")
	if err != nil {
		s.writeContactLookupError(w, err)
		return
	}
	limit, err := contactQueryInteger(r, "limit", 1, 100)
	if err != nil {
		s.writeContactLookupError(w, err)
		return
	}
	after, err := contactQueryInteger(r, "after_id", 0, 0)
	if err != nil {
		s.writeContactLookupError(w, err)
		return
	}
	query := store.ContactCandidateQuery{Query: name, Limit: int(limit), AfterID: after}
	if err := store.ValidateContactCandidateQuery(query); err != nil {
		s.writeContactLookupError(w, err)
		return
	}
	backend, ok := s.store.(ContactRouteStore)
	if !ok {
		s.writeContactLookupError(w, errors.New("contact route backend unavailable"))
		return
	}
	page, err := backend.FindContactCandidatesContext(r.Context(), query)
	if err != nil {
		s.writeContactLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleGetPersonMessagingRoutes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	personUID, err := contactQueryString(r, "person_uid")
	if err != nil {
		s.writeContactLookupError(w, err)
		return
	}
	network, err := contactQueryString(r, "network")
	if err != nil {
		s.writeContactLookupError(w, err)
		return
	}
	query := store.PersonMessagingRouteQuery{PersonUID: personUID, Network: network}
	limit, err := contactQueryInteger(r, "limit", 1, 100)
	if err != nil {
		s.writeContactLookupError(w, err)
		return
	}
	query.Limit = int(limit)
	for _, field := range []struct {
		name        string
		destination *int64
	}{
		{"source_id", &query.SourceID}, {"after_conversation_id", &query.AfterConversationID}, {"after_contact_point_id", &query.AfterContactPointID}, {"after_observation_id", &query.AfterObservationID}, {"after_suggestion_id", &query.AfterSuggestionID},
	} {
		value, err := contactQueryInteger(r, field.name, 0, 0)
		if err != nil {
			s.writeContactLookupError(w, err)
			return
		}
		*field.destination = value
	}
	if err := store.ValidatePersonMessagingRouteQuery(query); err != nil {
		s.writeContactLookupError(w, err)
		return
	}
	backend, ok := s.store.(ContactRouteStore)
	if !ok {
		s.writeContactLookupError(w, errors.New("contact route backend unavailable"))
		return
	}
	page, err := backend.GetPersonMessagingRoutesContext(r.Context(), query)
	if err != nil {
		s.writeContactLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) writeContactLookupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidContactLookup):
		writeError(w, http.StatusBadRequest, "invalid_contact_lookup", "Invalid contact lookup parameters")
	case errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusNotFound, "person_not_found", "Person UID not found")
	case errors.Is(err, store.ErrPersonUIDGone):
		writeError(w, http.StatusGone, "person_uid_gone", "Person UID retired without a surviving person")
	default:
		s.logger.Error("contact lookup failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "contact_lookup_unavailable", "Contact lookup is unavailable")
	}
}
