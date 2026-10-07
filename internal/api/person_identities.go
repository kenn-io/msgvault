package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// PersonIdentityStore lists the archived participant identities currently
// bound to a durable person.
type PersonIdentityStore interface {
	ListPersonIdentitiesContext(ctx context.Context, personID int64) ([]PersonIdentity, error)
}

// PersonIdentity is one archived identity of a person. Only email addresses
// are supported draft recipients.
type PersonIdentity struct {
	Kind      string `json:"kind" doc:"Identity kind, such as email, phone, or a chat service"`
	Value     string `json:"value" doc:"Archived value; a supported email is ready to pass as a draft recipient"`
	Supported bool   `json:"supported" doc:"Whether the value can be a draft recipient"`
}

type PersonIdentitiesResponse struct {
	PersonID   int64            `json:"person_id"`
	Identities []PersonIdentity `json:"identities"`
}

func (s *Server) registerPersonIdentityRoutes(api huma.API) {
	list := rawAPIV1Operation("listPersonIdentities", http.MethodGet, "/people/{id}/identities",
		"List a durable person's archived identities")
	list.Description = "Lists the identities of the person's current participants. Email addresses are " +
		"supported draft recipients; phone numbers and chat identifiers are not. " +
		"Curated contact points and postal addresses are not listed."
	addPersonIDParameter(&list)
	list.Responses = jsonResponsesFor[PersonIdentitiesResponse](api)
	addErrorResponses(api, list.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListPersonIdentities)
}

func (s *Server) handleListPersonIdentities(w http.ResponseWriter, r *http.Request) {
	identities, ok := s.store.(PersonIdentityStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "persons_unavailable", "Person profiles are unavailable")
		return
	}
	id, ok := personProfileID(w, r)
	if !ok {
		return
	}
	rows, err := identities.ListPersonIdentitiesContext(r.Context(), id)
	if err != nil {
		s.writePersonError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, PersonIdentitiesResponse{PersonID: id, Identities: rows})
}
