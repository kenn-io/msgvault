package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

var errPersonsUnavailable = errors.New("person profiles are unavailable")

// PersonAttributeStore is the value capability required by the API.
type PersonAttributeStore interface {
	AttributeDefinitionStore
	ListPersonAttributeValuesContext(
		ctx context.Context, personID int64, query store.PersonAttributeQuery,
	) ([]store.PersonAttributeValue, error)
	SetPersonAttributeValueContext(
		ctx context.Context, input store.PersonAttributeValueInput,
	) (*store.PersonAttributeWrite, error)
	AppendPersonNoteContext(
		ctx context.Context, input store.PersonNoteAppendInput,
	) (*store.PersonAttributeWrite, error)
	SupersedePersonAttributeValueContext(
		ctx context.Context, input store.PersonAttributeSupersedeInput,
	) (*store.PersonAttributeWrite, error)
}

// PersonAttributeGroup pairs a definition with current and historical values.
type PersonAttributeGroup struct {
	Definition store.AttributeDefinition    `json:"definition"`
	Current    []store.PersonAttributeValue `json:"current"`
	History    []store.PersonAttributeValue `json:"history,omitempty"`
}

// PersonAttributesResponse is the grouped attribute read model.
type PersonAttributesResponse struct {
	PersonID   int64                  `json:"person_id"`
	Attributes []PersonAttributeGroup `json:"attributes"`
}

// PersonAttributeConflictResponse gives optimistic clients the current value
// needed to review and retry a lost compare-and-swap write.
type PersonAttributeConflictResponse struct {
	Error          string                      `json:"error"`
	Message        string                      `json:"message,omitempty"`
	CurrentValueID *int64                      `json:"current_value_id,omitzero" nullable:"false"`
	CurrentValue   *store.PersonAttributeValue `json:"current_value,omitzero" nullable:"false"`
}

// SetPersonAttributeRequest carries a typed value and its provenance.
type SetPersonAttributeRequest struct {
	Value           store.AttributeValue `json:"value"`
	Ordinal         *int64               `json:"ordinal,omitzero" nullable:"false"`
	Source          string               `json:"source,omitempty" enum:"user,carddav_import,vcard_import,archive_observation,extraction,enrichment,system"`
	SourceRef       *string              `json:"source_ref,omitzero" nullable:"false"`
	Confidence      *float64             `json:"confidence,omitzero" nullable:"false"`
	Actor           *string              `json:"actor,omitzero" nullable:"false"`
	ActiveFrom      *time.Time           `json:"active_from,omitempty"`
	ActiveUntil     *time.Time           `json:"active_until,omitempty"`
	ExpectedValueID *int64               `json:"expected_value_id,omitzero" nullable:"false" minimum:"0" doc:"Current value ID, or zero to require an empty slot; required for delegated edits. Multi-valued creation requires an explicit ordinal."`
}

// AppendPersonNoteRequest carries one note fragment and its provenance.
type AppendPersonNoteRequest struct {
	Text       string   `json:"text"`
	Source     string   `json:"source,omitempty" enum:"user,carddav_import,vcard_import,archive_observation,extraction,enrichment,system"`
	SourceRef  *string  `json:"source_ref,omitzero" nullable:"false"`
	Confidence *float64 `json:"confidence,omitzero" nullable:"false"`
	Actor      *string  `json:"actor,omitzero" nullable:"false"`
}

func (s *Server) registerPersonAttributeRoutes(api huma.API) {
	list := rawAPIV1Operation("listPersonAttributes", http.MethodGet,
		"/people/{id}/attributes", "List a person's typed attributes")
	addPersonIDParameter(&list)
	list.Parameters = append(list.Parameters,
		queryBooleanParam("history", "Include superseded values"),
		queryStringParam("slug", "Restrict the response to one definition slug", false),
		queryStringParam("universal_id",
			"Restrict the response to one portable definition identifier", false))
	list.Responses = jsonResponsesFor[PersonAttributesResponse](api)
	addPersonETagHeader(list.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, list.Responses, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListPersonAttributes)

	set := rawAPIV1Operation("setPersonAttribute", http.MethodPut,
		"/people/{id}/attributes/{slug}", "Set a person's attribute value")
	addPersonIDParameter(&set)
	addAttributeSlugParameter(&set)
	addAttributePersonIfMatchParameter(&set)
	set.Parameters = append(set.Parameters,
		queryBooleanParam("dry_run", "Validate and preview without writing"))
	set.RequestBody = jsonRequestBodyFor[SetPersonAttributeRequest](api)
	set.Responses = jsonResponsesFor[store.PersonAttributeWrite](api)
	addErrorResponses(api, set.Responses, http.StatusBadRequest, http.StatusConflict,
		http.StatusForbidden, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusPreconditionRequired, http.StatusNotImplemented, http.StatusServiceUnavailable)
	set.Responses[httpStatusKey(http.StatusConflict)] = personAttributeConflictResponse(api)
	registerRawHumaRoute(api, set, s.handleSetPersonAttribute)

	appendNote := rawAPIV1Operation("appendPersonNote", http.MethodPost,
		"/people/{id}/notes/append", "Append to a person's notes")
	addPersonIDParameter(&appendNote)
	appendNote.Parameters = append(appendNote.Parameters,
		queryBooleanParam("dry_run", "Validate and preview without writing"))
	appendNote.RequestBody = jsonRequestBodyFor[AppendPersonNoteRequest](api)
	appendNote.Responses = jsonResponsesFor[store.PersonAttributeWrite](api)
	addErrorResponses(api, appendNote.Responses, http.StatusBadRequest, http.StatusConflict,
		http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, appendNote, s.handleAppendPersonNote)

	clearOperation := rawAPIV1Operation("clearPersonAttribute", http.MethodDelete,
		"/people/{id}/attributes/{slug}", "Supersede a person's attribute value")
	addPersonIDParameter(&clearOperation)
	addAttributeSlugParameter(&clearOperation)
	addAttributePersonIfMatchParameter(&clearOperation)
	clearOperation.Parameters = append(clearOperation.Parameters,
		queryIntegerParam("ordinal", "Ordinal for a multi-valued definition"),
		queryIntegerParam("expected_value_id",
			"Compare-and-swap: the current value ID expected to be superseded"),
		queryBooleanParam("dry_run", "Validate and preview without writing"))
	clearOperation.Responses = jsonResponsesFor[store.PersonAttributeWrite](api)
	addErrorResponses(api, clearOperation.Responses, http.StatusBadRequest, http.StatusConflict,
		http.StatusForbidden, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusPreconditionRequired, http.StatusNotImplemented, http.StatusServiceUnavailable)
	clearOperation.Responses[httpStatusKey(http.StatusConflict)] = personAttributeConflictResponse(api)
	registerRawHumaRoute(api, clearOperation, s.handleClearPersonAttribute)
}

func personAttributeConflictResponse(api huma.API) *huma.Response {
	return &huma.Response{
		Description: http.StatusText(http.StatusConflict),
		Content: map[string]*huma.MediaType{
			applicationJSONMediaType: {Schema: schemaFor[PersonAttributeConflictResponse](api)},
		},
	}
}

func addAttributeSlugParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{
		Name: "slug", In: pathKey, Required: true,
		Description: "Immutable attribute definition slug",
		Schema:      &huma.Schema{Type: huma.TypeString},
	})
}

func addAttributePersonIfMatchParameter(operation *huma.Operation) {
	addPersonIfMatchParameter(operation)
	parameter := operation.Parameters[len(operation.Parameters)-1]
	parameter.Required = false
	parameter.Description = "Person revision tag from the attributes response. Required for delegated edits; checked when supplied by an owner. Value-slot CAS is separate."
}

func (s *Server) handleListPersonAttributes(w http.ResponseWriter, r *http.Request) {
	attributes, ok := s.personAttributeStore(w)
	if !ok {
		return
	}
	personID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	if !s.admitPersonTarget(w, r, personID, false) {
		return
	}
	if s.requestAuthentication(r).Mode == AuthModeDelegated {
		if _, ok := s.store.(ScopedPersonAttributeStore); !ok {
			writeError(w, http.StatusNotImplemented, "person_scope_unavailable", "Native scoped attribute access is unavailable")
			return
		}
	}
	includeHistory, _, err := queryBool(r, "history")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	universalID := strings.TrimSpace(r.URL.Query().Get("universal_id"))
	if slug != "" && universalID != "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"slug and universal_id cannot both be provided")
		return
	}
	if slug != "" {
		if err := store.ValidateAttributeSlug(slug); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_attribute_slug", err.Error())
			return
		}
	}
	person, err := s.requirePersonForAttributes(w, r, personID)
	if err != nil {
		return
	}
	definitions, err := attributes.ListAttributeDefinitionsContext(r.Context(),
		store.AttributeDefinitionFilter{
			ObjectType: store.AttributeObjectPerson, IncludeHidden: true})
	if err != nil {
		s.writeAttributeError(w, err)
		return
	}
	valueSlug := slug
	if universalID != "" {
		for _, definition := range definitions {
			if definition.UniversalID == universalID {
				valueSlug = definition.Slug
				break
			}
		}
	}
	values := []store.PersonAttributeValue{}
	if universalID == "" || valueSlug != "" {
		values, err = attributes.ListPersonAttributeValuesContext(r.Context(), personID,
			store.PersonAttributeQuery{
				DefinitionSlug: valueSlug, IncludeHistory: includeHistory,
			})
		if err != nil {
			s.writeAttributeError(w, err)
			return
		}
	}

	current := make(map[string][]store.PersonAttributeValue, len(definitions))
	history := make(map[string][]store.PersonAttributeValue, len(definitions))
	stored := make(map[string]bool, len(definitions))
	for _, value := range values {
		stored[value.DefinitionSlug] = true
		if value.ActiveUntil == nil && value.SupersededAt == nil {
			current[value.DefinitionSlug] = append(current[value.DefinitionSlug], value)
		}
		if includeHistory {
			history[value.DefinitionSlug] = append(history[value.DefinitionSlug], value)
		}
	}

	response := PersonAttributesResponse{
		PersonID: personID, Attributes: make([]PersonAttributeGroup, 0, len(definitions)),
	}
	for _, definition := range definitions {
		if slug != "" && definition.Slug != slug {
			continue
		}
		if universalID != "" && definition.UniversalID != universalID {
			continue
		}
		// Inactive definitions appear only while values remain stored under
		// them; hiding those values entirely would look like data loss.
		if !definition.IsActive && !stored[definition.Slug] {
			continue
		}
		group := PersonAttributeGroup{
			Definition: definition, Current: current[definition.Slug],
		}
		if group.Current == nil {
			group.Current = []store.PersonAttributeValue{}
		}
		if includeHistory {
			group.History = history[definition.Slug]
			if group.History == nil {
				group.History = []store.PersonAttributeValue{}
			}
		}
		response.Attributes = append(response.Attributes, group)
	}
	w.Header().Set(etagHeaderName, personETag(*person))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleSetPersonAttribute(w http.ResponseWriter, r *http.Request) {
	attributes, ok := s.personAttributeStore(w)
	if !ok {
		return
	}
	personID, slug, ok := personAttributeTarget(w, r)
	if !ok {
		return
	}
	if !s.admitPersonTarget(w, r, personID, true) {
		return
	}
	delegated := s.requestAuthentication(r).Mode == AuthModeDelegated
	var revision int64
	conditional := delegated || len(r.Header.Values(ifMatchHeaderName)) > 0
	if conditional {
		revision, ok = personIfMatch(w, r, personID)
		if !ok {
			return
		}
	}
	dryRun, _, err := queryBool(r, "dry_run")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var request SetPersonAttributeRequest
	if !decodeAttributeRequest(w, r, &request) {
		return
	}
	if request.ExpectedValueID != nil && *request.ExpectedValueID < 0 {
		writeError(w, http.StatusBadRequest, "invalid_expected_value_id",
			"expected_value_id must be a nonnegative integer")
		return
	}
	source := store.Provenance(strings.TrimSpace(request.Source))
	if source == "" {
		source = store.ProvenanceUser
	}
	if delegated {
		if request.ExpectedValueID == nil {
			writeError(w, http.StatusPreconditionRequired, "attribute_precondition_required", "expected_value_id is required; zero requires an empty slot")
			return
		}
		if source != store.ProvenanceUser || request.Actor != nil || request.SourceRef != nil || request.Confidence != nil || request.Value.Type == store.AttributeValueRecordReference || request.Value.RecordType != nil || request.Value.RecordID != nil {
			writeError(w, http.StatusBadRequest, "invalid_delegated_attribute", "Delegated attribute edits require user provenance and a value without record references")
			return
		}
	}
	input := store.PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: slug, Ordinal: request.Ordinal,
		Value: request.Value, ActiveFrom: request.ActiveFrom,
		ActiveUntil: request.ActiveUntil, Source: source, SourceRef: request.SourceRef,
		Confidence: request.Confidence, Actor: request.Actor,
		ExpectedValueID: request.ExpectedValueID, DryRun: dryRun,
	}
	var write *store.PersonAttributeWrite
	if conditional {
		backend, authorize, admitted := s.scopedPersonAttributeAdmission(w, r, personID, revision)
		if !admitted {
			return
		}
		if delegated {
			release, admitted := s.beginScopedPersonEdit(w, r)
			if !admitted {
				return
			}
			defer release()
			actor := "agent:" + s.requestAuthentication(r).Grant.ID
			input.Actor = &actor
		}
		write, err = backend.SetPersonAttributeValueAuthorizedContext(r.Context(), input, authorize)
	} else {
		write, err = attributes.SetPersonAttributeValueContext(r.Context(), input)
	}
	if err != nil {
		s.writeAttributeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, write)
}

func (s *Server) handleClearPersonAttribute(w http.ResponseWriter, r *http.Request) {
	attributes, ok := s.personAttributeStore(w)
	if !ok {
		return
	}
	personID, slug, ok := personAttributeTarget(w, r)
	if !ok {
		return
	}
	if !s.admitPersonTarget(w, r, personID, true) {
		return
	}
	delegated := s.requestAuthentication(r).Mode == AuthModeDelegated
	var revision int64
	conditional := delegated || len(r.Header.Values(ifMatchHeaderName)) > 0
	if conditional {
		revision, ok = personIfMatch(w, r, personID)
		if !ok {
			return
		}
	}
	query, ok := s.attributeClearQuery(w, r)
	if !ok {
		return
	}
	if delegated && query.expectedValueID == nil {
		writeError(w, http.StatusPreconditionRequired, "attribute_precondition_required", "expected_value_id is required")
		return
	}
	input := store.PersonAttributeSupersedeInput{
		PersonID: personID, DefinitionSlug: slug, Ordinal: query.ordinal,
		ExpectedValueID: query.expectedValueID, DryRun: query.dryRun,
	}
	var write *store.PersonAttributeWrite
	var err error
	if conditional {
		backend, authorize, admitted := s.scopedPersonAttributeAdmission(w, r, personID, revision)
		if !admitted {
			return
		}
		if delegated {
			release, admitted := s.beginScopedPersonEdit(w, r)
			if !admitted {
				return
			}
			defer release()
			actor := "agent:" + s.requestAuthentication(r).Grant.ID
			input.Actor = &actor
		}
		write, err = backend.SupersedePersonAttributeValueAuthorizedContext(r.Context(), input, authorize)
	} else {
		write, err = attributes.SupersedePersonAttributeValueContext(r.Context(), input)
	}
	if err != nil {
		s.writeAttributeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, write)
}

func (s *Server) handleAppendPersonNote(w http.ResponseWriter, r *http.Request) {
	attributes, ok := s.personAttributeStore(w)
	if !ok {
		return
	}
	personID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	dryRun, _, err := queryBool(r, "dry_run")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var request AppendPersonNoteRequest
	if !decodeAttributeRequest(w, r, &request) {
		return
	}
	source := store.Provenance(strings.TrimSpace(request.Source))
	if source == "" {
		source = store.ProvenanceUser
	}
	write, err := attributes.AppendPersonNoteContext(r.Context(), store.PersonNoteAppendInput{
		PersonID: personID, Text: request.Text, Source: source,
		SourceRef: request.SourceRef, Confidence: request.Confidence,
		Actor: request.Actor, DryRun: dryRun,
	})
	if err != nil {
		s.writeAttributeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, write)
}

func (s *Server) personAttributeStore(w http.ResponseWriter) (PersonAttributeStore, bool) {
	attributes, ok := s.store.(PersonAttributeStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "attributes_unavailable",
			"Person attributes are unavailable")
	}
	return attributes, ok
}

func personAttributeTarget(w http.ResponseWriter, r *http.Request) (int64, string, bool) {
	personID, ok := personProfileID(w, r)
	if !ok {
		return 0, "", false
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if err := store.ValidateAttributeSlug(slug); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_attribute_slug", err.Error())
		return 0, "", false
	}
	return personID, slug, true
}

// attributeClearParams holds the query parameters both attribute clear routes accept.
type attributeClearParams struct {
	ordinal, expectedValueID *int64
	dryRun                   bool
}

func (s *Server) attributeClearQuery(w http.ResponseWriter, r *http.Request) (attributeClearParams, bool) {
	var params attributeClearParams
	dryRun, _, err := queryBool(r, "dry_run")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return params, false
	}
	params.dryRun = dryRun
	expectedValueID, hasExpectedValueID, err := queryInt64(r, "expected_value_id")
	if err != nil {
		s.rejectBadParam(w, err)
		return params, false
	}
	if hasExpectedValueID {
		if expectedValueID < 1 {
			writeError(w, http.StatusBadRequest, "invalid_expected_value_id",
				"expected_value_id must be a positive integer")
			return params, false
		}
		params.expectedValueID = &expectedValueID
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("ordinal")); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid_ordinal",
				"ordinal must be a non-negative integer")
			return params, false
		}
		params.ordinal = &parsed
	}
	return params, true
}

func (s *Server) requirePersonForAttributes(
	w http.ResponseWriter, r *http.Request, personID int64,
) (*store.Person, error) {
	profiles, ok := s.store.(PersonProfileStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "persons_unavailable",
			"Person profiles are unavailable")
		return nil, errPersonsUnavailable
	}
	person, err := profiles.GetPersonContext(r.Context(), personID)
	if err != nil {
		s.writeAttributeError(w, err)
		return nil, err
	}
	if !s.admitPersonRead(w, r, person) {
		return nil, errPersonScopeDenied
	}
	return person, nil
}
