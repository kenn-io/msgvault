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

const attributeDefinitionsPath = "/api/v1/attribute-definitions"

var attributeReservedRequestKeys = map[string]struct{}{
	"is_unique": {},
	"unique":    {},
}

// AttributeDefinitionStore is the definition capability required by the API.
type AttributeDefinitionStore interface {
	ListAttributeDefinitionsContext(
		ctx context.Context, filter store.AttributeDefinitionFilter,
	) ([]store.AttributeDefinition, error)
	GetAttributeDefinitionContext(
		ctx context.Context, id int64,
	) (*store.AttributeDefinition, error)
	GetAttributeDefinitionBySlugContext(
		ctx context.Context, objectType store.AttributeObjectType, slug string,
	) (*store.AttributeDefinition, error)
	CreateAttributeDefinitionContext(
		ctx context.Context, input store.AttributeDefinitionInput,
	) (*store.AttributeDefinition, error)
	UpdateAttributeDefinitionContext(
		ctx context.Context, id, expectedRevision int64,
		update store.AttributeDefinitionUpdate,
	) (*store.AttributeDefinition, error)
	DeleteAttributeDefinitionContext(ctx context.Context, id, expectedRevision int64) error
}

// AttributeDefinitionsResponse wraps a definition listing.
type AttributeDefinitionsResponse struct {
	Definitions []store.AttributeDefinition `json:"definitions"`
}

// CreateAttributeDefinitionRequest is the user-creatable definition subset.
type CreateAttributeDefinitionRequest struct {
	ObjectType    string                  `json:"object_type" enum:"person,organization"`
	Slug          string                  `json:"slug"`
	Label         string                  `json:"label"`
	Description   *string                 `json:"description,omitempty"`
	ValueType     string                  `json:"value_type"`
	FieldType     string                  `json:"field_type"`
	RecordTarget  *string                 `json:"record_target,omitempty"`
	Cardinality   string                  `json:"cardinality,omitempty" enum:"single,multi"`
	DisplayOrder  int64                   `json:"display_order,omitempty"`
	IsRequired    bool                    `json:"is_required,omitempty"`
	IsSearchable  bool                    `json:"is_searchable,omitempty"`
	IsAudited     bool                    `json:"is_audited,omitempty"`
	Options       *store.AttributeOptions `json:"options,omitempty"`
	VCardProperty *string                 `json:"vcard_property,omitempty"`
}

// StoreInput maps the request onto the store's input shape, fixing the
// capability flags the API grants to user-created definitions. The CLI
// dry-run path reuses it so local validation matches the create path.
func (r CreateAttributeDefinitionRequest) StoreInput(
	universalID string,
) store.AttributeDefinitionInput {
	return store.AttributeDefinitionInput{
		UniversalID: universalID, ObjectType: store.AttributeObjectType(r.ObjectType),
		Slug: r.Slug, Label: r.Label, Description: r.Description,
		ValueType:    store.AttributeValueType(r.ValueType),
		FieldType:    store.AttributeFieldType(r.FieldType),
		RecordTarget: r.RecordTarget, Cardinality: store.AttributeCardinality(r.Cardinality),
		DisplayOrder: r.DisplayOrder, IsRequired: r.IsRequired,
		Ownership: store.AttributeOwnershipUser, UICreatable: true, UIEditable: true,
		APIMutable: true, IsSearchable: r.IsSearchable, IsAudited: r.IsAudited,
		IsDeletable: true, Options: r.Options, VCardProperty: r.VCardProperty,
	}
}

// PatchAttributeDefinitionRequest carries mutable definition fields.
type PatchAttributeDefinitionRequest struct {
	Label        *string `json:"label,omitempty"`
	Description  *string `json:"description,omitempty" nullable:"true"`
	DisplayOrder *int64  `json:"display_order,omitempty"`
	IsActive     *bool   `json:"is_active,omitempty"`
}

func (s *Server) registerAttributeDefinitionRoutes(api huma.API) {
	list := rawAPIV1Operation("listAttributeDefinitions", http.MethodGet,
		"/attribute-definitions", "List portable attribute definitions")
	list.Parameters = append(list.Parameters,
		queryStringParam("object_type", "Filter by object type", false),
		queryBooleanParam("include_hidden", "Include deactivated definitions"))
	list.Responses = jsonResponsesFor[AttributeDefinitionsResponse](api)
	addErrorResponses(api, list.Responses, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListAttributeDefinitions)

	create := rawAPIV1Operation("createAttributeDefinition", http.MethodPost,
		"/attribute-definitions", "Create a user attribute definition")
	create.RequestBody = jsonRequestBodyFor[CreateAttributeDefinitionRequest](api)
	create.Responses = jsonResponsesFor[store.AttributeDefinition](api, http.StatusCreated)
	addAttributeDefinitionETagHeader(create.Responses[httpStatusKey(http.StatusCreated)])
	addErrorResponses(api, create.Responses, http.StatusBadRequest,
		http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, create, s.handleCreateAttributeDefinition)

	get := rawAPIV1Operation("getAttributeDefinition", http.MethodGet,
		"/attribute-definitions/{id}", "Get one attribute definition")
	addAttributeDefinitionIDParameter(&get)
	get.Responses = jsonResponsesFor[store.AttributeDefinition](api)
	addAttributeDefinitionETagHeader(get.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, get.Responses, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, get, s.handleGetAttributeDefinition)

	patch := rawAPIV1Operation("patchAttributeDefinition", http.MethodPatch,
		"/attribute-definitions/{id}", "Update mutable attribute definition fields")
	addAttributeDefinitionIDParameter(&patch)
	addAttributeDefinitionIfMatchParameter(&patch)
	patch.RequestBody = jsonRequestBodyFor[PatchAttributeDefinitionRequest](api)
	patch.Responses = jsonResponsesFor[store.AttributeDefinition](api)
	addAttributeDefinitionETagHeader(patch.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, patch.Responses, http.StatusBadRequest, http.StatusConflict,
		http.StatusNotFound, http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, patch, s.handlePatchAttributeDefinition)

	remove := rawAPIV1Operation("deleteAttributeDefinition", http.MethodDelete,
		"/attribute-definitions/{id}", "Delete a user attribute definition")
	addAttributeDefinitionIDParameter(&remove)
	addAttributeDefinitionIfMatchParameter(&remove)
	remove.Responses = rawHumaResponses(http.StatusNoContent)
	remove.Responses["default"] = errorResponseFor(api)
	addErrorResponses(api, remove.Responses, http.StatusBadRequest, http.StatusConflict,
		http.StatusNotFound, http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, remove, s.handleDeleteAttributeDefinition)
}

func (s *Server) handleListAttributeDefinitions(w http.ResponseWriter, r *http.Request) {
	definitions, ok := s.attributeDefinitionStore(w)
	if !ok {
		return
	}
	filter := store.AttributeDefinitionFilter{}
	if raw := strings.TrimSpace(r.URL.Query().Get("object_type")); raw != "" {
		if raw != string(store.AttributeObjectPerson) &&
			raw != string(store.AttributeObjectOrganization) {
			writeError(w, http.StatusBadRequest, "invalid_object_type",
				"object_type must be person or organization")
			return
		}
		filter.ObjectType = store.AttributeObjectType(raw)
	}
	includeHidden, _, err := queryBool(r, "include_hidden")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	filter.IncludeHidden = includeHidden
	listed, err := definitions.ListAttributeDefinitionsContext(r.Context(), filter)
	if err != nil {
		s.writeAttributeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, AttributeDefinitionsResponse{Definitions: listed})
}

func (s *Server) handleCreateAttributeDefinition(w http.ResponseWriter, r *http.Request) {
	definitions, ok := s.attributeDefinitionStore(w)
	if !ok {
		return
	}
	var request CreateAttributeDefinitionRequest
	if !decodeAttributeRequest(w, r, &request) {
		return
	}
	universalID, err := store.NewAttributeUniversalID()
	if err != nil {
		s.writeAttributeError(w, err)
		return
	}
	created, err := definitions.CreateAttributeDefinitionContext(
		r.Context(), request.StoreInput(universalID))
	if err != nil {
		s.writeAttributeError(w, err)
		return
	}
	w.Header().Set("Location",
		attributeDefinitionsPath+"/"+strconv.FormatInt(created.ID, 10))
	writeAttributeDefinition(w, http.StatusCreated, created)
}

func (s *Server) handleGetAttributeDefinition(w http.ResponseWriter, r *http.Request) {
	definitions, ok := s.attributeDefinitionStore(w)
	if !ok {
		return
	}
	id, ok := attributeDefinitionID(w, r)
	if !ok {
		return
	}
	definition, err := definitions.GetAttributeDefinitionContext(r.Context(), id)
	if err != nil {
		s.writeAttributeError(w, err)
		return
	}
	writeAttributeDefinition(w, http.StatusOK, definition)
}

func (s *Server) handlePatchAttributeDefinition(w http.ResponseWriter, r *http.Request) {
	definitions, ok := s.attributeDefinitionStore(w)
	if !ok {
		return
	}
	id, ok := attributeDefinitionID(w, r)
	if !ok {
		return
	}
	revision, ok := attributeDefinitionIfMatch(w, r, id)
	if !ok {
		return
	}
	var request PatchAttributeDefinitionRequest
	fields, ok := decodeAttributeRequestFields(w, r, &request)
	if !ok {
		return
	}
	update := store.AttributeDefinitionUpdate{
		Label: request.Label, DisplayOrder: request.DisplayOrder, IsActive: request.IsActive,
	}
	if raw, present := fields["description"]; present {
		description := request.Description
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			description = nil
		}
		update.Description = &description
	}
	updated, err := definitions.UpdateAttributeDefinitionContext(
		r.Context(), id, revision, update)
	if err != nil {
		s.writeAttributeError(w, err)
		return
	}
	writeAttributeDefinition(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteAttributeDefinition(w http.ResponseWriter, r *http.Request) {
	definitions, ok := s.attributeDefinitionStore(w)
	if !ok {
		return
	}
	id, ok := attributeDefinitionID(w, r)
	if !ok {
		return
	}
	revision, ok := attributeDefinitionIfMatch(w, r, id)
	if !ok {
		return
	}
	if err := definitions.DeleteAttributeDefinitionContext(
		r.Context(), id, revision); err != nil {
		s.writeAttributeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) attributeDefinitionStore(
	w http.ResponseWriter,
) (AttributeDefinitionStore, bool) {
	definitions, ok := s.store.(AttributeDefinitionStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "attributes_unavailable",
			"Attribute definitions are unavailable")
	}
	return definitions, ok
}

func (s *Server) writeAttributeError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrAttributeUniquenessUnsupported):
		writeError(w, http.StatusBadRequest, "attribute_uniqueness_unsupported", err.Error())
	case errors.Is(err, store.ErrAttributeDefinitionInvalid),
		errors.Is(err, store.ErrAttributeValueInvalid):
		writeError(w, http.StatusBadRequest, "attribute_invalid", err.Error())
	case errors.Is(err, store.ErrAttributeDefinitionNotFound):
		writeError(w, http.StatusNotFound, "attribute_definition_not_found",
			"Attribute definition not found")
	case errors.Is(err, store.ErrAttributeValueNotFound):
		writeError(w, http.StatusNotFound, "attribute_value_not_found", "Attribute value not found")
	case errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusNotFound, "person_profile_not_found", "Person profile not found")
	case errors.Is(err, store.ErrAttributeDefinitionSlugConflict):
		writeError(w, http.StatusConflict, "attribute_definition_slug_conflict",
			"An attribute definition with that slug already exists")
	case errors.Is(err, store.ErrAttributeDefinitionUniversalIDConflict):
		writeError(w, http.StatusConflict, "attribute_definition_universal_id_conflict",
			"An attribute definition with that universal identifier already exists")
	case errors.Is(err, store.ErrAttributeDefinitionRevisionConflict):
		writeError(w, http.StatusConflict, "attribute_definition_revision_conflict",
			"Attribute definition changed; reload and retry")
	case errors.Is(err, store.ErrAttributeDefinitionNotDeletable):
		writeError(w, http.StatusConflict, "attribute_definition_not_deletable",
			"This attribute definition cannot be deleted")
	case errors.Is(err, store.ErrAttributeDefinitionHasValues):
		writeError(w, http.StatusConflict, "attribute_definition_has_values",
			"This attribute definition still has stored values, including "+
				"superseded history; definitions with recorded values cannot be deleted")
	case errors.Is(err, store.ErrAttributeDefinitionNotWritable):
		writeError(w, http.StatusConflict, "attribute_definition_not_writable", err.Error())
	case errors.Is(err, store.ErrAttributeDefinitionInactive):
		writeError(w, http.StatusConflict, "attribute_definition_inactive", err.Error())
	case errors.Is(err, store.ErrAttributeValueConflict):
		writeError(w, http.StatusConflict, "attribute_value_conflict",
			"Attribute value changed; reload and retry")
	default:
		s.logger.Error("attribute operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "attribute_failed",
			"Attribute operation failed")
	}
}

func writeAttributeDefinition(
	w http.ResponseWriter, status int, definition *store.AttributeDefinition,
) {
	w.Header().Set("ETag", attributeDefinitionETag(*definition))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, definition)
}

func attributeDefinitionETag(definition store.AttributeDefinition) string {
	return fmt.Sprintf(`"attribute-definition-%d-r%d"`, definition.ID, definition.Revision)
}

func addAttributeDefinitionIDParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{
		Name: "id", In: "path", Required: true, Description: "Attribute definition ID",
		Schema: &huma.Schema{Type: huma.TypeInteger, Format: "int64"},
	})
}

func addAttributeDefinitionIfMatchParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{
		Name: "If-Match", In: "header", Required: true,
		Description: "Strong ETag returned by the latest definition read",
		Schema:      &huma.Schema{Type: huma.TypeString},
	})
}

func addAttributeDefinitionETagHeader(response *huma.Response) {
	response.Headers = map[string]*huma.Param{
		"ETag": {
			Description: "Strong attribute definition revision tag",
			Schema:      &huma.Schema{Type: huma.TypeString},
		},
	}
}

func attributeDefinitionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_attribute_definition_id",
			"Attribute definition ID must be a positive integer")
		return 0, false
	}
	return id, true
}

func attributeDefinitionIfMatch(
	w http.ResponseWriter, r *http.Request, id int64,
) (int64, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 || (len(values) == 1 && strings.TrimSpace(values[0]) == "") {
		writeError(w, http.StatusPreconditionRequired, "if_match_required",
			"If-Match is required")
		return 0, false
	}
	if len(values) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_if_match",
			"If-Match must contain exactly one revision tag")
		return 0, false
	}
	prefix := fmt.Sprintf(`"attribute-definition-%d-r`, id)
	value := strings.TrimSpace(values[0])
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, `"`) {
		writeError(w, http.StatusBadRequest, "invalid_if_match",
			"If-Match is not an attribute definition revision tag")
		return 0, false
	}
	revision, err := strconv.ParseInt(
		strings.TrimSuffix(strings.TrimPrefix(value, prefix), `"`), 10, 64)
	if err != nil || revision <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_if_match",
			"If-Match is not an attribute definition revision tag")
		return 0, false
	}
	return revision, true
}

func decodeAttributeRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	_, ok := decodeAttributeRequestFields(w, r, target)
	return ok
}

func decodeAttributeRequestFields(
	w http.ResponseWriter, r *http.Request, target any,
) (map[string]json.RawMessage, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid attribute request")
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"Attribute request must be a JSON object")
		return nil, false
	}
	for key := range attributeReservedRequestKeys {
		if _, present := fields[key]; present {
			writeError(w, http.StatusBadRequest, "attribute_uniqueness_unsupported",
				store.ErrAttributeUniquenessUnsupported.Error())
			return nil, false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"Invalid attribute request: "+err.Error())
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"Attribute request must contain one JSON object")
		return nil, false
	}
	return fields, true
}
