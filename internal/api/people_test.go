package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil"
)

type peopleAPIEngine struct {
	*querytest.MockEngine

	peopleRequest        query.PersonSearchRequest
	peopleResult         *query.PersonSearchResponse
	personSummaryRequest query.ExploreRequest
	personSummaryMembers []int64
	personSummaryResult  *query.PersonSearchResponse
	domainRequest        query.DomainSearchRequest
	domainResult         *query.DomainSearchResponse
	domainSummaryRequest query.ExploreRequest
	domainSummaryResult  *query.DomainSearchResponse
	person               *query.PersonSummary
	personClusterMembers []int64
	domain               *query.DomainSummary
	timeline             query.ExploreRequest
	timelineResult       *query.ExploreResponse
	peopleErr            error
}

func (e *peopleAPIEngine) SearchPeople(_ context.Context, request query.PersonSearchRequest) (*query.PersonSearchResponse, error) {
	e.peopleRequest = request
	return e.peopleResult, e.peopleErr
}

func (e *peopleAPIEngine) GetPerson(_ context.Context, _ int64, _ query.Context, clusterMemberIDs []int64) (*query.PersonSummary, error) {
	e.personClusterMembers = clusterMemberIDs
	return e.person, e.peopleErr
}

func (e *peopleAPIEngine) GetPersonSummary(_ context.Context, _ int64, request query.ExploreRequest, clusterMemberIDs []int64) (*query.PersonSearchResponse, error) {
	e.personSummaryRequest = request
	e.personSummaryMembers = clusterMemberIDs
	return e.personSummaryResult, e.peopleErr
}

func (e *peopleAPIEngine) SearchDomains(_ context.Context, request query.DomainSearchRequest) (*query.DomainSearchResponse, error) {
	e.domainRequest = request
	return e.domainResult, e.peopleErr
}

func (e *peopleAPIEngine) GetDomain(_ context.Context, _ string, _ query.Context) (*query.DomainSummary, error) {
	return e.domain, e.peopleErr
}

func (e *peopleAPIEngine) GetDomainSummary(_ context.Context, _ string, request query.ExploreRequest) (*query.DomainSearchResponse, error) {
	e.domainSummaryRequest = request
	return e.domainSummaryResult, e.peopleErr
}

func (e *peopleAPIEngine) Explore(_ context.Context, request query.ExploreRequest) (*query.ExploreResponse, error) {
	e.timeline = request
	return e.timelineResult, e.peopleErr
}

func (e *peopleAPIEngine) ExploreCoverage(context.Context, query.ExploreCoverageRequest, func([]int64) error) (*query.ExploreCoverageResult, error) {
	return &query.ExploreCoverageResult{}, e.peopleErr
}

func (e *peopleAPIEngine) ExploreGroups(context.Context, query.ExploreGroupRequest) (*query.ExploreGroupResponse, error) {
	return &query.ExploreGroupResponse{}, e.peopleErr
}

func (e *peopleAPIEngine) ExploreSelectionStats(context.Context, query.ExploreSelectionRequest) (*query.ExploreSelectionStats, error) {
	return &query.ExploreSelectionStats{}, e.peopleErr
}

func (e *peopleAPIEngine) ExploreFiles(context.Context, query.ExploreFilesRequest) (*query.ExploreFilesResponse, error) {
	return &query.ExploreFilesResponse{}, e.peopleErr
}

func (e *peopleAPIEngine) ExploreMatchCounts(context.Context, query.ExploreMatchCountsRequest) (*query.ExploreMatchCountsResponse, error) {
	return &query.ExploreMatchCountsResponse{}, e.peopleErr
}

func newPeopleAPIServer(engine *peopleAPIEngine) *Server {
	return newPeopleAPIServerWithStore(engine, &mockStore{})
}

func newPeopleAPIServerWithStore(engine *peopleAPIEngine, store MessageStore) *Server {
	return NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  store, Engine: engine, Logger: testLogger(),
	})
}

func TestPeopleSearchResolvesCanonicalFullTextCandidatesAndReturnsAuthority(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{}, peopleResult: &query.PersonSearchResponse{
		Rows: []query.PersonSummary{{ID: 12, DisplayLabel: "Shared Name"}}, TotalCount: 1,
		CacheRevision: "cache-people", SearchProvenance: query.SearchProvenance{LexicalIndexRevision: "resolved"},
	}}
	store := &mockStore{messages: []APIMessage{{ID: 42}}, total: 1}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/people/search", bytes.NewBufferString(`{
		"predicate":{"query":"needle","search_mode":"full_text"},
		"identity_query":"Shared Name","limit":25
	}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newPeopleAPIServerWithStore(engine, store).Router().ServeHTTP(response, request)

	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal(query.SearchFullText, engine.peopleRequest.Explore.Search.Mode)
	assertions.Equal([]int64{42}, engine.peopleRequest.Explore.Search.CandidateMessageIDs)
	assertions.NotEmpty(engine.peopleRequest.Explore.Search.LexicalIndexRevision)
	var body PersonSearchHTTPResponse
	requirements.NoError(json.NewDecoder(response.Body).Decode(&body))
	assertions.Equal(query.SearchProvenance{LexicalIndexRevision: "resolved"}, body.SearchProvenance)
}

func TestPeopleSearchNamesUnavailableSemanticAuthorityWithoutFallback(t *testing.T) {
	assertions := assert.New(t)
	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{}, peopleResult: &query.PersonSearchResponse{
		Rows: []query.PersonSummary{{ID: 11}}, TotalCount: 1, CacheRevision: "cache",
	}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/people/search", bytes.NewBufferString(`{
		"predicate":{"query":"needle","search_mode":"semantic"},"limit":25
	}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newPeopleAPIServer(engine).Router().ServeHTTP(response, request)

	assertions.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	assertions.Contains(response.Body.String(), "vector_not_enabled")
	assertions.Empty(engine.peopleRequest.Explore.Search.Mode, "unavailable semantic search must not call DuckDB with a broadened predicate")
}

func TestContextualSummaryPOSTsCarryCanonicalSearchAndReturnNamed404(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{},
		personSummaryResult: &query.PersonSearchResponse{Rows: []query.PersonSummary{{ID: 11, DisplayLabel: "Person", ActivityCount: 1}}, CacheRevision: "cache-person", SearchProvenance: query.SearchProvenance{LexicalIndexRevision: "person-rev"}},
		domainSummaryResult: &query.DomainSearchResponse{Rows: []query.DomainSummary{}, CacheRevision: "cache-domain"},
	}
	store := &mockStore{messages: []APIMessage{{ID: 42}}, total: 1}
	srv := newPeopleAPIServerWithStore(engine, store)
	body := `{"query":"needle","search_mode":"full_text","filters":[{"dimension":"source","values":["7"]}]}`

	person := httptest.NewRecorder()
	personRequest := httptest.NewRequest(http.MethodPost, "/api/v1/people/11/summary", bytes.NewBufferString(body))
	personRequest.Header.Set("Content-Type", "application/json")
	srv.Router().ServeHTTP(person, personRequest)
	requirements.Equal(http.StatusOK, person.Code, person.Body.String())
	assertions.Equal([]int64{7}, engine.personSummaryRequest.Context.SourceIDs)
	assertions.Equal([]int64{42}, engine.personSummaryRequest.Search.CandidateMessageIDs)
	var personBody PersonContextSummaryHTTPResponse
	requirements.NoError(json.NewDecoder(person.Body).Decode(&personBody))
	assertions.Equal(int64(1), personBody.Summary.ActivityCount)
	assertions.Equal(query.SearchProvenance{LexicalIndexRevision: "person-rev"}, personBody.SearchProvenance)

	domain := httptest.NewRecorder()
	domainRequest := httptest.NewRequest(http.MethodPost, "/api/v1/domains/example.com/summary", bytes.NewBufferString(body))
	domainRequest.Header.Set("Content-Type", "application/json")
	srv.Router().ServeHTTP(domain, domainRequest)
	assertions.Equal(http.StatusNotFound, domain.Code, domain.Body.String())
	assertions.Contains(domain.Body.String(), "domain_not_found")
}

func TestPeopleSearchForwardsCanonicalContextAndNeverAcceptsNameAsIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	when := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{}, peopleResult: &query.PersonSearchResponse{
		Rows: []query.PersonSummary{
			{ID: 11, DisplayLabel: "Shared Name", ActivityCount: 2, FirstAt: when, LastAt: when},
			{ID: 12, DisplayLabel: "Shared Name", ActivityCount: 1, FirstAt: when, LastAt: when},
		}, TotalCount: 2, CacheRevision: "cache-people",
	}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/people/search", bytes.NewBufferString(`{
		"predicate":{"filters":[{"dimension":"source","values":["7"]}]},
		"identity_query":" Shared Name ","sort":{"field":"display_label","direction":"asc"},"limit":25
	}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newPeopleAPIServer(engine).Router().ServeHTTP(response, request)

	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var result PersonSearchHTTPResponse
	requirements.NoError(json.NewDecoder(response.Body).Decode(&result))
	requirements.Len(result.Rows, 2)
	assertions.NotEqual(result.Rows[0].ID, result.Rows[1].ID)
	assertions.Equal([]int64{7}, engine.peopleRequest.Explore.Context.SourceIDs)
	assertions.Equal("Shared Name", engine.peopleRequest.Query)
	assertions.Equal(query.SortSpec{Field: "display_label", Direction: "asc"}, engine.peopleRequest.Sort)
}

func TestPersonDetailAndTimelineRequireDurablePositiveID(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	when := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{},
		person:         &query.PersonSummary{ID: 11, DisplayLabel: "Person", FirstAt: when, LastAt: when},
		timelineResult: &query.ExploreResponse{Rows: []query.EntryRow{{Key: "source:1:message:1", OccurredAt: when}}, TotalCount: 1, CacheRevision: "cache-timeline"},
	}
	srv := newPeopleAPIServerWithStore(engine, &mockStore{messages: []APIMessage{{ID: 42}}, total: 1})

	detail := httptest.NewRecorder()
	srv.Router().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/v1/people/11", nil))
	requirements.Equal(http.StatusOK, detail.Code, detail.Body.String())

	timeline := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/people/11/timeline", bytes.NewBufferString(`{"query":"needle","search_mode":"full_text","limit":25}`))
	request.Header.Set("Content-Type", "application/json")
	srv.Router().ServeHTTP(timeline, request)
	requirements.Equal(http.StatusOK, timeline.Code, timeline.Body.String())
	assertions.Equal([]int64{11}, engine.timeline.Context.ParticipantIDs)
	assertions.Equal([]int64{42}, engine.timeline.Search.CandidateMessageIDs)
	assertions.NotEmpty(engine.timeline.Search.LexicalIndexRevision)
	assertions.Equal(query.PageSpec{Limit: 25}, engine.timeline.Page)

	for _, path := range []string{"/api/v1/people/0", "/api/v1/people/name", "/api/v1/people/-4/timeline"} {
		method := http.MethodGet
		var body *bytes.Buffer
		if path[len(path)-9:] == "/timeline" {
			method, body = http.MethodPost, bytes.NewBufferString(`{}`)
		} else {
			body = bytes.NewBuffer(nil)
		}
		response := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("Content-Type", "application/json")
		srv.Router().ServeHTTP(response, req)
		assertions.Equal(http.StatusBadRequest, response.Code, path)
	}
}

func TestDomainEndpointsNormalizeExactDomainAndRejectAmbiguousOrSQLLikeValues(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	when := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{},
		domain:         &query.DomainSummary{Domain: "example.com", ActivityCount: 2, FirstAt: when, LastAt: when},
		domainResult:   &query.DomainSearchResponse{Rows: []query.DomainSummary{{Domain: "example.com", FirstAt: when, LastAt: when}}, TotalCount: 1, CacheRevision: "cache-domains"},
		timelineResult: &query.ExploreResponse{Rows: []query.EntryRow{}, CacheRevision: "cache-timeline"},
	}
	srv := newPeopleAPIServer(engine)

	detail := httptest.NewRecorder()
	srv.Router().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/v1/domains/EXAMPLE.COM", nil))
	requirements.Equal(http.StatusOK, detail.Code, detail.Body.String())

	timeline := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/domains/EXAMPLE.COM/timeline", bytes.NewBufferString(`{}`))
	request.Header.Set("Content-Type", "application/json")
	srv.Router().ServeHTTP(timeline, request)
	requirements.Equal(http.StatusOK, timeline.Code, timeline.Body.String())
	assertions.Equal([]string{"example.com"}, engine.timeline.Context.Domains)

	for _, path := range []string{"/api/v1/domains/%20", "/api/v1/domains/example.com%20OR%201=1"} {
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		assertions.Equal(http.StatusBadRequest, response.Code, path)
	}
}

// TestGetPersonComposesClusterBlockFromStoreForLinkedParticipant covers the
// handler-level composition documented in ClusterLookupStore and
// handleGetPerson: for a linked participant, the handler must resolve
// cluster membership from a real store (not a mock), forward every member
// ID to the query layer so identifiers span the whole cluster, and attach a
// PersonCluster with the canonical ID and every store-owned edge.
func TestGetPersonComposesClusterBlockFromStoreForLinkedParticipant(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	primary, err := st.EnsureParticipant("primary@example.com", "Primary", "example.com")
	requirements.NoError(err)
	secondary, err := st.EnsureParticipant("secondary@example.com", "Secondary", "example.com")
	requirements.NoError(err)
	_, err = st.LinkParticipants(primary, secondary)
	requirements.NoError(err)

	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{}, person: &query.PersonSummary{
		ID: primary, DisplayLabel: "Primary",
		Identifiers: []query.PersonIdentifier{
			{Type: "email", Value: "primary@example.com", IsPrimary: true, Provenance: "participant_identifiers", ParticipantID: primary},
			{Type: "email", Value: "secondary@example.com", IsPrimary: true, Provenance: "participant_identifiers", ParticipantID: secondary},
		},
	}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st, Engine: engine, Logger: testLogger(),
	})

	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/people/%d", primary), nil))
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.ElementsMatch([]int64{primary, secondary}, engine.personClusterMembers,
		"the handler must resolve cluster membership from the store and forward it to the query layer")

	var body query.PersonSummary
	requirements.NoError(json.NewDecoder(response.Body).Decode(&body))
	requirements.NotNil(body.Cluster, "a linked participant's detail must carry a cluster block")
	lo, hi := min(primary, secondary), max(primary, secondary)
	assertions.Equal(lo, body.Cluster.CanonicalID, "canonical ID is the cluster's smallest member")
	assertions.ElementsMatch([]int64{primary, secondary}, body.Cluster.MemberIDs)
	assertions.Equal([]query.PersonClusterEdge{{ParticipantA: lo, ParticipantB: hi}}, body.Cluster.Edges)
	requirements.Len(body.Identifiers, 2, "identifiers must span every cluster member")
	byParticipant := map[int64]string{}
	for _, identifier := range body.Identifiers {
		byParticipant[identifier.ParticipantID] = identifier.Value
	}
	assertions.Equal("primary@example.com", byParticipant[primary])
	assertions.Equal("secondary@example.com", byParticipant[secondary])
}

// TestGetPersonOmitsClusterBlockForUnlinkedParticipant is the negative case:
// an unlinked participant's detail must not carry a Cluster block, and the
// handler must not widen the query-layer call with a member list.
func TestGetPersonOmitsClusterBlockForUnlinkedParticipant(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	solo, err := st.EnsureParticipant("solo@example.com", "Solo", "example.com")
	requirements.NoError(err)

	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{}, person: &query.PersonSummary{ID: solo, DisplayLabel: "Solo"}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st, Engine: engine, Logger: testLogger(),
	})

	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/people/%d", solo), nil))
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Nil(engine.personClusterMembers)

	var body query.PersonSummary
	requirements.NoError(json.NewDecoder(response.Body).Decode(&body))
	assertions.Nil(body.Cluster)
}

// TestPersonTimelineWidensScopeToIdentityCluster covers identity consistency
// between the person timeline and the rest of the person surface: the
// timeline must scope to the same cluster the person summary and files
// search resolve, so activity owned only by a linked alias is forwarded to
// the engine as in-scope. An unlinked participant stays scoped to its own ID.
func TestPersonTimelineWidensScopeToIdentityCluster(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	primary, err := st.EnsureParticipant("primary@example.com", "Primary", "example.com")
	requirements.NoError(err)
	secondary, err := st.EnsureParticipant("secondary@example.com", "Secondary", "example.com")
	requirements.NoError(err)
	solo, err := st.EnsureParticipant("solo@example.com", "Solo", "example.com")
	requirements.NoError(err)
	_, err = st.LinkParticipants(primary, secondary)
	requirements.NoError(err)

	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{},
		timelineResult: &query.ExploreResponse{Rows: []query.EntryRow{}, CacheRevision: "cache-timeline"}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st, Engine: engine, Logger: testLogger(),
	})
	timeline := func(participantID int64) {
		request := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/api/v1/people/%d/timeline", participantID), bytes.NewBufferString(`{}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	}

	timeline(primary)
	assertions.ElementsMatch([]int64{primary, secondary}, engine.timeline.Context.ParticipantIDs,
		"a linked participant's timeline must scope to every cluster member so alias-owned activity is included")

	timeline(secondary)
	assertions.ElementsMatch([]int64{primary, secondary}, engine.timeline.Context.ParticipantIDs,
		"any cluster member resolves the same scope")

	timeline(solo)
	assertions.Equal([]int64{solo}, engine.timeline.Context.ParticipantIDs,
		"an unlinked participant stays scoped to its own ID")
}

// TestPersonContextSummaryWidensScopeToIdentityCluster covers identity
// consistency for the contextual summary: the handler must resolve cluster
// membership from the store and forward it to the query layer, so a predicate
// matching only alias-owned activity still yields the canonical identity's
// metrics — matching the person detail, timeline, and files search. An
// unlinked participant stays scoped to its own ID.
func TestPersonContextSummaryWidensScopeToIdentityCluster(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	primary, err := st.EnsureParticipant("primary@example.com", "Primary", "example.com")
	requirements.NoError(err)
	secondary, err := st.EnsureParticipant("secondary@example.com", "Secondary", "example.com")
	requirements.NoError(err)
	solo, err := st.EnsureParticipant("solo@example.com", "Solo", "example.com")
	requirements.NoError(err)
	_, err = st.LinkParticipants(primary, secondary)
	requirements.NoError(err)

	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{},
		personSummaryResult: &query.PersonSearchResponse{Rows: []query.PersonSummary{{ID: primary, DisplayLabel: "Primary", ActivityCount: 1}}, CacheRevision: "cache-person"}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st, Engine: engine, Logger: testLogger(),
	})
	summary := func(participantID int64) {
		request := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/api/v1/people/%d/summary", participantID), bytes.NewBufferString(`{}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	}

	summary(primary)
	assertions.ElementsMatch([]int64{primary, secondary}, engine.personSummaryMembers,
		"a linked participant's summary must scope to every cluster member so alias-owned activity counts")

	summary(secondary)
	assertions.ElementsMatch([]int64{primary, secondary}, engine.personSummaryMembers,
		"any cluster member resolves the same scope")

	summary(solo)
	assertions.Nil(engine.personSummaryMembers,
		"an unlinked participant stays scoped to its own ID")
}

func TestPeopleEndpointsNameUnavailableCacheInsteadOfReturningEmpty(t *testing.T) {
	assertions := assert.New(t)
	engine := &peopleAPIEngine{MockEngine: &querytest.MockEngine{}, peopleErr: &query.CacheUnavailableError{Readiness: query.CacheStaleSchema}}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/people/search", bytes.NewBufferString(`{}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	newPeopleAPIServer(engine).Router().ServeHTTP(response, request)
	assertions.Equal(http.StatusServiceUnavailable, response.Code)
	assertions.Contains(response.Body.String(), "stale_schema")
}
