package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/personsearch"
)

const (
	peoplePath               = "/api/v1/people"
	defaultPersonSearchLimit = 20
	maximumPersonSearchLimit = 100
)

// PersonProfileStore is the feature-local capability for durable curated
// people. The daemon adapter passes these calls directly to *store.Store.
type PersonProfileStore interface {
	CreatePersonFromParticipantContext(ctx context.Context, participantID int64) (*store.Person, bool, error)
	GetPersonContext(ctx context.Context, id int64) (*store.Person, error)
	ListPersonsContext(ctx context.Context) ([]store.Person, error)
	DirectoryPeoplePageContext(ctx context.Context, query store.DirectoryPeopleQuery) (*store.DirectoryPeoplePage, error)
	UpdatePersonDisplayNameContext(
		ctx context.Context, id, expectedRevision int64, displayName *string,
	) (*store.Person, error)
	DeletePersonContext(ctx context.Context, id, expectedRevision int64) error
	PersonForParticipantsContext(ctx context.Context, participantIDs []int64) (*store.Person, error)
	MergePersonsContext(ctx context.Context, request store.PersonMergeRequest) (*store.PersonMergeResult, error)
	SplitPersonMergeContext(ctx context.Context, request store.PersonSplitRequest) (*store.PersonSplitResult, error)
	ListPersonMergesPageContext(
		ctx context.Context, personID int64, limit, offset int,
	) ([]store.PersonMergeSummary, error)
	GetPersonMergeContext(ctx context.Context, mergeID int64) (*store.PersonMergeDetail, error)
	GetPersonMergeSnapshotContext(ctx context.Context, mergeID int64) (*store.PersonMergeSnapshotResponse, error)
	DecidePersonMergeCandidateContext(
		ctx context.Context, request store.PersonMergeCandidateDecisionRequest,
	) (*store.PersonMergeCandidateDecisionResult, error)
}

type CreatePersonRequest struct {
	ParticipantID int64 `json:"participant_id"`
}

type PatchPersonRequest struct {
	DisplayName *string `json:"display_name" nullable:"true"`
}

type PeopleResponse struct {
	People []store.Person `json:"people"`
}

// DirectoryPeopleResponse is the non-sensitive, paginated Directory view of
// durable person roots.
type DirectoryPeopleResponse struct {
	People     []store.DirectoryPersonSummary `json:"people" nullable:"false"`
	NextCursor string                         `json:"next_cursor,omitempty"`
}

// PersonSearchEngine is the semantic people service consumed by the HTTP
// route. Production installs the concrete personsearch engine with the vector
// subsystem; tests can supply a focused service double.
type PersonSearchEngine interface {
	Search(ctx context.Context, query string, limit int) ([]personsearch.Result, error)
}

type PersonSearchRequest struct {
	Query string `json:"query" minLength:"1"`
	Limit int    `json:"limit,omitempty" minimum:"0" maximum:"100" default:"20"`
}

type PersonSearchResult struct {
	Person store.Person `json:"person"`
	Score  float64      `json:"score"`
}

type PersonSearchResponse struct {
	Results []PersonSearchResult `json:"results"`
}

func (s *Server) registerPersonProfileRoutes(api huma.API) {
	search := rawAPIV1Operation("searchPeople", http.MethodPost, "/people/search",
		"Search durable people semantically")
	search.Description = "Searches only the curated person vector corpus and returns durable person roots in relevance order."
	search.RequestBody = jsonRequestBodyFor[PersonSearchRequest](api)
	search.Responses = jsonResponsesFor[PersonSearchResponse](api)
	addErrorResponses(api, search.Responses, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, search, s.handleSemanticPersonSearch)

	list := rawAPIV1Operation("listPeople", http.MethodGet, "/people", "List durable person profiles")
	list.Description = "Durable people are curated profiles; /api/v1/participants exposes observed analytical groupings. " +
		"The listing is deliberately unpaginated: persons exist only through explicit promotion, so the set stays small."
	list.Responses = jsonResponsesFor[PeopleResponse](api)
	addErrorResponses(api, list.Responses, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListPeople)

	directory := rawAPIV1Operation("listDirectoryPeople", http.MethodGet, "/people/directory",
		"Query durable people for the Directory")
	directory.Description = "Returns one stable, non-sensitive page of promoted durable people."
	addDirectoryPeopleParameters(&directory)
	directory.Responses = jsonResponsesFor[DirectoryPeopleResponse](api)
	addErrorResponses(api, directory.Responses, http.StatusBadRequest, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, directory, s.handleDirectoryPeople)

	create := rawAPIV1Operation("createPerson", http.MethodPost, "/people", "Promote a participant cluster to a durable person")
	create.Description = "Returns 201 when a new person is created, or 200 when the cluster is already " +
		"represented by a person (idempotent re-promotion, which also binds any unbound cluster members)."
	create.RequestBody = jsonRequestBodyFor[CreatePersonRequest](api)
	create.Responses = jsonResponsesFor[store.Person](api, http.StatusOK, http.StatusCreated)
	addPersonETagHeader(create.Responses[httpStatusKey(http.StatusOK)])
	addPersonETagHeader(create.Responses[httpStatusKey(http.StatusCreated)])
	addErrorResponses(api, create.Responses, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, create, s.handleCreatePerson)

	get := rawAPIV1Operation("getPersonProfile", http.MethodGet, "/people/{id}", "Get a durable person profile")
	addPersonIDParameter(&get)
	get.Responses = jsonResponsesFor[store.Person](api)
	addPersonETagHeader(get.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, get.Responses, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, get, s.handleGetPersonProfile)

	patch := rawAPIV1Operation("patchPerson", http.MethodPatch, "/people/{id}", "Update a durable person's display name")
	addPersonIDParameter(&patch)
	addPersonIfMatchParameter(&patch)
	patch.RequestBody = jsonRequestBodyFor[PatchPersonRequest](api)
	patch.Responses = jsonResponsesFor[store.Person](api)
	addPersonETagHeader(patch.Responses[httpStatusKey(http.StatusOK)])
	addErrorResponses(api, patch.Responses, http.StatusConflict, http.StatusNotFound,
		http.StatusPreconditionRequired, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, patch, s.handlePatchPerson)

	remove := rawAPIV1Operation("deletePerson", http.MethodDelete, "/people/{id}", "Delete a durable person profile")
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

func (s *Server) handleSemanticPersonSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request PersonSearchRequest
	if !decodePersonRequest(w, r, &request) {
		return
	}
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" {
		writeError(w, http.StatusBadRequest, "invalid_query", "query must contain non-whitespace text")
		return
	}
	if request.Limit < 0 || request.Limit > maximumPersonSearchLimit {
		writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 0 and 100")
		return
	}
	if request.Limit == 0 {
		request.Limit = defaultPersonSearchLimit
	}

	if !s.vectorSearchPreflight(r.Context(), w) {
		return
	}
	status, _ := s.VectorStatus()
	if status != VectorStatusReady {
		s.writeVectorUnavailable(w)
		return
	}
	engine := s.personSearchComponent()
	if engine == nil {
		writeError(w, http.StatusServiceUnavailable, "vector_not_enabled",
			"Semantic person search is not configured on this server")
		return
	}
	results, err := engine.Search(r.Context(), request.Query, request.Limit)
	if err != nil {
		s.writePersonSearchError(w, err)
		return
	}
	response := PersonSearchResponse{Results: make([]PersonSearchResult, len(results))}
	for i, result := range results {
		response.Results[i] = PersonSearchResult{Person: result.Person, Score: result.Score}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) writePersonSearchError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, vector.ErrSemanticPersonEmbeddingsDisabled):
		writeError(w, http.StatusServiceUnavailable, "person_embeddings_disabled",
			"Semantic person search requires [vector.people] enabled = true")
	case errors.Is(err, vector.ErrSemanticPersonEmbeddingConsentRequired):
		writeError(w, http.StatusServiceUnavailable, "person_embedding_consent_required",
			"Semantic person search requires active exact consent; run `msgvault person provider consent --semantic-embeddings` to review and grant the current policy")
	case errors.Is(err, vector.ErrSemanticPersonEmbeddingPolicyUnavailable):
		s.logger.Error("semantic person embedding policy unavailable", "error", err)
		writeError(w, http.StatusServiceUnavailable, "person_embedding_policy_unavailable",
			"Semantic person search cannot verify the current semantic person embedding policy or exact consent; check the live configuration and consent store")
	case errors.Is(err, vector.ErrNotEnabled):
		writeError(w, http.StatusServiceUnavailable, "vector_not_enabled", "Vector search is not configured")
	case errors.Is(err, vector.ErrIndexStale):
		writeError(w, http.StatusServiceUnavailable, "index_stale", "The vector index does not match configured embedding settings")
	case errors.Is(err, personsearch.ErrPersonCoverageIncomplete):
		var coverageErr *personsearch.CoverageIncompleteError
		if errors.As(err, &coverageErr) && coverageErr.Rejected > 0 {
			writeError(w, http.StatusServiceUnavailable, "index_building", fmt.Sprintf(
				"The semantic person index has %d terminally rejected curated person profile record(s); run `msgvault embeddings resume --backstop` first to reconcile current profiles; if terminal rejections remain, correct or edit the affected current profiles so their source revisions change, then rerun the command",
				coverageErr.Rejected))
			return
		}
		writeError(w, http.StatusServiceUnavailable, "index_building",
			"The semantic person index is incomplete; run `msgvault embeddings resume --backstop` to embed curated person profiles")
	case errors.Is(err, vector.ErrIndexBuilding):
		writeError(w, http.StatusServiceUnavailable, "index_building", "The vector index is still being built")
	case errors.Is(err, vector.ErrEmbeddingTimeout):
		writeError(w, http.StatusServiceUnavailable, "embedding_timeout", "The embedding endpoint did not respond in time")
	default:
		s.logger.Error("semantic person search failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "semantic_search_unavailable",
			"Semantic person search could not resolve candidates")
	}
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

func (s *Server) handleListPeople(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, PeopleResponse{People: persons})
}

func (s *Server) handleDirectoryPeople(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	query, err := directoryPeopleQuery(r.URL.Query())
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	profiles, ok := s.personProfileStore(w)
	if !ok {
		return
	}
	page, err := profiles.DirectoryPeoplePageContext(r.Context(), query)
	if err != nil {
		s.writeDirectoryPeopleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, DirectoryPeopleResponse{People: page.People, NextCursor: page.NextCursor})
}

// directoryPeopleQuery reads each Directory query parameter once so the route
// has one canonical request representation for validation and pagination.
func directoryPeopleQuery(values url.Values) (store.DirectoryPeopleQuery, error) {
	query := store.DirectoryPeopleQuery{
		Query:          values.Get("q"),
		Cursor:         values.Get("cursor"),
		ContactState:   values.Get("contact_state"),
		Category:       values.Get("category"),
		Organization:   values.Get("organization"),
		PrimaryChannel: values.Get("primary_channel"),
		Sort:           values.Get("sort"),
	}
	for _, field := range []struct {
		name   string
		target **time.Time
	}{
		{name: "last_contact_after", target: &query.LastContactAfter},
		{name: "last_contact_before", target: &query.LastContactBefore},
	} {
		value := strings.TrimSpace(values.Get(field.name))
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return store.DirectoryPeopleQuery{}, fmt.Errorf("%w: %s must be RFC3339", store.ErrInvalidDirectoryQuery, field.name)
		}
		*field.target = &parsed
	}
	limit := strings.TrimSpace(values.Get("limit"))
	if limit == "" {
		return query, nil
	}
	parsed, err := strconv.Atoi(limit)
	if err != nil {
		return store.DirectoryPeopleQuery{}, newParamError("limit",
			fmt.Sprintf("query parameter %q must be an integer, got %q", "limit", limit))
	}
	query.Limit = parsed
	return query, nil
}

func (s *Server) writeDirectoryPeopleError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrInvalidDirectoryCursor):
		writeError(w, http.StatusBadRequest, "invalid_cursor", "Directory cursor is invalid")
	case errors.Is(err, store.ErrInvalidDirectoryQuery):
		writeError(w, http.StatusBadRequest, "invalid_query", "Directory query is invalid")
	case errors.Is(err, store.ErrDirectoryProjectionStale):
		writeError(w, http.StatusServiceUnavailable, "directory_projection_stale",
			"Directory data is refreshing; retry shortly")
	default:
		s.logger.Error("directory people query failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "directory_unavailable", "Directory is unavailable")
	}
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
	case errors.Is(err, store.ErrPersonCardDAVPublished):
		writeError(w, http.StatusConflict, "person_carddav_published",
			"Unpublish this person from CardDAV before deleting it")
	case errors.Is(err, store.ErrPersonMergeActive):
		writeError(w, http.StatusConflict, "person_merge_active",
			"Split the person's active merge lineage before deleting this profile")
	case errors.Is(err, store.ErrPersonEnrichmentDispatchInProgress):
		writeError(w, http.StatusConflict, "person_enrichment_dispatch_in_progress",
			"Person enrichment is being sent; retry the change after it finishes")
	case errors.Is(err, store.ErrParticipantNotFound), errors.Is(err, store.ErrInvalidParticipantID):
		writeError(w, http.StatusBadRequest, "invalid_participant_id", err.Error())
	default:
		s.logger.Error("person profile operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "person_profile_failed", "Person profile operation failed")
	}
}

func writePerson(w http.ResponseWriter, status int, person *store.Person) {
	w.Header().Set(etagHeaderName, personETag(*person))
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusCreated {
		w.Header().Set("Location", peoplePath+"/"+strconv.FormatInt(person.ID, 10))
	}
	writeJSON(w, status, person)
}

func addPersonIDParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{
		Name: "id", In: pathKey, Required: true, Description: "Durable person ID",
		Schema: &huma.Schema{Type: huma.TypeInteger, Format: formatInt64},
	})
}

func addDirectoryPeopleParameters(operation *huma.Operation) {
	lastContactAfter := queryStringParam("last_contact_after", "Return people contacted at or after this RFC3339 timestamp", false)
	lastContactAfter.Schema.Format = "date-time"
	lastContactBefore := queryStringParam("last_contact_before", "Return people contacted at or before this RFC3339 timestamp", false)
	lastContactBefore.Schema.Format = "date-time"
	sortParam := queryStringParam("sort", "Directory order: name, last_contact_desc, or last_contact_asc", false)
	sortParam.Schema.Enum = []any{
		store.DirectoryPeopleSortName,
		store.DirectoryPeopleSortLastContactDesc,
		store.DirectoryPeopleSortLastContactAsc,
	}
	operation.Parameters = append(operation.Parameters,
		queryStringParam("q", "Lexical query over person names, contact points, and organizations", false),
		queryStringParam("cursor", "Opaque cursor returned by the previous Directory page", false),
		queryIntegerParam("limit", "Maximum rows to return (default 50, max 100)"),
		queryStringParam("contact_state", "Current contact state: active or inactive", false),
		queryStringParam("category", "Current person category", false),
		queryStringParam("organization", "Current organization", false),
		queryStringParam("primary_channel", "Primary communication channel", false),
		lastContactAfter,
		lastContactBefore,
		sortParam,
	)
}

func addPersonIfMatchParameter(operation *huma.Operation) {
	operation.Parameters = append(operation.Parameters, &huma.Param{
		Name: ifMatchHeaderName, In: headerParamLocation, Required: true,
		Description: "Strong ETag returned by the latest person profile read. " +
			"Must be the exact single tag from that read; the RFC 7232 forms `*` " +
			"and comma-separated tag lists are not supported.",
		Schema: &huma.Schema{Type: huma.TypeString},
	})
}

func addPersonETagHeader(response *huma.Response) {
	response.Headers = map[string]*huma.Param{
		etagHeaderName: {
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
	return personRevisionETag(person.ID, person.Revision)
}

func personRevisionETag(personID, revision int64) string {
	return fmt.Sprintf(`"person-%d-r%d"`, personID, revision)
}

func personIfMatch(w http.ResponseWriter, r *http.Request, id int64) (int64, bool) {
	values := r.Header.Values(ifMatchHeaderName)
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
	return decodeEntityRequest(w, r, target, "person")
}

func decodePersonRequestFields(
	w http.ResponseWriter, r *http.Request, target any,
) (map[string]json.RawMessage, bool) {
	return decodeEntityRequestFields(w, r, target, "person")
}

// decodeEntityRequest decodes a strict single-object JSON body, labelling
// errors with the entity kind so an organization request never reports a
// person decoding failure.
func decodeEntityRequest(w http.ResponseWriter, r *http.Request, target any, entity string) bool {
	_, ok := decodeEntityRequestFields(w, r, target, entity)
	return ok
}

func decodeEntityRequestFields(
	w http.ResponseWriter, r *http.Request, target any, entity string,
) (map[string]json.RawMessage, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid "+entity+" request")
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid "+entity+" request: "+err.Error())
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request",
			capitalizeASCII(entity)+" request must contain one JSON object")
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid "+entity+" request")
		return nil, false
	}
	return fields, true
}

func capitalizeASCII(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}
