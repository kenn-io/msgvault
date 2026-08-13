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
	"go.kenn.io/msgvault/internal/vcardmap"
)

const employmentsPath = "/api/v1/employments"

// EmploymentStore is the feature-local capability for temporal employment
// association records and the derived current-company projection.
type EmploymentStore interface {
	AddEmploymentContext(ctx context.Context, input store.EmploymentInput) (*store.Employment, error)
	GetEmploymentContext(ctx context.Context, id int64) (*store.Employment, error)
	UpdateEmploymentContext(ctx context.Context, id, expectedRevision int64, input store.EmploymentInput) (*store.Employment, error)
	EndEmploymentContext(ctx context.Context, id, expectedRevision int64, endDate store.PartialDate) (*store.Employment, error)
	SetPrimaryEmploymentContext(ctx context.Context, id, expectedRevision int64) (*store.Employment, error)
	DeleteEmploymentContext(ctx context.Context, id, expectedRevision int64) error
	ListEmploymentsContext(ctx context.Context, filter store.EmploymentFilter) ([]store.Employment, error)
	PrimaryCurrentEmploymentContext(ctx context.Context, personID int64) (store.EmploymentProjection, bool, error)
}

// EmploymentBody is the full mutable field set. Partial dates are inbound
// truncated ISO 8601 strings while responses expose their known components.
type EmploymentBody struct {
	PersonID       int64    `json:"person_id"`
	OrganizationID int64    `json:"organization_id"`
	Title          *string  `json:"title,omitempty" nullable:"true"`
	Role           *string  `json:"role,omitempty" nullable:"true"`
	Department     *string  `json:"department,omitempty" nullable:"true"`
	Location       *string  `json:"location,omitempty" nullable:"true"`
	AddressID      *int64   `json:"address_id,omitempty" nullable:"true"`
	Description    *string  `json:"description,omitempty" nullable:"true"`
	StartDate      *string  `json:"start_date,omitempty" nullable:"true"`
	EndDate        *string  `json:"end_date,omitempty" nullable:"true"`
	IsCurrent      *bool    `json:"is_current,omitempty" nullable:"true"`
	IsPrimary      *bool    `json:"is_primary,omitempty" nullable:"true"`
	Source         string   `json:"source" enum:"user,carddav_import,vcard_import,archive_observation,extraction,enrichment,system"`
	SourceRef      *string  `json:"source_ref,omitempty" nullable:"true"`
	Confidence     *float64 `json:"confidence,omitempty" nullable:"true"`
}

type EndEmploymentBody struct {
	EndDate string `json:"end_date"`
}

// EmploymentVCard is the vCard projection of the primary current employment.
type EmploymentVCard struct {
	Org   []string `json:"org,omitempty"`
	Title string   `json:"title,omitempty"`
	Role  string   `json:"role,omitempty"`
}

// EmploymentProjectionResponse is a read-time derived company and title.
type EmploymentProjectionResponse struct {
	EmploymentID     int64           `json:"employment_id"`
	OrganizationID   int64           `json:"organization_id"`
	OrganizationName string          `json:"organization_name"`
	Title            string          `json:"title,omitempty"`
	Role             string          `json:"role,omitempty"`
	Department       string          `json:"department,omitempty"`
	VCard            EmploymentVCard `json:"vcard"`
}

// EmploymentsResponse carries a listing and, only for people, their derived
// primary-current employment projection.
type EmploymentsResponse struct {
	Employments []store.Employment            `json:"employments"`
	Projection  *EmploymentProjectionResponse `json:"projection,omitempty"`
}

func (s *Server) registerEmploymentRoutes(api huma.API) {
	create := rawAPIV1Operation("createEmployment", http.MethodPost, "/employments", "Create an employment record")
	create.RequestBody = jsonRequestBodyFor[EmploymentBody](api)
	create.Responses = jsonResponsesFor[store.Employment](api, http.StatusCreated)
	addEmploymentETagHeader(create.Responses[httpStatusKey(http.StatusCreated)])
	addEmploymentLocationHeader(create.Responses[httpStatusKey(http.StatusCreated)])
	addErrorResponses(api, create.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, create, s.handleCreateEmployment)

	get := rawAPIV1Operation("getEmployment", http.MethodGet, "/employments/{id}", "Get an employment record")
	addEmploymentIDParameter(&get)
	get.Responses = jsonResponsesFor[store.Employment](api)
	addEmploymentETagHeader(get.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, get.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, get, s.handleGetEmployment)

	patch := rawAPIV1Operation("patchEmployment", http.MethodPatch, "/employments/{id}", "Replace an employment record's mutable fields")
	addEmploymentIDParameter(&patch)
	addEmploymentIfMatchParameter(&patch)
	patch.RequestBody = jsonRequestBodyFor[EmploymentBody](api)
	patch.Responses = jsonResponsesFor[store.Employment](api)
	addEmploymentETagHeader(patch.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, patch.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound, http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, patch, s.handlePatchEmployment)

	remove := rawAPIV1Operation("deleteEmployment", http.MethodDelete, "/employments/{id}", "Delete an employment record")
	addEmploymentIDParameter(&remove)
	addEmploymentIfMatchParameter(&remove)
	remove.Responses = rawHumaResponses(http.StatusNoContent)
	remove.Responses["default"] = errorResponseFor(api)
	addErrorResponses(api, remove.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound, http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, remove, s.handleDeleteEmployment)

	end := rawAPIV1Operation("endEmployment", http.MethodPost, "/employments/{id}/end", "End an employment record")
	addEmploymentIDParameter(&end)
	addEmploymentIfMatchParameter(&end)
	end.RequestBody = jsonRequestBodyFor[EndEmploymentBody](api)
	end.Responses = jsonResponsesFor[store.Employment](api)
	addEmploymentETagHeader(end.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, end.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound, http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, end, s.handleEndEmployment)

	primary := rawAPIV1Operation("setPrimaryEmployment", http.MethodPost, "/employments/{id}/primary", "Set the primary current employment")
	addEmploymentIDParameter(&primary)
	addEmploymentIfMatchParameter(&primary)
	primary.Responses = jsonResponsesFor[store.Employment](api)
	addEmploymentETagHeader(primary.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, primary.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound, http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, primary, s.handleSetPrimaryEmployment)

	personEmployments := rawAPIV1Operation("listPersonEmployments", http.MethodGet, "/persons/{id}/employments", "List a person's employment history")
	personEmployments.Parameters = append(personEmployments.Parameters, pathIntegerParam("Person ID"))
	personEmployments.Parameters = append(personEmployments.Parameters, employmentListParameters()...)
	personEmployments.Responses = jsonResponsesFor[EmploymentsResponse](api)
	addErrorResponses(api, personEmployments.Responses, http.StatusBadRequest,
		http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, personEmployments, s.handleListPersonEmployments)

	organizationEmployments := rawAPIV1Operation("listOrganizationEmployments", http.MethodGet, "/organizations/{id}/employments", "List an organization's employment records")
	addOrganizationIDParameter(&organizationEmployments)
	organizationEmployments.Parameters = append(organizationEmployments.Parameters, employmentListParameters()...)
	organizationEmployments.Responses = jsonResponsesFor[EmploymentsResponse](api)
	addErrorResponses(api, organizationEmployments.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, organizationEmployments, s.handleListOrganizationEmployments)
}

func employmentListParameters() []*huma.Param {
	return []*huma.Param{
		queryBooleanParam("current_only", "Only current employments"),
		queryIntegerParam("limit", "Maximum results"),
		queryIntegerParam("offset", "Results to skip"),
	}
}

func (s *Server) handleCreateEmployment(w http.ResponseWriter, r *http.Request) {
	employments, ok := s.employmentStore(w)
	if !ok {
		return
	}
	var body EmploymentBody
	if _, ok := decodeEntityRequestFields(w, r, &body, "employment"); !ok {
		return
	}
	input, err := employmentInputFromBody(body)
	if err != nil {
		s.writeEmploymentError(w, err)
		return
	}
	employment, err := employments.AddEmploymentContext(r.Context(), input)
	if err != nil {
		s.writeEmploymentError(w, err)
		return
	}
	writeEmployment(w, http.StatusCreated, employment)
}

func (s *Server) handleGetEmployment(w http.ResponseWriter, r *http.Request) {
	employments, ok := s.employmentStore(w)
	if !ok {
		return
	}
	id, ok := employmentID(w, r)
	if !ok {
		return
	}
	employment, err := employments.GetEmploymentContext(r.Context(), id)
	if err != nil {
		s.writeEmploymentError(w, err)
		return
	}
	writeEmployment(w, http.StatusOK, employment)
}

func (s *Server) handlePatchEmployment(w http.ResponseWriter, r *http.Request) {
	employments, ok := s.employmentStore(w)
	if !ok {
		return
	}
	id, ok := employmentID(w, r)
	if !ok {
		return
	}
	revision, ok := employmentIfMatch(w, r, id)
	if !ok {
		return
	}
	var body EmploymentBody
	if _, ok := decodeEntityRequestFields(w, r, &body, "employment"); !ok {
		return
	}
	input, err := employmentInputFromBody(body)
	if err != nil {
		s.writeEmploymentError(w, err)
		return
	}
	employment, err := employments.UpdateEmploymentContext(r.Context(), id, revision, input)
	if err != nil {
		s.writeEmploymentError(w, err)
		return
	}
	writeEmployment(w, http.StatusOK, employment)
}

func (s *Server) handleDeleteEmployment(w http.ResponseWriter, r *http.Request) {
	employments, ok := s.employmentStore(w)
	if !ok {
		return
	}
	id, ok := employmentID(w, r)
	if !ok {
		return
	}
	revision, ok := employmentIfMatch(w, r, id)
	if !ok {
		return
	}
	if err := employments.DeleteEmploymentContext(r.Context(), id, revision); err != nil {
		s.writeEmploymentError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEndEmployment(w http.ResponseWriter, r *http.Request) {
	employments, ok := s.employmentStore(w)
	if !ok {
		return
	}
	id, ok := employmentID(w, r)
	if !ok {
		return
	}
	revision, ok := employmentIfMatch(w, r, id)
	if !ok {
		return
	}
	var body EndEmploymentBody
	if _, ok := decodeEntityRequestFields(w, r, &body, "employment"); !ok {
		return
	}
	endDate, err := store.ParsePartialDate(body.EndDate)
	if err != nil {
		s.writeEmploymentError(w, err)
		return
	}
	employment, err := employments.EndEmploymentContext(r.Context(), id, revision, endDate)
	if err != nil {
		s.writeEmploymentError(w, err)
		return
	}
	writeEmployment(w, http.StatusOK, employment)
}

func (s *Server) handleSetPrimaryEmployment(w http.ResponseWriter, r *http.Request) {
	employments, ok := s.employmentStore(w)
	if !ok {
		return
	}
	id, ok := employmentID(w, r)
	if !ok {
		return
	}
	revision, ok := employmentIfMatch(w, r, id)
	if !ok {
		return
	}
	employment, err := employments.SetPrimaryEmploymentContext(r.Context(), id, revision)
	if err != nil {
		s.writeEmploymentError(w, err)
		return
	}
	writeEmployment(w, http.StatusOK, employment)
}

func (s *Server) handleListPersonEmployments(w http.ResponseWriter, r *http.Request) {
	profiles, ok := s.personProfileStore(w)
	if !ok {
		return
	}
	personID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	if _, err := profiles.GetPersonContext(r.Context(), personID); err != nil {
		s.writePersonError(w, err)
		return
	}
	employments, ok := s.employmentStore(w)
	if !ok {
		return
	}
	filter, ok := s.employmentFilter(w, r, store.EmploymentFilter{PersonID: personID})
	if !ok {
		return
	}
	rows, err := employments.ListEmploymentsContext(r.Context(), filter)
	if err != nil {
		s.writeEmploymentError(w, err)
		return
	}
	projection, found, err := employments.PrimaryCurrentEmploymentContext(r.Context(), personID)
	if err != nil {
		s.writeEmploymentError(w, err)
		return
	}
	response := EmploymentsResponse{Employments: rows}
	if found {
		response.Projection = employmentProjectionResponse(projection)
	}
	writeEmployments(w, response)
}

func (s *Server) handleListOrganizationEmployments(w http.ResponseWriter, r *http.Request) {
	organizations, ok := s.organizationStore(w)
	if !ok {
		return
	}
	organizationID, ok := organizationID(w, r)
	if !ok {
		return
	}
	if _, err := organizations.GetOrganizationContext(r.Context(), organizationID); err != nil {
		s.writeOrganizationError(w, err)
		return
	}
	employments, ok := s.employmentStore(w)
	if !ok {
		return
	}
	filter, ok := s.employmentFilter(w, r, store.EmploymentFilter{OrganizationID: organizationID})
	if !ok {
		return
	}
	rows, err := employments.ListEmploymentsContext(r.Context(), filter)
	if err != nil {
		s.writeEmploymentError(w, err)
		return
	}
	writeEmployments(w, EmploymentsResponse{Employments: rows})
}

func (s *Server) employmentFilter(w http.ResponseWriter, r *http.Request, filter store.EmploymentFilter) (store.EmploymentFilter, bool) {
	filter.Limit = store.DefaultEmploymentPageSize
	limit, present, err := queryInt(r, "limit")
	if err != nil {
		s.rejectBadParam(w, err)
		return filter, false
	}
	if present {
		if limit < 0 {
			s.rejectBadParam(w, newParamError("limit", "limit must not be negative"))
			return filter, false
		}
		if limit > 0 {
			filter.Limit = min(limit, store.MaxEmploymentPageSize)
		}
	}
	offset, present, err := queryInt(r, "offset")
	if err != nil {
		s.rejectBadParam(w, err)
		return filter, false
	}
	if present {
		if offset < 0 {
			s.rejectBadParam(w, newParamError("offset", "offset must not be negative"))
			return filter, false
		}
		filter.Offset = offset
	}
	currentOnly, _, err := queryBool(r, "current_only")
	if err != nil {
		s.rejectBadParam(w, err)
		return filter, false
	}
	filter.CurrentOnly = currentOnly
	return filter, true
}

func employmentInputFromBody(body EmploymentBody) (store.EmploymentInput, error) {
	source, err := store.ParseProvenance(body.Source)
	if err != nil {
		return store.EmploymentInput{}, err
	}
	input := store.EmploymentInput{
		PersonID: body.PersonID, OrganizationID: body.OrganizationID,
		Title: body.Title, Role: body.Role, Department: body.Department,
		Location: body.Location, AddressID: body.AddressID, Description: body.Description,
		IsCurrent: body.IsCurrent, IsPrimary: body.IsPrimary, Source: source,
		SourceRef: body.SourceRef, Confidence: body.Confidence,
	}
	if body.StartDate != nil {
		startDate, err := store.ParsePartialDate(*body.StartDate)
		if err != nil {
			return input, err
		}
		input.StartDate = &startDate
	}
	if body.EndDate != nil {
		endDate, err := store.ParsePartialDate(*body.EndDate)
		if err != nil {
			return input, err
		}
		input.EndDate = &endDate
	}
	return input, nil
}

func employmentProjectionResponse(projection store.EmploymentProjection) *EmploymentProjectionResponse {
	employment := vcardmap.FromProjection(projection)
	return &EmploymentProjectionResponse{
		EmploymentID: projection.EmploymentID, OrganizationID: projection.OrganizationID,
		OrganizationName: projection.OrganizationName, Title: projection.Title,
		Role: projection.Role, Department: projection.Department,
		VCard: EmploymentVCard{Org: vcardmap.OrgComponents(employment), Title: vcardmap.Title(employment), Role: vcardmap.Role(employment)},
	}
}

func (s *Server) employmentStore(w http.ResponseWriter) (EmploymentStore, bool) {
	employments, ok := s.store.(EmploymentStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "employments_unavailable", "Employments are unavailable")
	}
	return employments, ok
}

func (s *Server) writeEmploymentError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrEmploymentNotFound):
		writeError(w, http.StatusNotFound, "employment_not_found", "Employment not found")
	case errors.Is(err, store.ErrEmploymentRevisionConflict):
		writeError(w, http.StatusConflict, "employment_revision_conflict", "Employment changed; reload and retry")
	case errors.Is(err, store.ErrEmploymentPrimaryConflict):
		writeError(w, http.StatusConflict, "employment_primary_conflict", "Person already has a primary current employment")
	case errors.Is(err, store.ErrEmploymentDuplicateActive):
		writeError(w, http.StatusConflict, "employment_duplicate_active", "Person already has this current employment")
	case errors.Is(err, store.ErrEmploymentInvalid):
		writeError(w, http.StatusBadRequest, "invalid_employment", err.Error())
	case errors.Is(err, store.ErrInvalidPartialDate):
		writeError(w, http.StatusBadRequest, "invalid_partial_date", err.Error())
	case errors.Is(err, store.ErrInvalidProvenance):
		writeError(w, http.StatusBadRequest, "invalid_source", err.Error())
	case errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusBadRequest, "invalid_person_id", "Person ID is invalid")
	case errors.Is(err, store.ErrOrganizationNotFound):
		writeError(w, http.StatusBadRequest, "invalid_organization_id", "Organization ID is invalid")
	case errors.Is(err, store.ErrOrganizationInvalid):
		writeError(w, http.StatusBadRequest, "invalid_organization", err.Error())
	default:
		s.logger.Error("employment operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "employment_failed", "Employment operation failed")
	}
}

func writeEmployment(w http.ResponseWriter, status int, employment *store.Employment) {
	w.Header().Set(etagHeaderName, employmentETag(*employment))
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusCreated {
		w.Header().Set("Location", employmentsPath+"/"+strconv.FormatInt(employment.ID, 10))
	}
	writeJSON(w, status, employment)
}

func writeEmployments(w http.ResponseWriter, response EmploymentsResponse) {
	if response.Employments == nil {
		response.Employments = make([]store.Employment, 0)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func addEmploymentIDParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, pathIntegerParam("Employment ID"))
}

func addEmploymentIfMatchParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{Name: ifMatchHeaderName, In: "header", Required: true, Description: "Strong ETag returned by the latest employment read", Schema: &huma.Schema{Type: huma.TypeString}})
}

func addEmploymentETagHeader(response *huma.Response) {
	response.Headers = map[string]*huma.Param{etagHeaderName: {Description: "Strong employment revision tag for optimistic concurrency", Schema: &huma.Schema{Type: huma.TypeString}}}
}

func addEmploymentLocationHeader(response *huma.Response) {
	response.Headers["Location"] = &huma.Param{Description: "Canonical URL of the created employment record", Schema: &huma.Schema{Type: huma.TypeString}}
}

func employmentID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_employment_id", "Employment ID must be a positive integer")
		return 0, false
	}
	return id, true
}

func employmentETag(employment store.Employment) string {
	return fmt.Sprintf(`"employment-%d-r%d"`, employment.ID, employment.Revision)
}

func employmentIfMatch(w http.ResponseWriter, r *http.Request, id int64) (int64, bool) {
	values := r.Header.Values(ifMatchHeaderName)
	if len(values) == 0 || (len(values) == 1 && strings.TrimSpace(values[0]) == "") {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "If-Match is required")
		return 0, false
	}
	if len(values) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match must contain exactly one revision tag")
		return 0, false
	}
	prefix := fmt.Sprintf(`"employment-%d-r`, id)
	value := strings.TrimSpace(values[0])
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, `"`) {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match is not an employment revision tag")
		return 0, false
	}
	revision, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(value, prefix), `"`), 10, 64)
	if err != nil || revision <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match is not an employment revision tag")
		return 0, false
	}
	return revision, true
}
