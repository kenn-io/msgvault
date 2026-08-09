package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

const (
	relationshipTypesPath   = "/api/v1/relationship-types"
	personRelationshipsPath = "/api/v1/person-relationships"
	relationshipReviewsPath = "/api/v1/person-relationship-reviews"
)

// PersonRelationshipStore is the capability required by the curated person
// relationship HTTP surface.
type PersonRelationshipStore interface {
	ListRelationshipTypesContext(ctx context.Context) ([]store.RelationshipType, error)
	GetRelationshipTypeContext(ctx context.Context, id int64) (*store.RelationshipType, error)
	CreateRelationshipTypeContext(ctx context.Context, input store.RelationshipTypeInput) (*store.RelationshipType, error)
	UpdateRelationshipTypeContext(ctx context.Context, id, expectedRevision int64, update store.RelationshipTypeUpdate) (*store.RelationshipType, error)
	DeleteRelationshipTypeContext(ctx context.Context, id, expectedRevision int64) error
	AddPersonRelationshipContext(ctx context.Context, input store.PersonRelationshipInput) (*store.PersonRelationship, error)
	GetPersonRelationshipContext(ctx context.Context, id int64) (*store.PersonRelationship, error)
	PatchPersonRelationshipContext(ctx context.Context, id, expectedRevision int64, patch store.PersonRelationshipPatch, actor string) (*store.PersonRelationship, error)
	DeletePersonRelationshipContext(ctx context.Context, id, expectedRevision int64) error
	ListPersonRelationshipsContext(ctx context.Context, personID int64, opts store.PersonRelationshipListOptions) ([]store.PersonRelationshipView, error)
	ListRelationshipReviewsContext(ctx context.Context, opts store.RelationshipReviewListOptions) ([]store.RelationshipReview, error)
}

const apiRelationshipActor = "user"

type RelationshipTypesResponse struct {
	RelationshipTypes []store.RelationshipType `json:"relationship_types"`
}
type PersonRelationshipsResponse struct {
	Relationships []store.PersonRelationshipView `json:"relationships"`
}
type RelationshipReviewsResponse struct {
	Reviews []store.RelationshipReview `json:"reviews"`
}

type CreateRelationshipTypeRequest struct {
	Slug             string  `json:"slug"`
	ForwardLabel     string  `json:"forward_label"`
	ReverseLabel     string  `json:"reverse_label"`
	IsSymmetric      bool    `json:"is_symmetric,omitempty"`
	VCardRelatedType *string `json:"vcard_related_type,omitempty" nullable:"true"`
	Color            *string `json:"color,omitempty" nullable:"true"`
	Icon             *string `json:"icon,omitempty" nullable:"true"`
	Description      *string `json:"description,omitempty" nullable:"true"`
}

type PatchRelationshipTypeRequest struct {
	ForwardLabel     string `json:"forward_label,omitempty"`
	ReverseLabel     string `json:"reverse_label,omitempty"`
	VCardRelatedType string `json:"vcard_related_type,omitempty"`
	Color            string `json:"color,omitempty"`
	Icon             string `json:"icon,omitempty"`
	Description      string `json:"description,omitempty"`
}

type CreatePersonRelationshipRequest struct {
	SourcePersonID       int64   `json:"source_person_id"`
	TargetPersonID       int64   `json:"target_person_id"`
	RelationshipTypeSlug string  `json:"relationship_type_slug"`
	StartDate            *string `json:"start_date,omitempty"`
	EndDate              *string `json:"end_date,omitempty"`
	Notes                *string `json:"notes,omitempty" nullable:"true"`
}

type PatchPersonRelationshipRequest struct {
	EndDate *string `json:"end_date,omitempty"`
	Notes   *string `json:"notes,omitempty" nullable:"true"`
}

func (s *Server) registerPersonRelationshipRoutes(api huma.API) {
	listTypes := rawAPIV1Operation("listRelationshipTypes", http.MethodGet, "/relationship-types", "List person relationship types")
	listTypes.Responses = jsonResponsesFor[RelationshipTypesResponse](api)
	addErrorResponses(api, listTypes.Responses, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, listTypes, s.handleListRelationshipTypes)

	createType := rawAPIV1Operation("createRelationshipType", http.MethodPost, "/relationship-types", "Create a user-owned relationship type")
	createType.RequestBody = jsonRequestBodyFor[CreateRelationshipTypeRequest](api)
	createType.Responses = jsonResponsesFor[store.RelationshipType](api, http.StatusCreated)
	addRelationshipTypeHeaders(createType.Responses[httpStatusKey(http.StatusCreated)], true)
	addErrorResponses(api, createType.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, createType, s.handleCreateRelationshipType)

	getType := rawAPIV1Operation("getRelationshipType", http.MethodGet, "/relationship-types/{id}", "Get a relationship type")
	addRelationshipIDParameter(&getType, "Relationship type ID")
	getType.Responses = jsonResponsesFor[store.RelationshipType](api)
	addRelationshipTypeHeaders(getType.Responses[httpStatusKey(http.StatusOK)], false)
	addErrorResponses(api, getType.Responses, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, getType, s.handleGetRelationshipType)

	patchType := rawAPIV1Operation("patchRelationshipType", http.MethodPatch, "/relationship-types/{id}", "Update a relationship type")
	addRelationshipIDParameter(&patchType, "Relationship type ID")
	addRelationshipIfMatchParameter(&patchType, "relationship type")
	patchType.RequestBody = jsonRequestBodyFor[PatchRelationshipTypeRequest](api)
	patchType.Responses = jsonResponsesFor[store.RelationshipType](api)
	addRelationshipTypeHeaders(patchType.Responses[httpStatusKey(http.StatusOK)], false)
	addErrorResponses(api, patchType.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound, http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, patchType, s.handlePatchRelationshipType)

	deleteType := rawAPIV1Operation("deleteRelationshipType", http.MethodDelete, "/relationship-types/{id}", "Delete an unused relationship type")
	addRelationshipIDParameter(&deleteType, "Relationship type ID")
	addRelationshipIfMatchParameter(&deleteType, "relationship type")
	deleteType.Responses = rawHumaResponses(http.StatusNoContent)
	deleteType.Responses["default"] = errorResponseFor(api)
	addErrorResponses(api, deleteType.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound, http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, deleteType, s.handleDeleteRelationshipType)

	listForPerson := rawAPIV1Operation("listPersonRelationships", http.MethodGet, "/persons/{id}/relationships", "List one person's relationships")
	addRelationshipIDParameter(&listForPerson, "Durable person ID")
	listForPerson.Parameters = append(listForPerson.Parameters, &huma.Param{Name: "include_ended", In: "query", Schema: &huma.Schema{Type: huma.TypeBoolean}})
	listForPerson.Responses = jsonResponsesFor[PersonRelationshipsResponse](api)
	addErrorResponses(api, listForPerson.Responses, http.StatusBadRequest, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, listForPerson, s.handleListPersonRelationships)

	createEdge := rawAPIV1Operation("createPersonRelationship", http.MethodPost, "/person-relationships", "Declare a relationship between two persons")
	createEdge.RequestBody = jsonRequestBodyFor[CreatePersonRelationshipRequest](api)
	createEdge.Responses = jsonResponsesFor[store.PersonRelationship](api, http.StatusCreated)
	addPersonRelationshipHeaders(createEdge.Responses[httpStatusKey(http.StatusCreated)], true)
	addErrorResponses(api, createEdge.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, createEdge, s.handleCreatePersonRelationship)

	getEdge := rawAPIV1Operation("getPersonRelationship", http.MethodGet, "/person-relationships/{id}", "Get one person relationship")
	addRelationshipIDParameter(&getEdge, "Person relationship ID")
	getEdge.Responses = jsonResponsesFor[store.PersonRelationship](api)
	addPersonRelationshipHeaders(getEdge.Responses[httpStatusKey(http.StatusOK)], false)
	addErrorResponses(api, getEdge.Responses, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, getEdge, s.handleGetPersonRelationship)

	patchEdge := rawAPIV1Operation("patchPersonRelationship", http.MethodPatch, "/person-relationships/{id}", "End a relationship or replace its notes")
	addRelationshipIDParameter(&patchEdge, "Person relationship ID")
	addRelationshipIfMatchParameter(&patchEdge, "person relationship")
	patchEdge.RequestBody = jsonRequestBodyFor[PatchPersonRelationshipRequest](api)
	patchEdge.Responses = jsonResponsesFor[store.PersonRelationship](api)
	addPersonRelationshipHeaders(patchEdge.Responses[httpStatusKey(http.StatusOK)], false)
	addErrorResponses(api, patchEdge.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound, http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, patchEdge, s.handlePatchPersonRelationship)

	deleteEdge := rawAPIV1Operation("deletePersonRelationship", http.MethodDelete, "/person-relationships/{id}", "Delete a person relationship")
	addRelationshipIDParameter(&deleteEdge, "Person relationship ID")
	addRelationshipIfMatchParameter(&deleteEdge, "person relationship")
	deleteEdge.Responses = rawHumaResponses(http.StatusNoContent)
	deleteEdge.Responses["default"] = errorResponseFor(api)
	addErrorResponses(api, deleteEdge.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound, http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, deleteEdge, s.handleDeletePersonRelationship)

	listReviews := rawAPIV1Operation("listPersonRelationshipReviews", http.MethodGet, "/person-relationship-reviews", "List imported RELATED values awaiting review")
	listReviews.Parameters = append(listReviews.Parameters,
		&huma.Param{Name: "status", In: "query", Schema: &huma.Schema{Type: huma.TypeString, Enum: []any{"pending", "accepted", "rejected"}}},
		&huma.Param{Name: "person_id", In: "query", Schema: &huma.Schema{Type: huma.TypeInteger, Format: "int64"}},
	)
	listReviews.Responses = jsonResponsesFor[RelationshipReviewsResponse](api)
	addErrorResponses(api, listReviews.Responses, http.StatusBadRequest, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, listReviews, s.handleListRelationshipReviews)
}

func (s *Server) handleListRelationshipTypes(w http.ResponseWriter, r *http.Request) {
	relationships, ok := s.personRelationshipStore(w)
	if !ok {
		return
	}
	types, err := relationships.ListRelationshipTypesContext(r.Context())
	if err != nil {
		s.writeRelationshipError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, RelationshipTypesResponse{RelationshipTypes: types})
}

func (s *Server) handleCreateRelationshipType(w http.ResponseWriter, r *http.Request) {
	relationships, ok := s.personRelationshipStore(w)
	if !ok {
		return
	}
	var request CreateRelationshipTypeRequest
	if !decodePersonRequest(w, r, &request) {
		return
	}
	created, err := relationships.CreateRelationshipTypeContext(r.Context(), store.RelationshipTypeInput{Slug: request.Slug, ForwardLabel: request.ForwardLabel, ReverseLabel: request.ReverseLabel, IsSymmetric: request.IsSymmetric, VCardRelatedType: request.VCardRelatedType, Color: request.Color, Icon: request.Icon, Description: request.Description})
	if err != nil {
		s.writeRelationshipError(w, err)
		return
	}
	w.Header().Set("Location", relationshipTypesPath+"/"+strconv.FormatInt(created.ID, 10))
	writeRelationshipType(w, http.StatusCreated, created)
}

func (s *Server) handleGetRelationshipType(w http.ResponseWriter, r *http.Request) {
	relationships, ok := s.personRelationshipStore(w)
	if !ok {
		return
	}
	id, ok := relationshipPathID(w, r, "relationship type")
	if !ok {
		return
	}
	relationshipType, err := relationships.GetRelationshipTypeContext(r.Context(), id)
	if err != nil {
		s.writeRelationshipError(w, err)
		return
	}
	writeRelationshipType(w, http.StatusOK, relationshipType)
}

func (s *Server) handlePatchRelationshipType(w http.ResponseWriter, r *http.Request) {
	relationships, ok := s.personRelationshipStore(w)
	if !ok {
		return
	}
	id, ok := relationshipPathID(w, r, "relationship type")
	if !ok {
		return
	}
	revision, ok := relationshipIfMatch(w, r, "relationship-type", id)
	if !ok {
		return
	}
	var request PatchRelationshipTypeRequest
	fields, ok := decodePersonRequestFields(w, r, &request)
	if !ok {
		return
	}
	if len(fields) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "at least one relationship type field is required")
		return
	}
	for field, value := range fields {
		if isJSONNull(value) {
			writeError(w, http.StatusBadRequest, "bad_request", field+" must not be null")
			return
		}
	}
	update := store.RelationshipTypeUpdate{}
	if _, present := fields["forward_label"]; present {
		update.ForwardLabel = &request.ForwardLabel
	}
	if _, present := fields["reverse_label"]; present {
		update.ReverseLabel = &request.ReverseLabel
	}
	if _, present := fields["vcard_related_type"]; present {
		update.VCardRelatedType = &request.VCardRelatedType
	}
	if _, present := fields["color"]; present {
		update.Color = &request.Color
	}
	if _, present := fields["icon"]; present {
		update.Icon = &request.Icon
	}
	if _, present := fields["description"]; present {
		update.Description = &request.Description
	}
	updated, err := relationships.UpdateRelationshipTypeContext(r.Context(), id, revision, update)
	if err != nil {
		s.writeRelationshipError(w, err)
		return
	}
	writeRelationshipType(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteRelationshipType(w http.ResponseWriter, r *http.Request) {
	relationships, ok := s.personRelationshipStore(w)
	if !ok {
		return
	}
	id, ok := relationshipPathID(w, r, "relationship type")
	if !ok {
		return
	}
	revision, ok := relationshipIfMatch(w, r, "relationship-type", id)
	if !ok {
		return
	}
	if err := relationships.DeleteRelationshipTypeContext(r.Context(), id, revision); err != nil {
		s.writeRelationshipError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListPersonRelationships(w http.ResponseWriter, r *http.Request) {
	relationships, ok := s.personRelationshipStore(w)
	if !ok {
		return
	}
	personID, ok := relationshipPathID(w, r, "person")
	if !ok {
		return
	}
	includeEnded, ok := relationshipBoolQuery(w, r, "include_ended")
	if !ok {
		return
	}
	views, err := relationships.ListPersonRelationshipsContext(r.Context(), personID, store.PersonRelationshipListOptions{IncludeEnded: includeEnded})
	if err != nil {
		s.writeRelationshipError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, PersonRelationshipsResponse{Relationships: views})
}

func (s *Server) handleCreatePersonRelationship(w http.ResponseWriter, r *http.Request) {
	relationships, ok := s.personRelationshipStore(w)
	if !ok {
		return
	}
	var request CreatePersonRelationshipRequest
	if !decodePersonRequest(w, r, &request) {
		return
	}
	startDate, ok := relationshipPartialDate(w, "start_date", request.StartDate)
	if !ok {
		return
	}
	endDate, ok := relationshipPartialDate(w, "end_date", request.EndDate)
	if !ok {
		return
	}
	created, err := relationships.AddPersonRelationshipContext(r.Context(), store.PersonRelationshipInput{SourcePersonID: request.SourcePersonID, TargetPersonID: request.TargetPersonID, TypeSlug: request.RelationshipTypeSlug, StartDate: startDate, EndDate: endDate, Notes: request.Notes, Source: store.ProvenanceUser, Actor: apiRelationshipActor})
	if err != nil {
		if errors.Is(err, store.ErrRelationshipTypeNotFound) {
			writeError(w, http.StatusBadRequest, "invalid_relationship_type", "relationship_type_slug is not a known relationship type")
			return
		}
		s.writeRelationshipError(w, err)
		return
	}
	w.Header().Set("Location", personRelationshipsPath+"/"+strconv.FormatInt(created.ID, 10))
	writePersonRelationship(w, http.StatusCreated, created)
}

func (s *Server) handleGetPersonRelationship(w http.ResponseWriter, r *http.Request) {
	relationships, ok := s.personRelationshipStore(w)
	if !ok {
		return
	}
	id, ok := relationshipPathID(w, r, "person relationship")
	if !ok {
		return
	}
	edge, err := relationships.GetPersonRelationshipContext(r.Context(), id)
	if err != nil {
		s.writeRelationshipError(w, err)
		return
	}
	writePersonRelationship(w, http.StatusOK, edge)
}

func (s *Server) handlePatchPersonRelationship(w http.ResponseWriter, r *http.Request) {
	relationships, ok := s.personRelationshipStore(w)
	if !ok {
		return
	}
	id, ok := relationshipPathID(w, r, "person relationship")
	if !ok {
		return
	}
	revision, ok := relationshipIfMatch(w, r, "person-relationship", id)
	if !ok {
		return
	}
	var request PatchPersonRelationshipRequest
	fields, ok := decodePersonRequestFields(w, r, &request)
	if !ok {
		return
	}
	_, hasEnd := fields["end_date"]
	_, hasNotes := fields["notes"]
	if !hasEnd && !hasNotes {
		writeError(w, http.StatusBadRequest, "bad_request", "end_date or notes is required")
		return
	}
	var until *store.PartialDate
	if hasEnd {
		until, ok = relationshipPartialDate(w, "end_date", request.EndDate)
		if !ok {
			return
		}
		if until == nil {
			writeError(w, http.StatusBadRequest, "bad_request", "end_date must be a partial date; to reopen a relationship, declare a new one")
			return
		}
	}
	updated, err := relationships.PatchPersonRelationshipContext(r.Context(), id, revision, store.PersonRelationshipPatch{EndDate: until, UpdateEndDate: hasEnd, Notes: request.Notes, UpdateNotes: hasNotes}, apiRelationshipActor)
	if err != nil {
		s.writeRelationshipError(w, err)
		return
	}
	writePersonRelationship(w, http.StatusOK, updated)
}

func (s *Server) handleDeletePersonRelationship(w http.ResponseWriter, r *http.Request) {
	relationships, ok := s.personRelationshipStore(w)
	if !ok {
		return
	}
	id, ok := relationshipPathID(w, r, "person relationship")
	if !ok {
		return
	}
	revision, ok := relationshipIfMatch(w, r, "person-relationship", id)
	if !ok {
		return
	}
	if err := relationships.DeletePersonRelationshipContext(r.Context(), id, revision); err != nil {
		s.writeRelationshipError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListRelationshipReviews(w http.ResponseWriter, r *http.Request) {
	relationships, ok := s.personRelationshipStore(w)
	if !ok {
		return
	}
	opts := store.RelationshipReviewListOptions{}
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		switch store.RelationshipReviewStatus(raw) {
		case store.RelationshipReviewPending, store.RelationshipReviewAccepted, store.RelationshipReviewRejected:
			opts.Status = store.RelationshipReviewStatus(raw)
		default:
			writeError(w, http.StatusBadRequest, "invalid_status", "status must be pending, accepted, or rejected")
			return
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("person_id")); raw != "" {
		personID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || personID <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_person_id", "person_id must be a positive integer")
			return
		}
		opts.PersonID = personID
	}
	reviews, err := relationships.ListRelationshipReviewsContext(r.Context(), opts)
	if err != nil {
		s.writeRelationshipError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, RelationshipReviewsResponse{Reviews: reviews})
}

func (s *Server) personRelationshipStore(w http.ResponseWriter) (PersonRelationshipStore, bool) {
	relationships, ok := s.store.(PersonRelationshipStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "person_relationships_unavailable", "Person relationships are unavailable")
	}
	return relationships, ok
}

func (s *Server) writeRelationshipError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrRelationshipTypeNotFound):
		writeError(w, http.StatusNotFound, "relationship_type_not_found", "Relationship type not found")
	case errors.Is(err, store.ErrPersonRelationshipNotFound):
		writeError(w, http.StatusNotFound, "person_relationship_not_found", "Person relationship not found")
	case errors.Is(err, store.ErrRelationshipReviewNotFound):
		writeError(w, http.StatusNotFound, "relationship_review_not_found", "Relationship review not found")
	case errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusNotFound, "person_not_found", "Person not found")
	case errors.Is(err, store.ErrRelationshipTypeRevisionConflict), errors.Is(err, store.ErrPersonRelationshipRevisionConflict):
		writeError(w, http.StatusConflict, "relationship_revision_conflict", "The record changed; reload and retry")
	case errors.Is(err, store.ErrRelationshipTypeSlugConflict):
		writeError(w, http.StatusConflict, "relationship_type_slug_conflict", "A relationship type with that slug already exists")
	case errors.Is(err, store.ErrRelationshipTypeRelatedTypeConflict):
		writeError(w, http.StatusConflict, "relationship_type_related_type_conflict", "That vCard RELATED type is already mapped to another relationship type")
	case errors.Is(err, store.ErrRelationshipTypeNotDeletable):
		writeError(w, http.StatusConflict, "relationship_type_not_deletable", "Seeded system relationship types cannot be deleted")
	case errors.Is(err, store.ErrRelationshipTypeInUse):
		writeError(w, http.StatusConflict, "relationship_type_in_use", "That relationship type is still referenced by relationships")
	case errors.Is(err, store.ErrPersonRelationshipDuplicate):
		writeError(w, http.StatusConflict, "person_relationship_duplicate", "An active relationship of that type already exists between those people")
	case errors.Is(err, store.ErrRelationshipReviewNotPending):
		writeError(w, http.StatusConflict, "relationship_review_not_pending", "That relationship review has already been decided")
	case errors.Is(err, store.ErrPersonRelationshipSelf), errors.Is(err, store.ErrPersonRelationshipInterval), errors.Is(err, store.ErrPersonRelationshipInvalid), errors.Is(err, store.ErrRelationshipTypeInvalid), errors.Is(err, store.ErrRelationshipTypeSymmetricLabels), errors.Is(err, store.ErrRelationshipReviewInvalid), errors.Is(err, store.ErrInvalidPartialDate), errors.Is(err, store.ErrConfidenceScope), errors.Is(err, store.ErrInvalidProvenance):
		writeError(w, http.StatusBadRequest, "invalid_relationship", err.Error())
	default:
		s.logger.Error("person relationship operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "person_relationship_failed", "Person relationship operation failed")
	}
}

func writeRelationshipType(w http.ResponseWriter, status int, relationshipType *store.RelationshipType) {
	w.Header().Set("ETag", fmt.Sprintf(`"relationship-type-%d-r%d"`, relationshipType.ID, relationshipType.Revision))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, relationshipType)
}
func writePersonRelationship(w http.ResponseWriter, status int, edge *store.PersonRelationship) {
	w.Header().Set("ETag", fmt.Sprintf(`"person-relationship-%d-r%d"`, edge.ID, edge.Revision))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, edge)
}

func addRelationshipIDParameter(operation *huma.Operation, description string) {
	operation.Parameters = append(operation.Parameters, &huma.Param{Name: "id", In: "path", Required: true, Description: description, Schema: &huma.Schema{Type: huma.TypeInteger, Format: "int64"}})
}
func addRelationshipIfMatchParameter(operation *huma.Operation, subject string) {
	operation.Parameters = append(operation.Parameters, &huma.Param{Name: ifMatchHeaderName, In: "header", Required: true, Description: "Strong ETag returned by the latest " + subject + " read", Schema: &huma.Schema{Type: huma.TypeString}})
}
func addRelationshipTypeHeaders(response *huma.Response, location bool) {
	response.Headers = map[string]*huma.Param{"ETag": {Description: "Strong relationship type revision tag for optimistic concurrency", Schema: &huma.Schema{Type: huma.TypeString}}}
	if location {
		response.Headers["Location"] = &huma.Param{Description: "Created relationship type", Schema: &huma.Schema{Type: huma.TypeString}}
	}
}
func addPersonRelationshipHeaders(response *huma.Response, location bool) {
	response.Headers = map[string]*huma.Param{"ETag": {Description: "Strong person relationship revision tag for optimistic concurrency", Schema: &huma.Schema{Type: huma.TypeString}}}
	if location {
		response.Headers["Location"] = &huma.Param{Description: "Created person relationship", Schema: &huma.Schema{Type: huma.TypeString}}
	}
}

func relationshipPathID(w http.ResponseWriter, r *http.Request, subject string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_id", strings.ToUpper(subject[:1])+subject[1:]+" ID must be a positive integer")
		return 0, false
	}
	return id, true
}
func relationshipIfMatch(w http.ResponseWriter, r *http.Request, prefix string, id int64) (int64, bool) {
	values := r.Header.Values(ifMatchHeaderName)
	if len(values) == 0 || len(values) == 1 && strings.TrimSpace(values[0]) == "" {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "If-Match is required")
		return 0, false
	}
	if len(values) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match must contain exactly one revision tag")
		return 0, false
	}
	expected := fmt.Sprintf(`"%s-%d-r`, prefix, id)
	value := strings.TrimSpace(values[0])
	if !strings.HasPrefix(value, expected) || !strings.HasSuffix(value, `"`) {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match is not a "+prefix+" revision tag")
		return 0, false
	}
	revision, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(value, expected), `"`), 10, 64)
	if err != nil || revision <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match is not a "+prefix+" revision tag")
		return 0, false
	}
	return revision, true
}
func relationshipPartialDate(w http.ResponseWriter, field string, value *string) (*store.PartialDate, bool) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil, true
	}
	parsed, err := store.ParseRelationshipDate(*value)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_partial_date", field+" must be YYYY, YYYY-MM, or YYYY-MM-DD")
		return nil, false
	}
	return &parsed, true
}
func relationshipBoolQuery(w http.ResponseWriter, r *http.Request, name string) (bool, bool) {
	switch strings.TrimSpace(r.URL.Query().Get(name)) {
	case "":
		return false, true
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		writeError(w, http.StatusBadRequest, "invalid_query_parameter", name+" must be true or false")
		return false, false
	}
}
