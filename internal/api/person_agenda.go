package api

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
)

const maxPersonAgendaRequestBytes = 64 << 10

type PersonAgendaItem struct {
	UID          string             `json:"uid"`
	Ref          string             `json:"ref"`
	QualifiedRef string             `json:"qualified_ref"`
	Project      string             `json:"project"`
	Title        string             `json:"title"`
	Body         string             `json:"body,omitempty"`
	Revision     string             `json:"revision"`
	List         string             `json:"list"`
	Status       string             `json:"status"`
	State        personagenda.State `json:"state"`
	Priority     *int64             `json:"priority,omitempty"`
	Labels       []string           `json:"labels,omitempty"`
	Owner        string             `json:"owner,omitempty"`
	WebURL       string             `json:"web_url,omitempty"`
}

type PersonAgendaResult struct {
	PersonUID  string             `json:"person_uid" doc:"Canonical stable person UID used for new Kata links"`
	PersonUIDs []string           `json:"person_uids" doc:"Canonical UID followed by retired aliases that still resolve to this person"`
	Truncated  bool               `json:"truncated" doc:"More open tasks exist than the agenda result limit"`
	Project    string             `json:"project"`
	Items      []PersonAgendaItem `json:"items"`
}

type PersonAgendaCreateRequest struct {
	Title    string   `json:"title"`
	Body     string   `json:"body,omitempty"`
	List     string   `json:"list,omitempty"`
	Priority *int64   `json:"priority,omitempty"`
	Labels   []string `json:"labels,omitempty"`
}

type PersonAgendaLinkRequest struct {
	Ref  string `json:"ref"`
	List string `json:"list,omitempty"`
}

type PersonAgendaMutationResponse struct {
	Item PersonAgendaItem `json:"item"`
}

type PersonAgendaUpdateRequest struct {
	List *string `json:"list" nullable:"false"`
}

func (s *Server) registerPersonAgendaRoutes(api huma.API) {
	registerAPIV1RawHumaJSONRoute[TaskIntegrationStatusResponse](api,
		"getKataIntegrationStatus", http.MethodGet, "/integrations/kata/status",
		"Get Kata person agenda availability", s.handleKataIntegrationStatus)

	list := rawAPIV1Operation("listPersonAgenda", http.MethodGet, "/people/{id}/agenda", "List a person's live Kata agenda")
	addPersonIDParameter(&list)
	list.Responses = jsonResponsesFor[PersonAgendaResult](api)
	addErrorResponses(api, list.Responses, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListPersonAgenda)

	create := rawAPIV1Operation("createPersonAgendaItem", http.MethodPost, "/people/{id}/agenda", "Create a live Kata item for a person")
	addPersonIDParameter(&create)
	addIdempotencyKeyParameter(&create)
	create.RequestBody = jsonRequestBodyFor[PersonAgendaCreateRequest](api)
	create.Responses = jsonResponsesFor[PersonAgendaMutationResponse](api, http.StatusCreated)
	addErrorResponses(api, create.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict, http.StatusPreconditionRequired, http.StatusUnprocessableEntity, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, create, s.handleCreatePersonAgendaItem)

	link := rawAPIV1Operation("linkPersonAgendaItem", http.MethodPost, "/people/{id}/agenda/links", "Link an existing Kata item to a person")
	addPersonIDParameter(&link)
	link.RequestBody = jsonRequestBodyFor[PersonAgendaLinkRequest](api)
	link.Responses = jsonResponsesFor[PersonAgendaMutationResponse](api, http.StatusCreated)
	addErrorResponses(api, link.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, link, s.handleLinkPersonAgendaItem)

	update := rawAPIV1Operation("updatePersonAgendaItem", http.MethodPatch, "/people/{id}/agenda/{ref}", "Move a linked Kata item to another list")
	addPersonIDParameter(&update)
	update.Parameters = append(update.Parameters, &huma.Param{Name: "ref", In: pathKey, Required: true, Description: "Kata issue ref or canonical UID", Schema: &huma.Schema{Type: huma.TypeString}})
	update.RequestBody = jsonRequestBodyFor[PersonAgendaUpdateRequest](api)
	update.Responses = jsonResponsesFor[PersonAgendaMutationResponse](api)
	addErrorResponses(api, update.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, update, s.handleUpdatePersonAgendaItem)

	unlink := rawAPIV1Operation("unlinkPersonAgendaItem", http.MethodDelete, "/people/{id}/agenda/{ref}", "Unlink a live Kata item from a person")
	addPersonIDParameter(&unlink)
	unlink.Parameters = append(unlink.Parameters, &huma.Param{Name: "ref", In: pathKey, Required: true, Description: "Kata issue ref or canonical UID", Schema: &huma.Schema{Type: huma.TypeString}})
	unlink.Responses = jsonResponsesFor[PersonAgendaMutationResponse](api)
	addErrorResponses(api, unlink.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, unlink, s.handleUnlinkPersonAgendaItem)
}

func (s *Server) handleListPersonAgenda(w http.ResponseWriter, r *http.Request) {
	personID, ok := personProfileID(w, r)
	if !ok || !s.requirePersonAgenda(w) {
		return
	}
	result, err := s.personAgendaOperations.List(r.Context(), personID)
	if err != nil {
		writePersonAgendaError(w, err)
		return
	}
	items := make([]PersonAgendaItem, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, personAgendaItem(item))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, PersonAgendaResult{Project: result.Project, PersonUID: result.PersonUID, PersonUIDs: result.PersonUIDs, Truncated: result.Truncated, Items: items})
}

func (s *Server) handleCreatePersonAgendaItem(w http.ResponseWriter, r *http.Request) {
	personID, ok := personProfileID(w, r)
	if !ok || !s.requirePersonAgenda(w) {
		return
	}
	idempotencyKey, ok := personOperationIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request PersonAgendaCreateRequest
	if !decodePersonAgendaRequest(w, r, &request, "Invalid agenda request") {
		return
	}
	if strings.TrimSpace(request.Title) == "" {
		writeError(w, http.StatusBadRequest, "title_required", "Task title is required")
		return
	}
	item, err := s.personAgendaOperations.Create(r.Context(), personID, idempotencyKey, personagenda.CreateInput{Title: request.Title, Body: request.Body, List: request.List, PriorityValue: request.Priority, Labels: request.Labels})
	if err != nil {
		writePersonAgendaError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, PersonAgendaMutationResponse{Item: personAgendaItem(item)})
}

func (s *Server) handleLinkPersonAgendaItem(w http.ResponseWriter, r *http.Request) {
	personID, ok := personProfileID(w, r)
	if !ok || !s.requirePersonAgenda(w) {
		return
	}
	var request PersonAgendaLinkRequest
	if !decodePersonAgendaRequest(w, r, &request, "Invalid agenda link request") {
		return
	}
	if strings.TrimSpace(request.Ref) == "" {
		writeError(w, http.StatusBadRequest, "ref_required", "Task ref is required")
		return
	}
	item, err := s.personAgendaOperations.Link(r.Context(), personID, request.Ref, request.List)
	if err != nil {
		writePersonAgendaError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, PersonAgendaMutationResponse{Item: personAgendaItem(item)})
}

func (s *Server) handleUpdatePersonAgendaItem(w http.ResponseWriter, r *http.Request) {
	personID, ok := personProfileID(w, r)
	if !ok || !s.requirePersonAgenda(w) {
		return
	}
	ref := strings.TrimSpace(r.PathValue("ref"))
	if ref == "" {
		writeError(w, http.StatusBadRequest, "ref_required", "Task ref is required")
		return
	}
	var request PersonAgendaUpdateRequest
	if !decodePersonAgendaRequest(w, r, &request, "Invalid agenda update") {
		return
	}
	if request.List == nil {
		writeError(w, http.StatusBadRequest, "list_required", "List is required; edit task content in Kata")
		return
	}
	item, err := s.personAgendaOperations.Update(r.Context(), personID, ref, personagenda.UpdateInput{List: request.List})
	if err != nil {
		writePersonAgendaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, PersonAgendaMutationResponse{Item: personAgendaItem(item)})
}

func (s *Server) handleUnlinkPersonAgendaItem(w http.ResponseWriter, r *http.Request) {
	personID, ok := personProfileID(w, r)
	if !ok || !s.requirePersonAgenda(w) {
		return
	}
	ref := strings.TrimSpace(r.PathValue("ref"))
	if ref == "" {
		writeError(w, http.StatusBadRequest, "ref_required", "Task ref is required")
		return
	}
	item, err := s.personAgendaOperations.Unlink(r.Context(), personID, ref)
	if err != nil {
		writePersonAgendaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, PersonAgendaMutationResponse{Item: personAgendaItem(item)})
}

func (s *Server) requirePersonAgenda(w http.ResponseWriter) bool {
	if s.personAgendaOperations != nil {
		return true
	}
	writeError(w, http.StatusServiceUnavailable, "task_integration_unavailable", "Task integration is unavailable")
	return false
}

func decodePersonAgendaRequest(w http.ResponseWriter, r *http.Request, target any, message string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxPersonAgendaRequestBytes)
	decoder := jsontext.NewDecoder(r.Body, json.RejectUnknownMembers(true))
	if err := json.UnmarshalDecode(decoder, target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", message)
		return false
	}
	if err := json.UnmarshalDecode(decoder, &struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Agenda request must contain exactly one JSON value")
		return false
	}
	return true
}

func personAgendaItem(item personagenda.Item) PersonAgendaItem {
	return PersonAgendaItem{
		UID: item.UID, Ref: item.Ref, QualifiedRef: item.QualifiedRef, Project: item.Project,
		Title: item.Title, Body: item.Body, Revision: item.Revision, List: item.List,
		Status: item.Status, State: item.State, Priority: item.PriorityValue,
		Labels: append([]string(nil), item.Labels...), Owner: item.Owner, WebURL: item.WebURL,
	}
}

func writePersonAgendaError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusNotFound, "person_profile_not_found", "Person profile not found")
	case errors.Is(err, personagenda.ErrPersonIdentity):
		writeError(w, http.StatusConflict, "person_identity_required", "Person has no stable vCard UID for agenda linking")
	case errors.Is(err, personagenda.ErrIdentityLookup):
		writeError(w, http.StatusServiceUnavailable, "person_identity_unavailable", "Person identity lookup failed")
	case errors.Is(err, personagenda.ErrAlreadyLinked):
		writeError(w, http.StatusConflict, "task_already_linked", "This task is linked to another person; unlink it before linking it here")
	case errors.Is(err, taskclient.ErrNotFound):
		writeError(w, http.StatusNotFound, "agenda_item_not_found", "Agenda item not found for this person")
	case errors.Is(err, personagenda.ErrUnsafePersonMetadata), errors.Is(err, personagenda.ErrUnsafeListMetadata):
		writeError(w, http.StatusConflict, "unsafe_task_metadata", "Existing task metadata cannot be updated safely")
	case errors.Is(err, taskclient.ErrConflict):
		writeError(w, http.StatusConflict, "task_conflict", "The task changed before it could be updated")
	case errors.Is(err, taskclient.ErrRequestRejected):
		writeError(w, http.StatusUnprocessableEntity, "task_request_rejected", "The task service rejected this request")
	case errors.Is(err, taskclient.ErrAuthenticationRequired):
		writeError(w, http.StatusUnauthorized, "authentication_required", "Task service authentication is required")
	case errors.Is(err, taskclient.ErrWrongProject):
		writeError(w, http.StatusServiceUnavailable, "wrong_project", "The configured task project is unavailable")
	case errors.Is(err, taskclient.ErrResponseTooLarge):
		writeError(w, http.StatusServiceUnavailable, "task_response_too_large", "Kata's response exceeds the agenda size limit; view these tasks in Kata")
	default:
		writeError(w, http.StatusServiceUnavailable, "task_integration_unavailable", "Task service is unavailable")
	}
}

func (s *Server) handleKataIntegrationStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Integrations.Kata
	status := taskclient.EvaluateKata(r.Context(), taskclient.IntegrationConfig{
		Enabled: cfg.Enabled, Endpoint: cfg.Endpoint, APIKey: cfg.APIKey, DefaultProject: cfg.DefaultProject,
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, TaskIntegrationStatusResponse{State: status.State, Project: status.Project, Message: status.Message, SecurityNote: status.SecurityNote})
}
