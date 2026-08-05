package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

const personsPath = "/api/v1/persons"

// PersonProfileStore is the feature-local capability for durable curated
// people. The daemon adapter passes these calls directly to *store.Store.
type PersonProfileStore interface {
	CreatePersonFromParticipantContext(ctx context.Context, participantID int64) (*store.Person, bool, error)
	GetPersonContext(ctx context.Context, id int64) (*store.Person, error)
	ListPersonsContext(ctx context.Context) ([]store.Person, error)
	UpdatePersonDisplayNameContext(
		ctx context.Context, id, expectedRevision int64, displayName *string,
	) (*store.Person, error)
	DeletePersonContext(ctx context.Context, id, expectedRevision int64) error
	PersonForParticipantsContext(ctx context.Context, participantIDs []int64) (*store.Person, error)
}

type CreatePersonRequest struct {
	ParticipantID int64 `json:"participant_id"`
}

type PatchPersonRequest struct {
	DisplayName *string `json:"display_name" nullable:"true"`
}

type PersonsResponse struct {
	Persons []store.Person `json:"persons"`
}

func (s *Server) registerPersonProfileRoutes(api huma.API) {
	list := rawAPIV1Operation("listPersons", http.MethodGet, "/persons", "List durable person profiles")
	list.Description = "Durable persons are curated profiles; /api/v1/people exposes derived analytics groupings. " +
		"The listing is deliberately unpaginated: persons exist only through explicit promotion, so the set stays small."
	list.Responses = jsonResponsesFor[PersonsResponse](api)
	addErrorResponses(api, list.Responses, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListPersons)

	create := rawAPIV1Operation("createPerson", http.MethodPost, "/persons", "Promote a participant cluster to a durable person")
	create.Description = "Returns 201 when a new person is created, or 200 when the cluster is already " +
		"represented by a person (idempotent re-promotion, which also binds any unbound cluster members)."
	create.RequestBody = jsonRequestBodyFor[CreatePersonRequest](api)
	create.Responses = jsonResponsesFor[store.Person](api, http.StatusOK, http.StatusCreated)
	addPersonETagHeader(create.Responses[httpStatusKey(http.StatusOK)])
	addPersonETagHeader(create.Responses[httpStatusKey(http.StatusCreated)])
	addErrorResponses(api, create.Responses, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, create, s.handleCreatePerson)

	get := rawAPIV1Operation("getPersonProfile", http.MethodGet, "/persons/{id}", "Get a durable person profile")
	addPersonIDParameter(&get)
	get.Responses = jsonResponsesFor[store.Person](api)
	addPersonETagHeader(get.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, get.Responses, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, get, s.handleGetPersonProfile)

	patch := rawAPIV1Operation("patchPerson", http.MethodPatch, "/persons/{id}", "Update a durable person's display name")
	addPersonIDParameter(&patch)
	addPersonIfMatchParameter(&patch)
	patch.RequestBody = jsonRequestBodyFor[PatchPersonRequest](api)
	patch.Responses = jsonResponsesFor[store.Person](api)
	addPersonETagHeader(patch.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, patch.Responses, http.StatusConflict, http.StatusNotFound,
		http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, patch, s.handlePatchPerson)

	remove := rawAPIV1Operation("deletePerson", http.MethodDelete, "/persons/{id}", "Delete a durable person profile")
	remove.Description = "Deletion is permanent: the person's participant bindings are removed and its vCard UID " +
		"is retired forever. Re-promoting the same cluster afterwards creates a new person with a new UID."
	addPersonIDParameter(&remove)
	addPersonIfMatchParameter(&remove)
	remove.Responses = rawHumaResponses(http.StatusNoContent)
	remove.Responses["default"] = errorResponseFor(api)
	addErrorResponses(api, remove.Responses, http.StatusBadRequest, http.StatusUnauthorized,
		http.StatusConflict, http.StatusNotFound, http.StatusPreconditionRequired,
		http.StatusInternalServerError, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, remove, s.handleDeletePerson)
}

func (s *Server) handleCreatePerson(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileStore(w)
	if !ok {
		return
	}
	var request CreatePersonRequest
	if !decodePersonRequest(w, r, &request) {
		return
	}
	if request.ParticipantID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_participant_id", "participant_id must be a positive integer")
		return
	}
	person, created, err := profiles.CreatePersonFromParticipantContext(r.Context(), request.ParticipantID)
	if err != nil {
		s.writePersonError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writePerson(w, status, person)
}

func (s *Server) handleDeletePerson(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileStore(w)
	if !ok {
		return
	}
	id, ok := personProfileID(w, r)
	if !ok {
		return
	}
	revision, ok := personIfMatch(w, r, id)
	if !ok {
		return
	}
	if err := profiles.DeletePersonContext(r.Context(), id, revision); err != nil {
		s.writePersonError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetPersonProfile(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileStore(w)
	if !ok {
		return
	}
	id, ok := personProfileID(w, r)
	if !ok {
		return
	}
	person, err := profiles.GetPersonContext(r.Context(), id)
	if err != nil {
		s.writePersonError(w, err)
		return
	}
	writePerson(w, http.StatusOK, person)
}

func (s *Server) handleListPersons(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileStore(w)
	if !ok {
		return
	}
	persons, err := profiles.ListPersonsContext(r.Context())
	if err != nil {
		s.writePersonError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, PersonsResponse{Persons: persons})
}

func (s *Server) handlePatchPerson(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileStore(w)
	if !ok {
		return
	}
	id, ok := personProfileID(w, r)
	if !ok {
		return
	}
	revision, ok := personIfMatch(w, r, id)
	if !ok {
		return
	}
	var request PatchPersonRequest
	fields, ok := decodePersonRequestFields(w, r, &request)
	if !ok {
		return
	}
	raw, present := fields["display_name"]
	if !present {
		writeError(w, http.StatusBadRequest, "bad_request", "display_name is required")
		return
	}
	displayName := request.DisplayName
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		displayName = nil
	}
	person, err := profiles.UpdatePersonDisplayNameContext(r.Context(), id, revision, displayName)
	if err != nil {
		s.writePersonError(w, err)
		return
	}
	writePerson(w, http.StatusOK, person)
}

func (s *Server) personProfileStore(w http.ResponseWriter) (PersonProfileStore, bool) {
	profiles, ok := s.store.(PersonProfileStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "persons_unavailable", "Person profiles are unavailable")
	}
	return profiles, ok
}

func (s *Server) writePersonError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusNotFound, "person_profile_not_found", "Person profile not found")
	case errors.Is(err, store.ErrPersonRevisionConflict):
		writeError(w, http.StatusConflict, "person_revision_conflict", "Person profile changed; reload and retry")
	case errors.Is(err, store.ErrPersonBindingConflict):
		writeError(w, http.StatusConflict, "person_binding_conflict", "The identity clusters belong to different person profiles")
	case errors.Is(err, store.ErrPersonReferenced):
		writeError(w, http.StatusConflict, "person_referenced",
			"Another profile still references this person")
	case errors.Is(err, store.ErrParticipantNotFound), errors.Is(err, store.ErrInvalidParticipantID):
		writeError(w, http.StatusBadRequest, "invalid_participant_id", err.Error())
	default:
		s.logger.Error("person profile operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "person_profile_failed", "Person profile operation failed")
	}
}

func writePerson(w http.ResponseWriter, status int, person *store.Person) {
	w.Header().Set("ETag", personETag(*person))
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusCreated {
		w.Header().Set("Location", personsPath+"/"+strconv.FormatInt(person.ID, 10))
	}
	writeJSON(w, status, person)
}

func addPersonIDParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{
		Name: "id", In: "path", Required: true, Description: "Durable person ID",
		Schema: &huma.Schema{Type: huma.TypeInteger, Format: "int64"},
	})
}

func addPersonIfMatchParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{
		Name: "If-Match", In: "header", Required: true,
		Description: "Strong ETag returned by the latest person profile read. " +
			"Must be the exact single tag from that read; the RFC 7232 forms `*` " +
			"and comma-separated tag lists are not supported.",
		Schema: &huma.Schema{Type: huma.TypeString},
	})
}

func addPersonETagHeader(response *huma.Response) {
	response.Headers = map[string]*huma.Param{
		"ETag": {
			Description: "Strong person profile revision tag for optimistic concurrency",
			Schema:      &huma.Schema{Type: huma.TypeString},
		},
	}
}

func personProfileID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_person_id", "Person ID must be a positive integer")
		return 0, false
	}
	return id, true
}

func personETag(person store.Person) string {
	return fmt.Sprintf(`"person-%d-r%d"`, person.ID, person.Revision)
}

func personIfMatch(w http.ResponseWriter, r *http.Request, id int64) (int64, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 || (len(values) == 1 && strings.TrimSpace(values[0]) == "") {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "If-Match is required")
		return 0, false
	}
	if len(values) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match must contain exactly one revision tag")
		return 0, false
	}
	prefix := fmt.Sprintf(`"person-%d-r`, id)
	value := strings.TrimSpace(values[0])
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, `"`) {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match is not a person revision tag")
		return 0, false
	}
	revision, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(value, prefix), `"`), 10, 64)
	if err != nil || revision <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match is not a person revision tag")
		return 0, false
	}
	return revision, true
}

func decodePersonRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	_, ok := decodePersonRequestFields(w, r, target)
	return ok
}

func decodePersonRequestFields(
	w http.ResponseWriter, r *http.Request, target any,
) (map[string]json.RawMessage, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid person request")
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid person request: "+err.Error())
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "Person request must contain one JSON object")
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid person request")
		return nil, false
	}
	return fields, true
}
