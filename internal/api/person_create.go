package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

// StandalonePersonStore creates curated people independently of participants.
type StandalonePersonStore interface {
	CreateStandalonePersonContext(ctx context.Context, input store.PersonCreateInput) (*store.Person, error)
}

func (s *Server) registerStandalonePersonRoute(api huma.API) {
	op := rawAPIV1Operation("createStandalonePerson", http.MethodPost, "/people/create",
		"Create a person without message participants")
	op.Description = "Creates a durable profile with a new vCard UID and user-curated contact data. " +
		"Refuses contacts already present on a person or observed participant cluster. " +
		"A title requires an organization. Does not publish to CardDAV."
	op.RequestBody = jsonRequestBodyFor[store.PersonCreateInput](api)
	op.Responses = jsonResponsesFor[store.Person](api, http.StatusCreated)
	addPersonETagHeader(op.Responses[httpStatusKey(http.StatusCreated)])
	addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, op, s.handleCreateStandalonePerson)
}

func (s *Server) handleCreateStandalonePerson(w http.ResponseWriter, r *http.Request) {
	creator, ok := s.store.(StandalonePersonStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "person_profiles_unavailable", "Person creation is unavailable")
		return
	}
	var input store.PersonCreateInput
	if !decodePersonRequest(w, r, &input) {
		return
	}
	person, err := creator.CreateStandalonePersonContext(r.Context(), input)
	switch {
	case errors.Is(err, store.ErrPersonCreateInvalid):
		writeError(w, http.StatusBadRequest, "invalid_person", err.Error())
	case errors.Is(err, store.ErrPersonContactExists):
		writeError(w, http.StatusConflict, "person_contact_exists", err.Error())
	case err != nil:
		s.writePersonError(w, err)
	default:
		writePerson(w, http.StatusCreated, person)
	}
}
