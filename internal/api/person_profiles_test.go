package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/personsearch"
)

type fakePersonSearchEngine struct {
	results []personsearch.Result
	err     error
	queries []string
	limits  []int
}

func (e *fakePersonSearchEngine) Search(
	_ context.Context, query string, limit int,
) ([]personsearch.Result, error) {
	e.queries = append(e.queries, query)
	e.limits = append(e.limits, limit)
	return e.results, e.err
}

func TestPersonProfileHTTPPromoteListGetUpdateAndConflictingLink(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	alice := st.mustParticipant(t, "alice@example.com", "alice", "example.com")
	bob := st.mustParticipant(t, "bob@example.com", "bob", "example.com")

	createdResponse := personRequest(t, srv, http.MethodPost, peoplePath,
		fmt.Appendf(nil, `{"participant_id":%d}`, alice), "")
	require.Equal(http.StatusCreated, createdResponse.Code)
	var created store.Person
	require.NoError(json.Unmarshal(createdResponse.Body.Bytes(), &created))
	assert.Equal([]int64{alice}, created.ParticipantIDs)
	assert.NotEmpty(created.VCardUID)
	etag := createdResponse.Header().Get("ETag")
	assert.NotEmpty(etag)

	repromotedResponse := personRequest(t, srv, http.MethodPost, peoplePath,
		fmt.Appendf(nil, `{"participant_id":%d}`, alice), "")
	require.Equal(http.StatusOK, repromotedResponse.Code)
	var repromoted store.Person
	require.NoError(json.Unmarshal(repromotedResponse.Body.Bytes(), &repromoted))
	assert.Equal(created.ID, repromoted.ID)

	listResponse := personRequest(t, srv, http.MethodGet, peoplePath, nil, "")
	require.Equal(http.StatusOK, listResponse.Code)
	var listed PeopleResponse
	require.NoError(json.Unmarshal(listResponse.Body.Bytes(), &listed))
	require.Len(listed.People, 1)
	assert.Equal(created.ID, listed.People[0].ID)

	getResponse := personRequest(t, srv, http.MethodGet,
		fmt.Sprintf("%s/%d", peoplePath, created.ID), nil, "")
	require.Equal(http.StatusOK, getResponse.Code)
	assert.Equal(etag, getResponse.Header().Get("ETag"))

	updatedResponse := personRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", peoplePath, created.ID),
		[]byte(`{"display_name":"alice"}`), etag)
	require.Equal(http.StatusOK, updatedResponse.Code)
	var updated store.Person
	require.NoError(json.Unmarshal(updatedResponse.Body.Bytes(), &updated))
	require.NotNil(updated.DisplayName)
	assert.Equal("alice", *updated.DisplayName)

	clearedResponse := personRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", peoplePath, created.ID),
		[]byte(`{"display_name":null}`), updatedResponse.Header().Get("ETag"))
	require.Equal(http.StatusOK, clearedResponse.Code)
	var cleared store.Person
	require.NoError(json.Unmarshal(clearedResponse.Body.Bytes(), &cleared))
	assert.Nil(cleared.DisplayName)

	staleResponse := personRequest(t, srv, http.MethodPatch,
		fmt.Sprintf("%s/%d", peoplePath, created.ID),
		[]byte(`{"display_name":"alice stale"}`), etag)
	assert.Equal(http.StatusConflict, staleResponse.Code)

	_, _, err := st.CreatePersonFromParticipant(bob)
	require.NoError(err)
	linkResponse := postIdentityLink(t, srv, "/api/v1/identity/links",
		IdentityLinkRequest{ParticipantA: alice, ParticipantB: bob})
	assert.Equal(http.StatusConflict, linkResponse.Code)
}

func TestDirectoryPeopleHTTPReturnsNoStorePage(t *testing.T) {
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	participant := st.mustParticipant(t, "alice@example.test", "Alice Example", "example.test")
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(err)
	name := "Alice Example"
	person, err = st.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, &name)
	require.NoError(err)
	_, err = st.AddPersonCategoryContext(t.Context(), person.ID, store.PersonCategoryInput{
		OriginalValue: "friend", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	response := personRequest(t, srv, http.MethodGet,
		peoplePath+"/directory?q=alcie&category=friend&limit=1", nil, "")
	require.Equal(http.StatusOK, response.Code)
	assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
}

func TestDirectoryPeopleHTTPMapsEveryQueryParameter(t *testing.T) {
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	alice := createDirectoryHTTPPerson(t, st, "Alice Example", "alice@example.test", "friend", "Acme", true)
	bob := createDirectoryHTTPPerson(t, st, "Bob Example", "bob@example.test", "colleague", "Other", false)

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "query", path: peoplePath + "/directory?q=alcie"},
		{name: "contact state", path: peoplePath + "/directory?contact_state=active"},
		{name: "category", path: peoplePath + "/directory?category=friend"},
		{name: "organization", path: peoplePath + "/directory?organization=Acme"},
		{name: "primary channel", path: peoplePath + "/directory?primary_channel=email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := personRequest(t, srv, http.MethodGet, tc.path, nil, "")
			require.Equal(http.StatusOK, response.Code)
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			assertDirectoryPeopleResponseIDs(t, response, alice.ID)
		})
	}

	first := personRequest(t, srv, http.MethodGet, peoplePath+"/directory?limit=1", nil, "")
	require.Equal(http.StatusOK, first.Code)
	var page DirectoryPeopleResponse
	require.NoError(json.Unmarshal(first.Body.Bytes(), &page))
	require.NotEmpty(page.NextCursor)
	second := personRequest(t, srv, http.MethodGet,
		peoplePath+"/directory?limit=1&cursor="+page.NextCursor, nil, "")
	require.Equal(http.StatusOK, second.Code)
	assertDirectoryPeopleResponseIDs(t, second, bob.ID)
}

func TestDirectoryPeopleHTTPFiltersSortsAndReturnsLastContact(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newIdentityLinkTestServer(t)
	recent := createDirectoryHTTPPerson(t, st, "Alpha Recent", "recent@example.test", "friend", "Acme", true)
	older := createDirectoryHTTPPerson(t, st, "Zulu Older", "older@example.test", "friend", "Acme", true)
	never := createDirectoryHTTPPerson(t, st, "Bravo Never", "never@example.test", "friend", "Acme", false)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_contact_state SET last_contact_at = ? WHERE person_id = ?`), "2026-08-20T10:00:00Z", recent.ID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_contact_state SET last_contact_at = ? WHERE person_id = ?`), "2026-01-10T09:00:00Z", older.ID)
	require.NoError(err)

	after := url.QueryEscape("2026-06-01T00:00:00Z")
	filtered := personRequest(t, srv, http.MethodGet,
		peoplePath+"/directory?sort=last_contact_desc&last_contact_after="+after, nil, "")
	require.Equal(http.StatusOK, filtered.Code, filtered.Body.String())
	var filteredPage struct {
		People []struct {
			ID            int64   `json:"id"`
			LastContactAt *string `json:"last_contact_at"`
		} `json:"people"`
	}
	require.NoError(json.Unmarshal(filtered.Body.Bytes(), &filteredPage))
	require.Len(filteredPage.People, 1)
	assert.Equal(recent.ID, filteredPage.People[0].ID)
	require.NotNil(filteredPage.People[0].LastContactAt)
	assert.Equal("2026-08-20T10:00:00Z", *filteredPage.People[0].LastContactAt)

	first := personRequest(t, srv, http.MethodGet,
		peoplePath+"/directory?contact_state=active&sort=last_contact_asc&limit=1", nil, "")
	require.Equal(http.StatusOK, first.Code, first.Body.String())
	var firstPage DirectoryPeopleResponse
	require.NoError(json.Unmarshal(first.Body.Bytes(), &firstPage))
	assert.Equal([]int64{older.ID}, directoryHTTPPersonIDs(firstPage.People))
	require.NotEmpty(firstPage.NextCursor)

	second := personRequest(t, srv, http.MethodGet,
		peoplePath+"/directory?contact_state=active&sort=last_contact_asc&limit=1&cursor="+url.QueryEscape(firstPage.NextCursor), nil, "")
	require.Equal(http.StatusOK, second.Code, second.Body.String())
	var secondPage DirectoryPeopleResponse
	require.NoError(json.Unmarshal(second.Body.Bytes(), &secondPage))
	assert.Equal([]int64{recent.ID}, directoryHTTPPersonIDs(secondPage.People))
	assert.Empty(secondPage.NextCursor)
	assert.NotEqual(never.ID, secondPage.People[0].ID)
}

func TestDirectoryPeopleHTTPRejectsInvalidLastContactQuery(t *testing.T) {
	srv, _ := newIdentityLinkTestServer(t)
	for _, test := range []struct {
		path string
		code string
	}{
		{path: peoplePath + "/directory?sort=oldestish", code: "invalid_query"},
		{path: peoplePath + "/directory?last_contact_after=yesterday", code: "invalid_parameter"},
		{path: peoplePath + "/directory?last_contact_before=tomorrow", code: "invalid_parameter"},
	} {
		response := personRequest(t, srv, http.MethodGet, test.path, nil, "")
		assert.Equal(t, http.StatusBadRequest, response.Code, test.path)
		assertDirectoryPeopleError(t, response, test.code)
	}
}

func TestDirectoryPeopleHTTPRejectsInvalidParametersAndStaleProjection(t *testing.T) {
	srv, _ := newIdentityLinkTestServer(t)
	for _, tc := range []struct {
		name string
		path string
		code string
	}{
		{name: "cursor", path: peoplePath + "/directory?cursor=not-a-cursor", code: "invalid_cursor"},
		{name: "query", path: peoplePath + "/directory?contact_state=unknown", code: "invalid_query"},
		{name: "limit", path: peoplePath + "/directory?limit=many", code: "invalid_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := personRequest(t, srv, http.MethodGet, tc.path, nil, "")
			require.Equal(t, http.StatusBadRequest, response.Code)
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			assertDirectoryPeopleError(t, response, tc.code)
		})
	}

	staleServer := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}},
		&staleDirectoryPeopleStore{stubIdentityCacheStore: newDirectoryTestStore(t)}, nil, testLogger())
	stale := personRequest(t, staleServer, http.MethodGet, peoplePath+"/directory", nil, "")
	require.Equal(t, http.StatusServiceUnavailable, stale.Code)
	assert.Equal(t, "no-store", stale.Header().Get("Cache-Control"))
	assertDirectoryPeopleError(t, stale, "directory_projection_stale")
}

func TestDirectoryPeopleHTTPAcceptsEveryPublishedContactState(t *testing.T) {
	srv, _ := newIdentityLinkTestServer(t)
	for _, state := range []string{"", "active", "inactive"} {
		path := peoplePath + "/directory"
		if state != "" {
			path += "?contact_state=" + state
		}
		response := personRequest(t, srv, http.MethodGet, path, nil, "")
		require.Equal(t, http.StatusOK, response.Code, state)
	}
}

type staleDirectoryPeopleStore struct {
	*stubIdentityCacheStore
}

func (s *staleDirectoryPeopleStore) DirectoryPeoplePageContext(
	context.Context, store.DirectoryPeopleQuery,
) (*store.DirectoryPeoplePage, error) {
	return nil, store.ErrDirectoryProjectionStale
}

func newDirectoryTestStore(t *testing.T) *stubIdentityCacheStore {
	t.Helper()
	return &stubIdentityCacheStore{Store: testutil.NewTestStore(t)}
}

func createDirectoryHTTPPerson(
	t *testing.T,
	st *stubIdentityCacheStore,
	displayName, email, category, organizationName string,
	active bool,
) *store.Person {
	t.Helper()
	participantID, err := st.EnsureParticipantByIdentifier("email", email, displayName)
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(t, err)
	person, err = st.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, &displayName)
	require.NoError(t, err)
	_, err = st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: email,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	_, err = st.AddPersonCategoryContext(t.Context(), person.ID, store.PersonCategoryInput{
		OriginalValue: category, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	organization, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{
		Name: organizationName, Kind: store.OrganizationKindCompany,
	})
	require.NoError(t, err)
	_, err = st.AddEmploymentContext(t.Context(), store.EmploymentInput{
		PersonID: person.ID, OrganizationID: organization.ID, Source: store.ProvenanceUser,
	})
	require.NoError(t, err)
	if active {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_contact_state (
			person_id, last_contact_channel, last_contact_at, interaction_count
		) VALUES (?, 'email', CURRENT_TIMESTAMP, 1)`), person.ID)
		require.NoError(t, err)
	}
	return person
}

func assertDirectoryPeopleResponseIDs(t *testing.T, response *httptest.ResponseRecorder, want ...int64) {
	t.Helper()
	var page DirectoryPeopleResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	got := make([]int64, len(page.People))
	for i, person := range page.People {
		got[i] = person.ID
	}
	assert.Equal(t, want, got)
}

func directoryHTTPPersonIDs(people []store.DirectoryPersonSummary) []int64 {
	ids := make([]int64, 0, len(people))
	for _, person := range people {
		ids = append(ids, person.ID)
	}
	return ids
}

func TestDirectoryPeopleHTTPAlwaysEmitsArrays(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, st := newIdentityLinkTestServer(t)
	participantID, err := st.EnsureParticipantByIdentifier("email", "arrays@example.test", "Array Person")
	require.NoError(err)
	_, _, err = st.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(err)
	response := personRequest(t, srv, http.MethodGet, peoplePath+"/directory", nil, "")
	require.Equal(http.StatusOK, response.Code)
	var body struct {
		People []struct {
			Categories    []string `json:"categories"`
			Organizations []string `json:"organizations"`
		} `json:"people"`
	}
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(body.People, 1)
	assert.NotNil(body.People)
	assert.NotNil(body.People[0].Categories)
	assert.NotNil(body.People[0].Organizations)
	assert.Empty(body.People[0].Categories)
	assert.Empty(body.People[0].Organizations)
}

func assertDirectoryPeopleError(t *testing.T, response *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body ErrorResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, want, body.Error)
}

func TestPersonProfileHTTPDelete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newIdentityLinkTestServer(t)
	alice := st.mustParticipant(t, "alice@example.com", "alice", "example.com")
	person, _, err := st.CreatePersonFromParticipant(alice)
	require.NoError(err)
	path := fmt.Sprintf("%s/%d", peoplePath, person.ID)
	etag := fmt.Sprintf(`"person-%d-r%d"`, person.ID, person.Revision)

	missingIfMatch := personRequest(t, srv, http.MethodDelete, path, nil, "")
	assert.Equal(http.StatusPreconditionRequired, missingIfMatch.Code)

	stale := personRequest(t, srv, http.MethodDelete, path, nil,
		fmt.Sprintf(`"person-%d-r%d"`, person.ID, person.Revision+7))
	assert.Equal(http.StatusConflict, stale.Code)

	deleted := personRequest(t, srv, http.MethodDelete, path, nil, etag)
	require.Equal(http.StatusNoContent, deleted.Code)
	assert.Empty(deleted.Body.Bytes())

	gone := personRequest(t, srv, http.MethodGet, path, nil, "")
	assert.Equal(http.StatusNotFound, gone.Code)
	deletedAgain := personRequest(t, srv, http.MethodDelete, path, nil, etag)
	assert.Equal(http.StatusNotFound, deletedAgain.Code)
}

func TestPersonProfileHTTPDeleteReportsConflicts(t *testing.T) {
	srv, _ := newIdentityLinkTestServer(t)
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{name: "CardDAV publication", err: store.ErrPersonCardDAVPublished, code: "person_carddav_published"},
		{name: "enrichment dispatch", err: store.ErrPersonEnrichmentDispatchInProgress, code: "person_enrichment_dispatch_in_progress"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			srv.writePersonError(response, test.err)
			assert.Equal(t, http.StatusConflict, response.Code)
			assert.Contains(t, response.Body.String(), `"`+test.code+`"`)
		})
	}
}

func TestPersonPatchSchemaRequiresNullableDisplayName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, document := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		schema := document.Components.Schemas.Map()["PatchPersonRequest"]
		require.NotNil(schema)
		assert.Contains(schema.Required, "display_name")
		require.Contains(schema.Properties, "display_name")
		assert.True(schema.Properties["display_name"].Nullable)
	}
}

func TestSemanticPersonSearchReturnsRankedDurableRootsWithScores(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	firstName := "Synthetic Architect"
	secondName := "Test Researcher"
	engine := &fakePersonSearchEngine{results: []personsearch.Result{
		{Person: store.Person{
			ID: 22, VCardUID: "00000000-0000-4000-8000-000000000022",
			DisplayName: &firstName, Revision: 4, ParticipantIDs: []int64{220},
		}, Score: 0.91},
		{Person: store.Person{
			ID: 11, VCardUID: "00000000-0000-4000-8000-000000000011",
			DisplayName: &secondName, Revision: 7, ParticipantIDs: []int64{110},
		}, Score: 0.82},
	}}
	srv := newSemanticPersonSearchServer(t, engine, VectorStatusReady, "")

	response := semanticPersonSearchRequest(t, srv,
		[]byte(`{"query":"  synthetic product architect  "}`), "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Equal("no-store", response.Header().Get("Cache-Control"))
	assert.Equal([]string{"synthetic product architect"}, engine.queries)
	assert.Equal([]int{defaultPersonSearchLimit}, engine.limits)
	var body PersonSearchResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(body.Results, 2)
	assert.Equal(int64(22), body.Results[0].Person.ID)
	assert.InDelta(0.91, body.Results[0].Score, 0.00001)
	assert.Equal(int64(11), body.Results[1].Person.ID)
	assert.InDelta(0.82, body.Results[1].Score, 0.00001)
	assert.NotContains(response.Body.String(), "renderer_policy")
	assert.NotContains(response.Body.String(), `"text"`)
}

func TestSemanticPersonSearchReturnsEmptyArrayAndPassesExplicitLimit(t *testing.T) {
	engine := &fakePersonSearchEngine{results: []personsearch.Result{}}
	srv := newSemanticPersonSearchServer(t, engine, VectorStatusReady, "")

	response := semanticPersonSearchRequest(t, srv,
		[]byte(`{"query":"no matching person","limit":3}`), "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, []int{3}, engine.limits)
	assert.JSONEq(t, `{"results":[]}`, response.Body.String())
}

func TestSemanticPersonSearchValidatesQueryAndLimit(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "blank query", body: `{"query":"  "}`},
		{name: "negative limit", body: `{"query":"synthetic","limit":-1}`},
		{name: "limit above bound", body: `{"query":"synthetic","limit":101}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := &fakePersonSearchEngine{}
			srv := newSemanticPersonSearchServer(t, engine, VectorStatusReady, "")
			response := semanticPersonSearchRequest(t, srv, []byte(test.body), "")
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			assert.Empty(t, engine.queries)
		})
	}
}

func TestSemanticPersonSearchRequiresAuthenticationAndNeverCaches(t *testing.T) {
	assert := assert.New(t)
	const apiKey = "person-search-test-key"
	engine := &fakePersonSearchEngine{results: []personsearch.Result{}}
	srv := newSemanticPersonSearchServer(t, engine, VectorStatusReady, apiKey)

	unauthorized := semanticPersonSearchRequest(t, srv,
		[]byte(`{"query":"synthetic"}`), "")
	assert.Equal(http.StatusUnauthorized, unauthorized.Code, unauthorized.Body.String())
	assert.Equal("no-store", unauthorized.Header().Get("Cache-Control"))
	assert.Empty(engine.queries)

	authorized := semanticPersonSearchRequest(t, srv,
		[]byte(`{"query":"synthetic"}`), apiKey)
	assert.Equal(http.StatusOK, authorized.Code, authorized.Body.String())
	assert.Equal("no-store", authorized.Header().Get("Cache-Control"))
}

func TestSemanticPersonSearchReportsEveryVectorStatusState(t *testing.T) {
	tests := []struct {
		name      string
		status    VectorStatus
		statusErr string
		wantCode  string
	}{
		{name: "disabled", status: VectorStatusDisabled, wantCode: "vector_not_enabled"},
		{name: "initializing", status: VectorStatusInitializing, wantCode: "vector_initializing"},
		{name: "failed", status: VectorStatusError, statusErr: "synthetic migration failure", wantCode: "vector_init_failed"},
		{name: "stale", status: VectorStatusStale, statusErr: "synthetic stale index", wantCode: "index_stale"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			engine := &fakePersonSearchEngine{}
			srv := newSemanticPersonSearchServer(t, engine, test.status, "")
			if test.status == VectorStatusError {
				srv.SetVectorInitError(errors.New(test.statusErr))
			}
			if test.status == VectorStatusStale {
				srv.SetVectorStale(test.statusErr)
			}
			response := semanticPersonSearchRequest(t, srv,
				[]byte(`{"query":"synthetic"}`), "")
			require.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
			var body ErrorResponse
			require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(test.wantCode, body.Error)
			assert.Empty(engine.queries)
		})
	}
}

func TestSemanticPersonSearchClearsCachedStaleStatusBeforeServing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := vector.Config{}
	backend := &resolvingVectorBackend{fingerprint: cfg.GenerationFingerprint()}
	engine := &fakePersonSearchEngine{results: []personsearch.Result{}}
	srv := NewServerWithOptions(ServerOptions{
		Config:             &config.Config{},
		Logger:             testLogger(),
		Backend:            backend,
		VectorCfg:          cfg,
		VectorStatus:       VectorStatusStale,
		PersonSearchEngine: engine,
	})
	srv.SetVectorStale("synthetic cached stale status")
	status, _ := srv.VectorStatus()
	require.Equal(VectorStatusStale, status, "precondition: stale is cached")

	response := semanticPersonSearchRequest(t, srv,
		[]byte(`{"query":"synthetic"}`), "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Equal([]string{"synthetic"}, engine.queries)
	status, detail := srv.VectorStatus()
	assert.Equal(VectorStatusReady, status)
	assert.Empty(detail)
}

func TestSemanticPersonSearchMapsGenerationAndEmbeddingErrors(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantCode    string
		wantMessage string
		wantDetail  string
	}{
		{name: "not enabled", err: vector.ErrNotEnabled, wantCode: "vector_not_enabled"},
		{
			name: "semantic people disabled", err: vector.ErrSemanticPersonEmbeddingsDisabled,
			wantCode: "person_embeddings_disabled", wantMessage: "[vector.people] enabled = true",
		},
		{
			name: "semantic people unconsented", err: vector.ErrSemanticPersonEmbeddingConsentRequired,
			wantCode: "person_embedding_consent_required", wantMessage: "person provider consent --semantic-embeddings",
		},
		{
			name: "semantic person policy unavailable",
			err: fmt.Errorf("%w: read current policy: synthetic config source unavailable",
				vector.ErrSemanticPersonEmbeddingPolicyUnavailable),
			wantCode:    "person_embedding_policy_unavailable",
			wantMessage: "cannot verify the current semantic person embedding policy",
		},
		{name: "stale", err: vector.ErrIndexStale, wantCode: "index_stale"},
		{name: "building", err: vector.ErrIndexBuilding, wantCode: "index_building"},
		{
			name: "person coverage incomplete", err: personsearch.ErrPersonCoverageIncomplete,
			wantCode: "index_building", wantMessage: "msgvault embeddings resume --backstop",
		},
		{
			name: "person coverage terminal rejection",
			err: &personsearch.CoverageIncompleteError{
				Generation: 7, Rejected: 2,
			},
			wantCode:    "index_building",
			wantMessage: "run `msgvault embeddings resume --backstop` first",
			wantDetail:  "if terminal rejections remain",
		},
		{name: "embedding timeout", err: vector.ErrEmbeddingTimeout, wantCode: "embedding_timeout"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			engine := &fakePersonSearchEngine{err: test.err}
			srv := newSemanticPersonSearchServer(t, engine, VectorStatusReady, "")
			response := semanticPersonSearchRequest(t, srv,
				[]byte(`{"query":"synthetic"}`), "")
			requirements.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
			var body ErrorResponse
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assertions.Equal(test.wantCode, body.Error)
			if test.wantMessage != "" {
				assertions.Contains(body.Message, test.wantMessage)
			}
			if test.wantDetail != "" {
				assertions.Contains(body.Message, test.wantDetail)
			}
		})
	}
}

func newSemanticPersonSearchServer(
	t *testing.T, engine PersonSearchEngine, status VectorStatus, apiKey string,
) *Server {
	t.Helper()
	return NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: apiKey}},
		Logger: testLogger(), VectorStatus: status, PersonSearchEngine: engine,
	})
}

func semanticPersonSearchRequest(
	t *testing.T, srv *Server, body []byte, apiKey string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, peoplePath+"/search", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		request.Header.Set("X-Api-Key", apiKey)
	}
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	return response
}

func personRequest(
	t *testing.T, srv *Server, method, path string, body []byte, ifMatch string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, req)
	return response
}
