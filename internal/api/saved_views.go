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
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/jsonexact"
	"go.kenn.io/msgvault/internal/store"
)

const savedViewsPath = "/api/v1/saved-views"

type SavedViewStore interface {
	CreateSavedView(ctx context.Context, input store.SavedViewInput) (*store.SavedView, error)
	ListSavedViews(ctx context.Context) ([]store.SavedView, error)
	GetSavedView(ctx context.Context, id int64) (*store.SavedView, error)
	UpdateSavedView(ctx context.Context, id, expectedRevision int64, input store.SavedViewInput) (*store.SavedView, error)
	DeleteSavedView(ctx context.Context, id, expectedRevision int64) error
}

type SavedView struct {
	ID             int64                        `json:"id"`
	Name           string                       `json:"name"`
	Description    *string                      `json:"description,omitempty"`
	CanonicalState store.SavedViewStateEnvelope `json:"canonical_state"`
	SchemaVersion  int                          `json:"schema_version"`
	Revision       int64                        `json:"revision"`
	CreatedAt      time.Time                    `json:"created_at"`
	UpdatedAt      time.Time                    `json:"updated_at"`
}

type SavedViewsResponse struct {
	SavedViews []SavedView `json:"saved_views"`
}

type CreateSavedViewRequest struct {
	Name           string                       `json:"name"`
	Description    *string                      `json:"description,omitempty"`
	CanonicalState store.SavedViewStateEnvelope `json:"canonical_state"`
	SchemaVersion  int                          `json:"schema_version"`
}

type PatchSavedViewRequest struct {
	Name           *string                       `json:"name,omitempty"`
	Description    *string                       `json:"description,omitempty"`
	CanonicalState *store.SavedViewStateEnvelope `json:"canonical_state,omitempty"`
	SchemaVersion  *int                          `json:"schema_version,omitempty"`
}

func (s *Server) registerSavedViewRoutes(api huma.API) {
	list := rawAPIV1Operation("listSavedViews", http.MethodGet, "/saved-views", "List shared analytical Saved Views")
	list.Responses = jsonResponsesFor[SavedViewsResponse](api)
	registerRawHumaRoute(api, list, s.handleListSavedViews)

	create := rawAPIV1Operation("createSavedView", http.MethodPost, "/saved-views", "Create a shared analytical Saved View")
	create.RequestBody = jsonRequestBodyFor[CreateSavedViewRequest](api)
	create.Responses = jsonResponsesFor[SavedView](api, http.StatusCreated)
	addSavedViewETagHeader(create.Responses[httpStatusKey(http.StatusCreated)])
	addErrorResponses(api, create.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, create, s.handleCreateSavedView)

	get := rawAPIV1Operation("getSavedView", http.MethodGet, "/saved-views/{id}", "Get a shared analytical Saved View")
	addSavedViewIDParameter(&get)
	get.Responses = jsonResponsesFor[SavedView](api)
	addSavedViewETagHeader(get.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, get.Responses, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, get, s.handleGetSavedView)

	patch := rawAPIV1Operation("patchSavedView", http.MethodPatch, "/saved-views/{id}", "Update a shared analytical Saved View")
	addSavedViewIDParameter(&patch)
	addSavedViewIfMatchParameter(&patch)
	patch.RequestBody = jsonRequestBodyFor[PatchSavedViewRequest](api)
	patch.Responses = jsonResponsesFor[SavedView](api)
	addSavedViewETagHeader(patch.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, patch.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusNotFound,
		http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, patch, s.handlePatchSavedView)

	remove := rawAPIV1Operation("deleteSavedView", http.MethodDelete, "/saved-views/{id}", "Delete a shared analytical Saved View")
	addSavedViewIDParameter(&remove)
	addSavedViewIfMatchParameter(&remove)
	remove.Responses = rawHumaResponses(http.StatusNoContent)
	remove.Responses["default"] = errorResponseFor(api)
	addErrorResponses(api, remove.Responses, http.StatusBadRequest, http.StatusUnauthorized,
		http.StatusConflict, http.StatusNotFound, http.StatusPreconditionRequired,
		http.StatusInternalServerError, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, remove, s.handleDeleteSavedView)
}

func addSavedViewIDParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{
		Name: "id", In: "path", Required: true, Description: "Saved View ID",
		Schema: &huma.Schema{Type: huma.TypeInteger, Format: "int64"},
	})
}

func addSavedViewIfMatchParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{
		Name: "If-Match", In: "header", Required: true,
		Description: "Strong ETag returned by the latest Saved View read",
		Schema:      &huma.Schema{Type: huma.TypeString},
	})
}

func addSavedViewETagHeader(response *huma.Response) {
	response.Headers = map[string]*huma.Param{
		"ETag": {
			Description: "Strong Saved View revision tag for optimistic concurrency",
			Schema:      &huma.Schema{Type: huma.TypeString},
		},
	}
}

func addErrorResponses(api huma.API, responses map[string]*huma.Response, statuses ...int) {
	for _, status := range statuses {
		responses[httpStatusKey(status)] = errorResponseFor(api)
	}
}

func (s *Server) handleListSavedViews(w http.ResponseWriter, r *http.Request) {
	if !s.requireSavedViewStore(w) {
		return
	}
	views, err := s.savedViewStore.ListSavedViews(r.Context())
	if err != nil {
		s.writeSavedViewError(w, err)
		return
	}
	response := SavedViewsResponse{SavedViews: make([]SavedView, 0, len(views))}
	for i := range views {
		view, err := savedViewResponse(&views[i])
		if err != nil {
			writeError(w, http.StatusInternalServerError, "saved_view_read_failed", err.Error())
			return
		}
		response.SavedViews = append(response.SavedViews, view)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleCreateSavedView(w http.ResponseWriter, r *http.Request) {
	if !s.requireSavedViewStore(w) {
		return
	}
	var request CreateSavedViewRequest
	fields, ok := decodeSavedViewRequest(w, r, &request)
	if !ok {
		return
	}
	canonicalState, present := fields["canonical_state"]
	if !present || isJSONNull(canonicalState) {
		writeError(w, http.StatusBadRequest, "bad_request", "canonical_state is required and must not be null")
		return
	}
	input := savedViewInput(request.Name, request.Description, canonicalState, request.SchemaVersion)
	created, err := s.savedViewStore.CreateSavedView(r.Context(), input)
	if err != nil {
		s.writeSavedViewError(w, err)
		return
	}
	response, err := savedViewResponse(created)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "saved_view_read_failed", err.Error())
		return
	}
	w.Header().Set("ETag", savedViewETag(*created))
	w.Header().Set("Location", savedViewsPath+"/"+strconv.FormatInt(created.ID, 10))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) handleGetSavedView(w http.ResponseWriter, r *http.Request) {
	if !s.requireSavedViewStore(w) {
		return
	}
	id, ok := savedViewID(w, r)
	if !ok {
		return
	}
	view, err := s.savedViewStore.GetSavedView(r.Context(), id)
	if err != nil {
		s.writeSavedViewError(w, err)
		return
	}
	response, err := savedViewResponse(view)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "saved_view_read_failed", err.Error())
		return
	}
	w.Header().Set("ETag", savedViewETag(*view))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handlePatchSavedView(w http.ResponseWriter, r *http.Request) {
	if !s.requireSavedViewStore(w) {
		return
	}
	id, ok := savedViewID(w, r)
	if !ok {
		return
	}
	revision, ok := savedViewIfMatch(w, r, id)
	if !ok {
		return
	}
	var request PatchSavedViewRequest
	fields, ok := decodeSavedViewRequest(w, r, &request)
	if !ok {
		return
	}
	if canonicalState, present := fields["canonical_state"]; present && isJSONNull(canonicalState) {
		writeError(w, http.StatusBadRequest, "bad_request", "canonical_state must not be null")
		return
	}
	if request.Name == nil && request.Description == nil && request.CanonicalState == nil && request.SchemaVersion == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "At least one Saved View field is required")
		return
	}
	current, err := s.savedViewStore.GetSavedView(r.Context(), id)
	if err != nil {
		s.writeSavedViewError(w, err)
		return
	}
	if current.Revision != revision {
		s.writeSavedViewError(w, store.ErrSavedViewRevisionConflict)
		return
	}
	input := store.SavedViewInput{
		Name: current.Name, Description: current.Description,
		CanonicalState: current.CanonicalState, SchemaVersion: current.SchemaVersion,
	}
	if request.Name != nil {
		input.Name = *request.Name
	}
	if request.Description != nil {
		input.Description = normalizedDescription(request.Description)
	}
	if request.CanonicalState != nil {
		input.CanonicalState = append(json.RawMessage(nil), fields["canonical_state"]...)
	}
	if request.SchemaVersion != nil {
		input.SchemaVersion = *request.SchemaVersion
	}
	updated, err := s.savedViewStore.UpdateSavedView(r.Context(), id, revision, input)
	if err != nil {
		s.writeSavedViewError(w, err)
		return
	}
	response, err := savedViewResponse(updated)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "saved_view_read_failed", err.Error())
		return
	}
	w.Header().Set("ETag", savedViewETag(*updated))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleDeleteSavedView(w http.ResponseWriter, r *http.Request) {
	if !s.requireSavedViewStore(w) {
		return
	}
	id, ok := savedViewID(w, r)
	if !ok {
		return
	}
	revision, ok := savedViewIfMatch(w, r, id)
	if !ok {
		return
	}
	if err := s.savedViewStore.DeleteSavedView(r.Context(), id, revision); err != nil {
		s.writeSavedViewError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requireSavedViewStore(w http.ResponseWriter) bool {
	if s.savedViewStore != nil {
		return true
	}
	writeError(w, http.StatusServiceUnavailable, "saved_views_unavailable", "Saved Views are unavailable")
	return false
}

func decodeSavedViewRequest(
	w http.ResponseWriter, r *http.Request, target any,
) (map[string]json.RawMessage, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid Saved View request")
		return nil, false
	}
	if err := jsonexact.Validate(body, target); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid Saved View request")
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid Saved View request")
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid Saved View request")
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid Saved View request")
		return nil, false
	}
	return fields, true
}

func isJSONNull(value json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

func savedViewInput(
	name string, description *string, state json.RawMessage, schemaVersion int,
) store.SavedViewInput {
	return store.SavedViewInput{
		Name: name, Description: normalizedDescription(description),
		CanonicalState: append(json.RawMessage(nil), state...), SchemaVersion: schemaVersion,
	}
}

func normalizedDescription(description *string) *string {
	if description == nil || strings.TrimSpace(*description) == "" {
		return nil
	}
	value := *description
	return &value
}

func savedViewResponse(view *store.SavedView) (SavedView, error) {
	var state store.SavedViewStateEnvelope
	decoder := json.NewDecoder(strings.NewReader(string(view.CanonicalState)))
	decoder.UseNumber()
	if err := decoder.Decode(&state); err != nil {
		return SavedView{}, fmt.Errorf("decode Saved View %d canonical state: %w", view.ID, err)
	}
	return SavedView{
		ID: view.ID, Name: view.Name, Description: view.Description,
		CanonicalState: state, SchemaVersion: view.SchemaVersion, Revision: view.Revision,
		CreatedAt: view.CreatedAt, UpdatedAt: view.UpdatedAt,
	}, nil
}

func savedViewID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_saved_view_id", "Saved View ID must be a positive integer")
		return 0, false
	}
	return id, true
}

func savedViewETag(view store.SavedView) string {
	return fmt.Sprintf(`"saved-view-%d-r%d"`, view.ID, view.Revision)
}

func savedViewIfMatch(w http.ResponseWriter, r *http.Request, id int64) (int64, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 || (len(values) == 1 && strings.TrimSpace(values[0]) == "") {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "If-Match is required")
		return 0, false
	}
	if len(values) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match must contain exactly one revision tag")
		return 0, false
	}
	prefix := fmt.Sprintf(`"saved-view-%d-r`, id)
	value := strings.TrimSpace(values[0])
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, `"`) {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match is not a Saved View revision tag")
		return 0, false
	}
	revision, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(value, prefix), `"`), 10, 64)
	if err != nil || revision <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match is not a Saved View revision tag")
		return 0, false
	}
	return revision, true
}

func (s *Server) writeSavedViewError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrSavedViewNotFound):
		writeError(w, http.StatusNotFound, "saved_view_not_found", "Saved View not found")
	case errors.Is(err, store.ErrSavedViewNameConflict):
		writeError(w, http.StatusConflict, "saved_view_name_conflict", "A Saved View with that name already exists")
	case errors.Is(err, store.ErrSavedViewRevisionConflict):
		writeError(w, http.StatusConflict, "saved_view_revision_conflict", "Saved View changed; reload and retry")
	case errors.Is(err, store.ErrSavedViewInvalidState),
		errors.Is(err, store.ErrSavedViewUnsupportedSchemaVersion):
		writeError(w, http.StatusBadRequest, "invalid_saved_view", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "saved_view_failed", "Saved View operation failed")
	}
}
