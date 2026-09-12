package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type meetingRouteStore struct {
	*mockStore

	contextResult *meetingcontent.PacketResult
	contextErr    error
	actionsResult *meetingcontent.ActionsPage
	actionsErr    error
	metricsResult *meetingcontent.Metrics
	metricsErr    error

	contextCalls   int
	contextIDs     []int64
	contextOptions meetingcontent.PacketOptions
	actionsCalls   int
	actionsQuery   store.MeetingActionsQuery
	metricsCalls   int
	metricsScope   store.MeetingQueryScope
}

func (s *meetingRouteStore) GetMeetingContextContext(
	_ context.Context, ids []int64, options meetingcontent.PacketOptions,
) (*meetingcontent.PacketResult, error) {
	s.contextCalls++
	s.contextIDs = append([]int64(nil), ids...)
	s.contextOptions = options
	return s.contextResult, s.contextErr
}

func (s *meetingRouteStore) ListMeetingActionsContext(
	_ context.Context, request store.MeetingActionsQuery,
) (*meetingcontent.ActionsPage, error) {
	s.actionsCalls++
	s.actionsQuery = request
	return s.actionsResult, s.actionsErr
}

func (s *meetingRouteStore) GetMeetingMetricsContext(
	_ context.Context, scope store.MeetingQueryScope,
) (*meetingcontent.Metrics, error) {
	s.metricsCalls++
	s.metricsScope = scope
	return s.metricsResult, s.metricsErr
}

type meetingRouteEngine struct {
	*querytest.MockEngine

	result  *query.ExploreMeetingSelection
	err     error
	calls   int
	request query.ExploreSelectionRequest
	limit   int
}

func (e *meetingRouteEngine) ResolveExploreMeetings(
	_ context.Context, request query.ExploreSelectionRequest, limit int,
) (*query.ExploreMeetingSelection, error) {
	e.calls++
	e.request = request
	e.limit = limit
	return e.result, e.err
}

func newMeetingRouteTestServer(t *testing.T, st *meetingRouteStore, engine query.Engine) *Server {
	t.Helper()
	return NewServerWithOptions(ServerOptions{
		Config: &config.Config{}, Store: st, Engine: engine, Logger: testLogger(),
	})
}

func meetingRouteRequest(path, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", applicationJSONMediaType)
	return request
}

type meetingHTTPClient struct {
	client *http.Client
}

func (c meetingHTTPClient) Do(ctx context.Context, request *http.Request) (*http.Response, error) {
	return c.client.Do(request.WithContext(ctx))
}

func TestMeetingDirectRoutesPreserveScopeAndDefaults(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := &meetingRouteStore{
		mockStore:     &mockStore{},
		contextResult: &meetingcontent.PacketResult{SchemaVersion: 1, Format: meetingcontent.FormatJSON, Content: "{}", ContentBytes: 2, OmittedMessageIDs: []int64{}},
		actionsResult: &meetingcontent.ActionsPage{SchemaVersion: 1, Rows: []meetingcontent.ActionRow{}, Scope: meetingcontent.ScopeProvenance{Kind: "direct"}},
		metricsResult: &meetingcontent.Metrics{SchemaVersion: 1, Scope: meetingcontent.ScopeProvenance{Kind: "direct"}, DurationByBasis: []meetingcontent.BasisTotals{}, Months: []meetingcontent.MonthTotals{}},
	}
	srv := newMeetingRouteTestServer(t, st, &querytest.MockEngine{})

	contextResponse := httptest.NewRecorder()
	srv.Router().ServeHTTP(contextResponse, meetingRouteRequest(
		"/api/v1/meetings/context", `{"message_ids":[2,1,2]}`,
	))
	requirements.Equal(http.StatusOK, contextResponse.Code, contextResponse.Body.String())
	assertions.Equal([]int64{1, 2}, st.contextIDs)
	assertions.Equal(meetingcontent.PacketOptions{
		Format: meetingcontent.FormatJSON, IncludeTranscript: false, MaxBytes: 131072,
	}, st.contextOptions)

	actionsResponse := httptest.NewRecorder()
	srv.Router().ServeHTTP(actionsResponse, meetingRouteRequest(
		"/api/v1/meetings/actions", `{"scope":{"message_ids":[]},"limit":2}`,
	))
	requirements.Equal(http.StatusOK, actionsResponse.Code, actionsResponse.Body.String())
	requirements.NotNil(st.actionsQuery.Scope.MessageIDs)
	assertions.Empty(*st.actionsQuery.Scope.MessageIDs)
	assertions.Equal(2, st.actionsQuery.Limit)

	metricsResponse := httptest.NewRecorder()
	srv.Router().ServeHTTP(metricsResponse, meetingRouteRequest("/api/v1/meetings/metrics", `{}`))
	requirements.Equal(http.StatusOK, metricsResponse.Code, metricsResponse.Body.String())
	assertions.Nil(st.metricsScope.MessageIDs)
}

func TestGeneratedMeetingClientSendsExplicitEmptyMessageIDs(t *testing.T) {
	requirements := require.New(t)
	st := &meetingRouteStore{
		mockStore: &mockStore{},
		metricsResult: &meetingcontent.Metrics{
			DurationByBasis: []meetingcontent.BasisTotals{}, Months: []meetingcontent.MonthTotals{},
		},
	}
	server := httptest.NewServer(newMeetingRouteTestServer(t, st, &querytest.MockEngine{}).Router())
	t.Cleanup(server.Close)
	client, err := generated.NewDefaultClient(server.URL,
		runtime.WithHTTPClient(meetingHTTPClient{client: server.Client()}))
	requirements.NoError(err)

	emptyIDs := []int64{}
	scope := &generated.MeetingScopeRequest{MessageIds: &emptyIDs}
	response, err := client.GetMeetingMetricsWithResponse(t.Context(), &generated.GetMeetingMetricsRequestOptions{
		Body: &generated.MeetingMetricsRequest{Scope: scope},
	})
	requirements.NoError(err)
	requirements.Equal(http.StatusOK, response.StatusCode, string(response.Body))
	requirements.NotNil(st.metricsScope.MessageIDs)
	assert.Empty(t, *st.metricsScope.MessageIDs)
}

func TestMeetingRoutesRejectNoncanonicalJSONFieldsBeforeResolution(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "top level", body: `{"Explore":{"predicate":{},"cache_revision":"r","search_provenance":{}}}`},
		{name: "nested scalar", body: `{"scope":{"PERSON_ID":7}}`},
		{name: "nested array", body: `{"scope":{"MESSAGE_IDS":[0]}}`},
		{name: "conflicting variants", body: `{"scope":{"person_id":7},"Scope":{"person_id":8}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			personCalls := 0
			st := &meetingRouteStore{
				mockStore: &mockStore{personContextFunc: func(_ context.Context, id int64) (*store.Person, error) {
					personCalls++
					return &store.Person{ID: id, ParticipantIDs: []int64{id}}, nil
				}},
				metricsResult: &meetingcontent.Metrics{
					DurationByBasis: []meetingcontent.BasisTotals{}, Months: []meetingcontent.MonthTotals{},
				},
			}
			engine := &meetingRouteEngine{MockEngine: &querytest.MockEngine{}, result: &query.ExploreMeetingSelection{
				CacheRevision: "r", MessageIDs: []int64{},
			}}
			response := httptest.NewRecorder()
			newMeetingRouteTestServer(t, st, engine).Router().ServeHTTP(
				response, meetingRouteRequest("/api/v1/meetings/metrics", test.body),
			)
			assertions.Equal(http.StatusBadRequest, response.Code, response.Body.String())
			assertions.Zero(personCalls)
			assertions.Zero(engine.calls)
			assertions.Zero(st.metricsCalls)
		})
	}
}

func TestMeetingActionsValidateOptionsBeforeResolvingScope(t *testing.T) {
	t.Run("direct person", func(t *testing.T) {
		personCalls := 0
		st := &meetingRouteStore{mockStore: &mockStore{personContextFunc: func(_ context.Context, id int64) (*store.Person, error) {
			personCalls++
			return &store.Person{ID: id, ParticipantIDs: []int64{id}}, nil
		}}}
		response := httptest.NewRecorder()
		newMeetingRouteTestServer(t, st, &querytest.MockEngine{}).Router().ServeHTTP(
			response,
			meetingRouteRequest("/api/v1/meetings/actions", `{"scope":{"person_id":7},"limit":0}`),
		)

		assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		assert.Zero(t, personCalls)
		assert.Zero(t, st.actionsCalls)
	})

	t.Run("Explore resolver", func(t *testing.T) {
		st := &meetingRouteStore{mockStore: &mockStore{}}
		engine := &meetingRouteEngine{MockEngine: &querytest.MockEngine{}, result: &query.ExploreMeetingSelection{
			CacheRevision: "r", MessageIDs: []int64{},
		}}
		response := httptest.NewRecorder()
		newMeetingRouteTestServer(t, st, engine).Router().ServeHTTP(
			response,
			meetingRouteRequest("/api/v1/meetings/actions", `{"explore":{"predicate":{},"cache_revision":"r","search_provenance":{}},"status":"bad"}`),
		)

		assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		assert.Zero(t, engine.calls)
		assert.Zero(t, st.actionsCalls)
	})
}

func TestMeetingDirectScopeValidatesBeforeResolvingPerson(t *testing.T) {
	personCalls := 0
	st := &meetingRouteStore{mockStore: &mockStore{personContextFunc: func(_ context.Context, id int64) (*store.Person, error) {
		personCalls++
		return &store.Person{ID: id, ParticipantIDs: []int64{id}}, nil
	}}}
	response := httptest.NewRecorder()
	newMeetingRouteTestServer(t, st, &querytest.MockEngine{}).Router().ServeHTTP(
		response,
		meetingRouteRequest("/api/v1/meetings/metrics", `{"scope":{"person_id":7,"domains":[""]}}`),
	)

	assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.Zero(t, personCalls)
	assert.Zero(t, st.metricsCalls)
}

func TestMeetingExploreMetricsApplyResolvedDeletionToCurrentStoreSnapshot(t *testing.T) {
	requirements := require.New(t)
	fixture := storetest.New(t)
	meetingID, err := fixture.Store.UpsertMessage(&store.Message{
		ConversationID: fixture.ConvID, SourceID: fixture.Source.ID,
		SourceMessageID: "meeting-deletion-boundary", MessageType: "meeting_transcript",
	})
	requirements.NoError(err)

	messageValues := fmt.Sprintf(
		`(%d::BIGINT, 1::BIGINT, 'meeting-deletion-boundary', 101::BIGINT, 'Meeting', '', TIMESTAMP '2026-07-18 10:00:00', 0::BIGINT, false, 0::INTEGER, NULL::TIMESTAMP, NULL::BIGINT, NULL::BIGINT, 'meeting_transcript', NULL::VARCHAR, false, 2026, 7)`,
		meetingID,
	)
	engine, _ := newExploreDuckDBFixtureWithMessages(t, messageValues, meetingID)
	cacheSelection, err := engine.ResolveExploreMeetings(t.Context(), query.ExploreSelectionRequest{
		Explore: query.ExploreRequest{Context: query.Context{Deletion: query.DeletionActive}},
	}, meetingExploreMaxIDs)
	requirements.NoError(err)
	requirements.Equal([]int64{meetingID}, cacheSelection.MessageIDs)

	before, err := fixture.Store.GetMeetingMetricsContext(t.Context(), store.MeetingQueryScope{})
	requirements.NoError(err)
	requirements.Equal(int64(1), before.Totals.MeetingCount)
	_, err = fixture.Store.DB().ExecContext(t.Context(), fixture.Store.Rebind(
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), meetingID)
	requirements.NoError(err)
	archiveWide, err := fixture.Store.GetMeetingMetricsContext(t.Context(), store.MeetingQueryScope{})
	requirements.NoError(err)
	requirements.Equal(int64(1), archiveWide.Totals.MeetingCount,
		"the archive-wide default intentionally retains source-deleted meetings")

	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{}, Store: fixture.Store, Engine: engine, Logger: testLogger(),
	})
	body := fmt.Sprintf(`{
		"explore":{
			"predicate":{"filters":[{"dimension":"deletion","values":["active"]}]},
			"cache_revision":%q,
			"search_provenance":{}
		}
	}`, cacheSelection.CacheRevision)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, meetingRouteRequest("/api/v1/meetings/metrics", body))
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var metrics meetingcontent.Metrics
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &metrics))
	assert.Zero(t, metrics.Totals.MeetingCount,
		"the active cache selection must stay active-only in the current Store snapshot")
}

func TestMeetingRoutesRejectMalformedAndOversizedBodiesBeforeStoreWork(t *testing.T) {
	st := &meetingRouteStore{mockStore: &mockStore{}}
	srv := newMeetingRouteTestServer(t, st, &querytest.MockEngine{})

	tests := []struct {
		name, path, body, contentType string
		wantStatus                    int
	}{
		{name: "unknown field", path: "/api/v1/meetings/metrics", body: `{"surprise":true}`, contentType: applicationJSONMediaType, wantStatus: http.StatusBadRequest},
		{name: "trailing JSON", path: "/api/v1/meetings/metrics", body: `{}{}`, contentType: applicationJSONMediaType, wantStatus: http.StatusBadRequest},
		{name: "wrong media", path: "/api/v1/meetings/metrics", body: `{}`, contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType},
		{name: "oversized", path: "/api/v1/meetings/metrics", body: strings.Repeat(" ", (1<<20)+1), contentType: applicationJSONMediaType, wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			assert.Equal(t, test.wantStatus, response.Code, response.Body.String())
		})
	}
	assert.Zero(t, st.contextCalls)
	assert.Zero(t, st.actionsCalls)
	assert.Zero(t, st.metricsCalls)
}

func TestMeetingRoutesRejectInvalidPublicScopeAndOptionValues(t *testing.T) {
	st := &meetingRouteStore{mockStore: &mockStore{}}
	after := "2026-09-12T12:00:00Z"

	tests := []struct {
		name, path, body string
	}{
		{name: "context missing selection", path: "/api/v1/meetings/context", body: `{}`},
		{name: "context both selection modes", path: "/api/v1/meetings/context", body: `{"message_ids":[1],"selection":{"mode":"explicit","predicate":{},"row_keys":[],"cache_revision":"r"}}`},
		{name: "context missing predicate", path: "/api/v1/meetings/context", body: `{"selection":{"mode":"all_matching","cache_revision":"r","search_provenance":{}}}`},
		{name: "context missing search provenance", path: "/api/v1/meetings/context", body: `{"selection":{"mode":"all_matching","predicate":{},"cache_revision":"r"}}`},
		{name: "context empty IDs", path: "/api/v1/meetings/context", body: `{"message_ids":[]}`},
		{name: "context zero ID", path: "/api/v1/meetings/context", body: `{"message_ids":[0]}`},
		{name: "context unsafe ID", path: "/api/v1/meetings/context", body: `{"message_ids":[9007199254740992]}`},
		{name: "context explicit zero budget", path: "/api/v1/meetings/context", body: `{"message_ids":[1],"max_bytes":0}`},
		{name: "context null budget", path: "/api/v1/meetings/context", body: `{"message_ids":[1],"max_bytes":null}`},
		{name: "context small budget", path: "/api/v1/meetings/context", body: `{"message_ids":[1],"max_bytes":4095}`},
		{name: "context format", path: "/api/v1/meetings/context", body: `{"message_ids":[1],"format":"html"}`},
		{name: "context empty format", path: "/api/v1/meetings/context", body: `{"message_ids":[1],"format":""}`},
		{name: "context null transcript option", path: "/api/v1/meetings/context", body: `{"message_ids":[1],"include_transcript":null}`},
		{name: "actions both scopes", path: "/api/v1/meetings/actions", body: `{"scope":{},"explore":{"predicate":{},"cache_revision":"r","search_provenance":{}}}`},
		{name: "actions explicit zero limit", path: "/api/v1/meetings/actions", body: `{"limit":0}`},
		{name: "actions null limit", path: "/api/v1/meetings/actions", body: `{"limit":null}`},
		{name: "actions excessive limit", path: "/api/v1/meetings/actions", body: `{"limit":201}`},
		{name: "actions status", path: "/api/v1/meetings/actions", body: `{"status":"open"}`},
		{name: "actions empty status", path: "/api/v1/meetings/actions", body: `{"status":""}`},
		{name: "actions null status", path: "/api/v1/meetings/actions", body: `{"status":null}`},
		{name: "actions query", path: "/api/v1/meetings/actions", body: `{"query":"` + strings.Repeat("q", 257) + `"}`},
		{name: "scope empty sources", path: "/api/v1/meetings/metrics", body: `{"scope":{"source_ids":[]}}`},
		{name: "scope zero person supplied", path: "/api/v1/meetings/metrics", body: `{"scope":{"person_id":0}}`},
		{name: "scope null person supplied", path: "/api/v1/meetings/metrics", body: `{"scope":{"person_id":null}}`},
		{name: "scope singular conflict", path: "/api/v1/meetings/metrics", body: `{"scope":{"person_id":1,"participant_id":2}}`},
		{name: "scope plural conflict", path: "/api/v1/meetings/metrics", body: `{"scope":{"person_id":1,"participant_ids":[2]}}`},
		{name: "scope bad range", path: "/api/v1/meetings/metrics", body: `{"scope":{"after":"` + after + `","before":"` + after + `"}}`},
		{name: "scope null date", path: "/api/v1/meetings/metrics", body: `{"scope":{"after":null}}`},
		{name: "scope deletion", path: "/api/v1/meetings/metrics", body: `{"scope":{"deletion":"local"}}`},
		{name: "scope empty domain", path: "/api/v1/meetings/metrics", body: `{"scope":{"domains":[""]}}`},
		{name: "explore null predicate", path: "/api/v1/meetings/metrics", body: `{"explore":{"predicate":null,"cache_revision":"r","search_provenance":{}}}`},
		{name: "explore missing predicate", path: "/api/v1/meetings/metrics", body: `{"explore":{"cache_revision":"r","search_provenance":{}}}`},
		{name: "explore missing search provenance", path: "/api/v1/meetings/metrics", body: `{"explore":{"predicate":{},"cache_revision":"r"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := newMeetingRouteTestServer(t, st, &querytest.MockEngine{})
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, meetingRouteRequest(test.path, test.body))
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		})
	}
	assert.Zero(t, st.contextCalls)
	assert.Zero(t, st.actionsCalls)
	assert.Zero(t, st.metricsCalls)
}

func TestMeetingScopeResolvesDurablePersonAndPreservesExactPluralIDs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := &meetingRouteStore{mockStore: &mockStore{personContextFunc: func(_ context.Context, id int64) (*store.Person, error) {
		return &store.Person{ID: id, ParticipantIDs: []int64{9, 11}}, nil
	}}, metricsResult: &meetingcontent.Metrics{DurationByBasis: []meetingcontent.BasisTotals{}, Months: []meetingcontent.MonthTotals{}}}
	srv := newMeetingRouteTestServer(t, st, &querytest.MockEngine{})

	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, meetingRouteRequest(
		"/api/v1/meetings/metrics", `{"scope":{"person_id":7}}`,
	))
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	requirements.NotNil(st.metricsScope.Person)
	assertions.Equal([]int64{9, 11}, st.metricsScope.Person.ParticipantIDs)

	response = httptest.NewRecorder()
	srv.Router().ServeHTTP(response, meetingRouteRequest(
		"/api/v1/meetings/metrics", `{"scope":{"participant_ids":[11]}}`,
	))
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Nil(st.metricsScope.Person)
	assertions.Equal([]int64{11}, st.metricsScope.ParticipantIDs)
}

func TestMeetingExploreScopeRejectsMixedSelectionsAndBindsExactAuthority(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := &meetingRouteStore{
		mockStore:     &mockStore{},
		actionsResult: &meetingcontent.ActionsPage{Rows: []meetingcontent.ActionRow{}},
	}
	engine := &meetingRouteEngine{MockEngine: &querytest.MockEngine{}, result: &query.ExploreMeetingSelection{
		SelectedCount: 2, MeetingCount: 1, MessageIDs: []int64{41}, CacheRevision: "cache-r1",
	}}
	srv := newMeetingRouteTestServer(t, st, engine)
	selection := `{"selection":{"mode":"all_matching","predicate":{},"cache_revision":"cache-r1","search_provenance":{}}}`

	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, meetingRouteRequest("/api/v1/meetings/context", selection))
	requirements.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	var failure ErrorResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &failure))
	assertions.Equal("selection_not_all_meetings", failure.Error)
	assertions.Zero(st.contextCalls)

	engine.result = &query.ExploreMeetingSelection{
		SelectedCount: 1, MeetingCount: 1, MessageIDs: []int64{41}, CacheRevision: "cache-r1",
	}
	explore := `{"explore":{"predicate":{},"cache_revision":"cache-r1","search_provenance":{}}}`
	response = httptest.NewRecorder()
	srv.Router().ServeHTTP(response, meetingRouteRequest("/api/v1/meetings/actions", explore))
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	requirements.NotNil(st.actionsQuery.Scope.MessageIDs)
	assertions.Equal([]int64{41}, *st.actionsQuery.Scope.MessageIDs)
	assertions.NotEmpty(st.actionsQuery.Scope.Authority)
	assertions.Equal(10_000, engine.limit)
}

func TestMeetingExploreScopeUsesSemanticSnapshotAndRejectsSaturatedPool(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	generation := int64(7)
	st := &meetingRouteStore{
		mockStore:     &mockStore{messages: []APIMessage{{ID: 41}}, total: 1, stats: &StoreStats{}},
		actionsResult: &meetingcontent.ActionsPage{Rows: []meetingcontent.ActionRow{}},
	}
	engine := &meetingRouteEngine{MockEngine: &querytest.MockEngine{}, result: &query.ExploreMeetingSelection{
		SelectedCount: 1, MeetingCount: 1, MessageIDs: []int64{41}, CacheRevision: "cache-r1",
		SearchProvenance: query.SearchProvenance{VectorGeneration: &generation},
	}}
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 7, Model: "test", Dimension: 2, Fingerprint: "test:2", State: vector.GenerationActive,
		},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: st, Engine: engine,
		HybridEngine: hybrid.NewEngine(
			backend, nil, realEmbedder{dim: 2}, hybrid.Config{ExpectedFingerprint: "test:2"},
		),
		Backend: backend, Logger: testLogger(),
	})
	predicate := ExploreHTTPRequest{Query: "alpha", SearchMode: exploreSearchModeSemantic}
	snapshotID := srv.exploreState.issueSnapshot(exploreCandidateSnapshot{
		RequestHash: exploreSnapshotRequestHash(predicate), IDs: []int64{41}, Generation: generation,
	})
	body := fmt.Sprintf(`{
		"explore":{
			"predicate":{"query":"alpha","search_mode":"semantic"},
			"cache_revision":"cache-r1","search_provenance":{"vector_generation":7},
			"candidate_snapshot_id":%q
		}
	}`, snapshotID)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, meetingRouteRequest("/api/v1/meetings/actions", body))
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	assertions.Equal([]int64{41}, engine.request.Explore.Search.CandidateMessageIDs)
	requirements.NotNil(engine.request.Explore.Search.VectorGeneration)
	assertions.Equal(generation, *engine.request.Explore.Search.VectorGeneration)
	requirements.NotNil(st.actionsQuery.Scope.MessageIDs)
	assertions.Equal([]int64{41}, *st.actionsQuery.Scope.MessageIDs)

	saturatedID := srv.exploreState.issueSnapshot(exploreCandidateSnapshot{
		RequestHash: exploreSnapshotRequestHash(predicate), IDs: []int64{41}, Generation: generation,
		PoolSaturated: true,
	})
	response = httptest.NewRecorder()
	srv.Router().ServeHTTP(response, meetingRouteRequest(
		"/api/v1/meetings/actions", strings.Replace(body, snapshotID, saturatedID, 1),
	))
	requirements.Equal(http.StatusConflict, response.Code, response.Body.String())
	var failure ErrorResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &failure))
	assertions.Equal("candidate_pool_saturated", failure.Error)
	assertions.Equal(1, engine.calls, "saturated scope never reaches the exact resolver")
}

func TestMeetingExploreScopeMapsPopulationAndRevisionFailures(t *testing.T) {
	tests := []struct {
		name       string
		result     *query.ExploreMeetingSelection
		storeErr   error
		wantStatus int
		wantCode   string
	}{
		{name: "context selection too large", result: &query.ExploreMeetingSelection{SelectedCount: 101, MeetingCount: 101, LimitExceeded: true, CacheRevision: "r"}, wantStatus: http.StatusBadRequest, wantCode: "meeting_selection_too_large"},
		{name: "actions scope too large", result: &query.ExploreMeetingSelection{SelectedCount: 10001, MeetingCount: 10001, LimitExceeded: true, CacheRevision: "r"}, wantStatus: http.StatusConflict, wantCode: "meeting_scope_too_large"},
		{name: "cache changed", result: &query.ExploreMeetingSelection{SelectedCount: 1, MeetingCount: 1, MessageIDs: []int64{1}, CacheRevision: "new"}, wantStatus: http.StatusConflict, wantCode: "archive_revision_changed"},
		{name: "scope changed", result: &query.ExploreMeetingSelection{SelectedCount: 1, MeetingCount: 1, MessageIDs: []int64{1}, CacheRevision: "r"}, storeErr: store.ErrMeetingScopeChanged, wantStatus: http.StatusConflict, wantCode: "meeting_scope_changed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := &meetingRouteStore{mockStore: &mockStore{}, actionsErr: test.storeErr}
			engine := &meetingRouteEngine{MockEngine: &querytest.MockEngine{}, result: test.result}
			srv := newMeetingRouteTestServer(t, st, engine)
			path := "/api/v1/meetings/actions"
			body := `{"explore":{"predicate":{},"cache_revision":"r","search_provenance":{}}}`
			if strings.Contains(test.name, "context") {
				path = "/api/v1/meetings/context"
				body = `{"selection":{"mode":"all_matching","predicate":{},"cache_revision":"r","search_provenance":{}}}`
			}
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, meetingRouteRequest(path, body))
			require.Equal(t, test.wantStatus, response.Code, response.Body.String())
			var failure ErrorResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &failure))
			assert.Equal(t, test.wantCode, failure.Error)
		})
	}
}

func TestMeetingExploreContextMapsEligibilityDriftToScopeChanged(t *testing.T) {
	st := &meetingRouteStore{mockStore: &mockStore{}, contextErr: store.ErrMeetingNotFound}
	engine := &meetingRouteEngine{MockEngine: &querytest.MockEngine{}, result: &query.ExploreMeetingSelection{
		SelectedCount: 1, MeetingCount: 1, MessageIDs: []int64{41}, CacheRevision: "r",
	}}
	srv := newMeetingRouteTestServer(t, st, engine)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, meetingRouteRequest(
		"/api/v1/meetings/context",
		`{"selection":{"mode":"all_matching","predicate":{},"cache_revision":"r","search_provenance":{}}}`,
	))

	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	var failure ErrorResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &failure))
	assert.Equal(t, "meeting_scope_changed", failure.Error)
}

func TestMeetingStoreErrorsUseStableHTTPContracts(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "missing", err: store.ErrMeetingNotFound, wantStatus: http.StatusNotFound, wantCode: "meeting_not_found"},
		{name: "wrong type", err: store.ErrNotMeeting, wantStatus: http.StatusBadRequest, wantCode: "not_a_meeting"},
		{name: "projection", err: store.ErrMeetingProjectionUnavailable, wantStatus: http.StatusServiceUnavailable, wantCode: "meeting_projection_unavailable"},
		{name: "cursor", err: store.ErrMeetingInvalidCursor, wantStatus: http.StatusBadRequest, wantCode: "invalid_cursor"},
		{name: "budget", err: meetingcontent.ErrBudgetTooSmall, wantStatus: http.StatusBadRequest, wantCode: "context_budget_too_small"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := &meetingRouteStore{mockStore: &mockStore{}, contextErr: test.err}
			srv := newMeetingRouteTestServer(t, st, &querytest.MockEngine{})
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, meetingRouteRequest(
				"/api/v1/meetings/context", `{"message_ids":[1]}`,
			))
			require.Equal(t, test.wantStatus, response.Code, response.Body.String())
			var failure ErrorResponse
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &failure))
			assert.Equal(t, test.wantCode, failure.Error)
		})
	}
}

func TestMeetingRouteContextCancellationIsStructured(t *testing.T) {
	st := &meetingRouteStore{mockStore: &mockStore{}, contextErr: context.Canceled}
	srv := newMeetingRouteTestServer(t, st, &querytest.MockEngine{})
	request := meetingRouteRequest("/api/v1/meetings/context", `{"message_ids":[1]}`)
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request.WithContext(ctx))
	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
}

func TestMeetingRouteStoreErrorDoesNotLeakDetails(t *testing.T) {
	st := &meetingRouteStore{mockStore: &mockStore{}, contextErr: errors.New("private transcript content")}
	srv := newMeetingRouteTestServer(t, st, &querytest.MockEngine{})
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, meetingRouteRequest(
		"/api/v1/meetings/context", `{"message_ids":[1]}`,
	))
	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.NotContains(t, response.Body.String(), "private transcript content")
}
