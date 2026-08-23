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

// SetPersonAttributeRequest carries a typed value and its provenance.
type SetPersonAttributeRequest struct {
	Value           store.AttributeValue `json:"value"`
	Ordinal         *int64               `json:"ordinal,omitempty"`
	Source          string               `json:"source,omitempty" enum:"user,carddav_import,vcard_import,archive_observation,extraction,enrichment,system"`
	SourceRef       *string              `json:"source_ref,omitempty"`
	Confidence      *float64             `json:"confidence,omitempty"`
	Actor           *string              `json:"actor,omitempty"`
	ActiveFrom      *time.Time           `json:"active_from,omitempty"`
	ActiveUntil     *time.Time           `json:"active_until,omitempty"`
	ExpectedValueID *int64               `json:"expected_value_id,omitempty"`
}

func (s *Server) registerPersonAttributeRoutes(api huma.API) {
	list := rawAPIV1Operation("listPersonAttributes", http.MethodGet,
		"/people/{id}/attributes", "List a person's typed attributes")
	addPersonIDParameter(&list)
	list.Parameters = append(list.Parameters,
		queryBooleanParam("history", "Include superseded values"),
		queryStringParam("slug", "Restrict the response to one definition slug", false))
	list.Responses = jsonResponsesFor[PersonAttributesResponse](api)
	addErrorResponses(api, list.Responses, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListPersonAttributes)

	set := rawAPIV1Operation("setPersonAttribute", http.MethodPut,
		"/people/{id}/attributes/{slug}", "Set a person's attribute value")
	addPersonIDParameter(&set)
	addAttributeSlugParameter(&set)
	set.Parameters = append(set.Parameters,
		queryBooleanParam("dry_run", "Validate and preview without writing"))
	set.RequestBody = jsonRequestBodyFor[SetPersonAttributeRequest](api)
	set.Responses = jsonResponsesFor[store.PersonAttributeWrite](api)
	addErrorResponses(api, set.Responses, http.StatusBadRequest, http.StatusConflict,
		http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, set, s.handleSetPersonAttribute)

	clearOperation := rawAPIV1Operation("clearPersonAttribute", http.MethodDelete,
		"/people/{id}/attributes/{slug}", "Supersede a person's attribute value")
	addPersonIDParameter(&clearOperation)
	addAttributeSlugParameter(&clearOperation)
	clearOperation.Parameters = append(clearOperation.Parameters,
		queryIntegerParam("ordinal", "Ordinal for a multi-valued definition"),
		queryIntegerParam("expected_value_id",
			"Compare-and-swap: the current value ID expected to be superseded"),
		queryBooleanParam("dry_run", "Validate and preview without writing"))
	clearOperation.Responses = jsonResponsesFor[store.PersonAttributeWrite](api)
	addErrorResponses(api, clearOperation.Responses, http.StatusBadRequest, http.StatusConflict,
		http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, clearOperation, s.handleClearPersonAttribute)
}

func addAttributeSlugParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{
		Name: "slug", In: pathKey, Required: true,
		Description: "Immutable attribute definition slug",
		Schema:      &huma.Schema{Type: huma.TypeString},
	})
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
	includeHistory, _, err := queryBool(r, "history")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if slug != "" {
		if err := store.ValidateAttributeSlug(slug); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_attribute_slug", err.Error())
			return
		}
	}
	if _, err := s.requirePersonForAttributes(w, r, personID); err != nil {
		return
	}
	definitions, err := attributes.ListAttributeDefinitionsContext(r.Context(),
		store.AttributeDefinitionFilter{
			ObjectType: store.AttributeObjectPerson, IncludeHidden: true})
	if err != nil {
		s.writeAttributeError(w, err)
		return
	}
	values, err := attributes.ListPersonAttributeValuesContext(r.Context(), personID,
		store.PersonAttributeQuery{DefinitionSlug: slug, IncludeHistory: includeHistory})
	if err != nil {
		s.writeAttributeError(w, err)
		return
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
	dryRun, _, err := queryBool(r, "dry_run")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	var request SetPersonAttributeRequest
	if !decodeAttributeRequest(w, r, &request) {
		return
	}
	if request.ExpectedValueID != nil && *request.ExpectedValueID < 1 {
		writeError(w, http.StatusBadRequest, "invalid_expected_value_id",
			"expected_value_id must be a positive integer")
		return
	}
	source := store.Provenance(strings.TrimSpace(request.Source))
	if source == "" {
		source = store.ProvenanceUser
	}
	write, err := attributes.SetPersonAttributeValueContext(r.Context(),
		store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: slug, Ordinal: request.Ordinal,
			Value: request.Value, ActiveFrom: request.ActiveFrom,
			ActiveUntil: request.ActiveUntil, Source: source, SourceRef: request.SourceRef,
			Confidence: request.Confidence, Actor: request.Actor,
			ExpectedValueID: request.ExpectedValueID, DryRun: dryRun,
		})
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
	dryRun, _, err := queryBool(r, "dry_run")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	expectedValueID, hasExpectedValueID, err := queryInt64(r, "expected_value_id")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if hasExpectedValueID && expectedValueID < 1 {
		writeError(w, http.StatusBadRequest, "invalid_expected_value_id",
			"expected_value_id must be a positive integer")
		return
	}
	var expectedValueIDPtr *int64
	if hasExpectedValueID {
		expectedValueIDPtr = &expectedValueID
	}
	var ordinal *int64
	if raw := strings.TrimSpace(r.URL.Query().Get("ordinal")); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid_ordinal",
				"ordinal must be a non-negative integer")
			return
		}
		ordinal = &parsed
	}
	write, err := attributes.SupersedePersonAttributeValueContext(r.Context(),
		store.PersonAttributeSupersedeInput{
			PersonID: personID, DefinitionSlug: slug, Ordinal: ordinal,
			ExpectedValueID: expectedValueIDPtr, DryRun: dryRun,
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
	return person, nil
}
