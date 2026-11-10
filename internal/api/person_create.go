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
		"With if_exists=return, returns a single matching person without updating it; otherwise refuses existing contacts. " +
		"A title requires an organization. Does not publish to CardDAV."
	op.RequestBody = jsonRequestBodyFor[store.PersonCreateInput](api)
	op.Responses = jsonResponsesFor[store.Person](api, http.StatusCreated, http.StatusOK)
	addPersonETagHeader(op.Responses[httpStatusKey(http.StatusOK)])
	addPersonETagHeader(op.Responses[httpStatusKey(http.StatusCreated)])
	addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable)
	op.Responses[httpStatusKey(http.StatusConflict)] = jsonResponsesFor[PersonCreateErrorResponse](api, http.StatusConflict)[httpStatusKey(http.StatusConflict)]
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
	var ambiguous *store.PersonAmbiguousError
	var participant *store.PersonMatchesParticipantError
	switch {
	case errors.Is(err, store.ErrPersonCreateInvalid):
		writeError(w, http.StatusBadRequest, "invalid_person", err.Error())
	case errors.As(err, &ambiguous):
		writeJSON(w, http.StatusConflict, PersonCreateErrorResponse{Error: "person_ambiguous", Message: err.Error(), Details: ambiguous})
	case errors.As(err, &participant):
		writeJSON(w, http.StatusConflict, PersonCreateErrorResponse{Error: "person_matches_participant", Message: err.Error(), Details: participant})
	case errors.Is(err, store.ErrPersonContactExists):
		writeError(w, http.StatusConflict, "person_contact_exists", err.Error())
	case err != nil:
		s.writePersonError(w, err)
	default:
		status := http.StatusCreated
		if person.Ensure != nil && person.Ensure.Status == store.PersonEnsureMatchedExisting {
			status = http.StatusOK
		}
		writePerson(w, status, person)
	}
}

// PersonCreateErrorResponse preserves the existing error body and adds
// resolution details only for return-mode conflicts.
type PersonCreateErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
	Details any    `json:"details,omitempty"`
}
