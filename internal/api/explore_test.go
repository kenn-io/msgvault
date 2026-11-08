package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestExploreRejectsUnknownFilterDimension(t *testing.T) {
	t.Parallel()
	srv := newTestServerWithEngine(t, newExploreDuckDBFixture(t))
	response := postExploreJSON(t, srv, "/api/v1/explore", `{"filters":[{"dimension":"sql","values":["select *"]}]}`)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.Contains(t, response.Body.String(), "unknown filter dimension")
}

func TestExploreMailingListFilterAcceptsValuesAndConjoinsRepeats(t *testing.T) {
	t.Parallel()
	context, err := exploreContext([]ExploreFilter{
		{Dimension: "mailing_list", Values: []string{"<one@example.test>", "<two@example.test>"}},
		{Dimension: "mailing_list", Values: []string{"<ONE@EXAMPLE.TEST>"}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"<one@example.test>", "<two@example.test>"}, context.MailingLists)
	assert.Equal(t, [][]string{{"<ONE@EXAMPLE.TEST>"}}, context.AdditionalMailingListGroups)
}

func TestParseMessageFilterPreservesExactListID(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/messages/filter?list_id=%3CDev_1%40Example.Test%3E", nil)
	filter, err := parseMessageFilter(request)
	require.NoError(t, err)
	assert.Equal(t, "<Dev_1@Example.Test>", filter.ListID)
}

func TestExploreCursorAcceptsCanonicalFilterValueOrdering(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	srv := newTestServerWithEngine(t, newExploreDuckDBFixture(t))
	first := postExploreJSON(t, srv, "/api/v1/explore", `{
		"filters":[{"dimension":"source","values":["2","1"]}],"limit":1
	}`)
	requirements.Equal(http.StatusOK, first.Code, first.Body.String())
	var page ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(first.Body.Bytes(), &page))
	requirements.NotEmpty(page.NextCursor)

	second := postExploreJSON(t, srv, "/api/v1/explore", `{
		"filters":[{"dimension":"source","values":["1","2"]}],"limit":1,"cursor":"`+page.NextCursor+`"
	}`)
	requirements.Equal(http.StatusOK, second.Code, second.Body.String())
	var secondPage ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(second.Body.Bytes(), &secondPage))
	requirements.Len(secondPage.Rows, 1)
	assertions.Equal("Older", secondPage.Rows[0].Title)
	assertions.Equal(page.CacheRevision, secondPage.CacheRevision)
}

func TestExploreUnavailableReturnsNamedReadinessAndRecovery(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, _ := newTestServerWithMockStore(t)
	response := postExploreJSON(t, srv, "/api/v1/explore", `{}`)
	requirements.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	var body ExploreCacheUnavailableResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assertions.Equal("analytical_cache_unavailable", body.Error)
	assertions.Equal(query.CacheAbsent, body.Readiness)
	assertions.NotEmpty(body.RecoveryAction)
}

func TestExploreReportsTransientCacheInitialization(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	opts := testServerOptions(t, nil)
	opts.AnalyticsMode = AnalyticsModeInitializing
	srv := NewServerWithOptions(opts)

	response := postExploreJSON(t, srv, "/api/v1/explore", `{}`)
	requirements.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	var body ExploreCacheUnavailableResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assertions.Equal("analytical_cache_unavailable", body.Error)
	assertions.Equal(query.CacheReadiness("building"), body.Readiness)
	assertions.Equal("The analytical cache is being prepared", body.Message)
	assertions.Empty(body.RecoveryAction, "an in-progress automatic build must not prescribe a manual rebuild")
	var generatedBody generated.ExploreCacheUnavailableResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &generatedBody))
	requirements.NoError(generatedBody.Validate(), "the generated client must accept the server's building response")
}

func TestLiveExplorePaginationWarningsAndCacheRefusals(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, _, _ := liveExploreTestServer(t)
	srv.SetAnalyticsInitializationActive(true)
	response := postExploreJSON(t, srv, "/api/v1/explore", `{"limit":1}`)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var page ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &page))
	requirements.Len(page.Rows, 1)
	assertions.Equal("Newest", page.Rows[0].Title)
	assertions.Contains(page.Warnings[0], "while the analytical cache rebuilds")
	requirements.NotEmpty(page.NextCursor)
	srv.SetAnalyticsInitializationActive(false)
	next := postExploreJSON(t, srv, "/api/v1/explore", `{"limit":1,"cursor":"`+page.NextCursor+`"}`)
	requirements.Equal(http.StatusOK, next.Code, next.Body.String())
	var second ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(next.Body.Bytes(), &second))
	requirements.Len(second.Rows, 1)
	assertions.Equal("Older", second.Rows[0].Title)
	assertions.Equal(page.CacheRevision, second.CacheRevision)
	assertions.Contains(second.Warnings[0], "then restart msgvault")

	for route, body := range map[string]string{
		"/api/v1/explore/groups":    `{"grouping":["source"]}`,
		"/api/v1/explore/preflight": `{"selection":{"mode":"all_matching","predicate":{},"cache_revision":"` + page.CacheRevision + `"}}`,
	} {
		refused := postExploreJSON(t, srv, route, body)
		assertions.Equal(http.StatusServiceUnavailable, refused.Code, refused.Body.String())
	}
	srv.SetAnalyticsEngine(newExploreDuckDBFixture(t), AnalyticsModeDuckDB)
	changed := postExploreJSON(t, srv, "/api/v1/explore", `{"limit":1,"cursor":"`+page.NextCursor+`"}`)
	assertions.Equal(http.StatusConflict, changed.Code, changed.Body.String())
	assertions.Contains(changed.Body.String(), "archive_revision_changed")
	selection := postExploreJSON(t, srv, "/api/v1/explore/preflight", `{"selection":{"mode":"all_matching","predicate":{},"cache_revision":"`+page.CacheRevision+`"}}`)
	assertions.Equal(http.StatusConflict, selection.Code, selection.Body.String())
	assertions.Contains(selection.Body.String(), "archive_revision_changed")
}

func liveExploreTestServer(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	_, err := st.DB().ExecContext(t.Context(), `INSERT INTO sources(id,source_type,identifier) VALUES(1,'gmail','archive@example.test');
 INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type) VALUES(1,1,'thread','email_thread');
 INSERT INTO messages(id,source_id,source_message_id,conversation_id,message_type,subject,snippet,sent_at) VALUES
 (1,1,'older',1,'email','Older','needle OR needle','2026-07-01T00:00:00Z'),(2,1,'newest',1,'email','Newest','needle OR needle','2026-07-02T00:00:00Z'); INSERT INTO message_bodies(message_id,body_text) VALUES(1,'needle OR needle'),(2,'needle OR needle');`)
	requirements.NoError(err)
	_, err = st.RebuildFTS(nil)
	requirements.NoError(err)
	engine := query.NewLiveExploreEngine(st.DB(), path)
	t.Cleanup(func() { requirements.NoError(engine.Close()) })
	srv := newTestServerWithEngine(t, engine)
	srv.SetAnalyticsEngine(engine, AnalyticsModeSQLFallback)
	return srv, st, path
}

func TestLiveExploreRefusesSemanticBeforeCandidatesOrReader(t *testing.T) {
	t.Parallel()
	srv, backend := newReviewSemanticIntersectionServer(t)
	live, _, _ := liveExploreTestServer(t)
	engine, ok := live.queryEngineForContext(t.Context()).(*query.LiveExploreEngine)
	require.True(t, ok)
	srv.SetAnalyticsEngine(engine, AnalyticsModeSQLFallback)
	for _, held := range []bool{false, true} {
		if held {
			_, release, _, err := engine.BeginExploreSnapshot(t.Context())
			require.NoError(t, err)
			defer release()
		}
		for _, mode := range []string{"semantic", "hybrid"} {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/explore", bytes.NewBufferString(`{"query":"needle","search_mode":"`+mode+`"}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			cancel()
			assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "analytical_cache_unavailable")
			assert.Zero(t, backend.searchLimit, "unsupported searches must not query the vector index")
		}
	}
}

type liveExploreSearchStore struct {
	*mockStore

	archive         *store.Store
	afterCandidates func()
}

func (s *liveExploreSearchStore) SearchMessageIDsQueryContext(ctx context.Context, q *search.Query, limit int) ([]int64, int64, error) {
	ids, total, err := s.archive.SearchMessageIDsQueryContext(ctx, q, limit)
	if s.afterCandidates != nil {
		s.afterCandidates()
	}
	return ids, total, err
}

func TestLiveExploreSearchSharesSnapshot(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st, path := liveExploreTestServer(t)
	writer, err := store.OpenForTest(t.Context(), path)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(writer.Close()) })
	mock, ok := srv.store.(*mockStore)
	requirements.True(ok)
	searchStore := &liveExploreSearchStore{mockStore: mock, archive: st}
	srv.store = searchStore
	searchStore.afterCandidates = func() {
		_, err := writer.DB().ExecContext(t.Context(), "UPDATE messages SET subject='Changed' WHERE id=2")
		requirements.NoError(err)
		searchStore.afterCandidates = nil
	}
	first := postExploreJSON(t, srv, "/api/v1/explore", `{"query":"needle OR needle","search_mode":"full_text","limit":1}`)
	requirements.Equal(http.StatusOK, first.Code, first.Body.String())
	var page ExploreHTTPResponse
	requirements.NoError(json.Unmarshal(first.Body.Bytes(), &page))
	requirements.Len(page.Rows, 1)
	assertions.Equal("Newest", page.Rows[0].Title)
}

func TestExploreUnavailableUsesRequestInitializationSnapshot(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	opts := testServerOptions(t, nil)
	opts.AnalyticsMode = AnalyticsModeSQLFallback
	opts.AnalyticsInitializationActive = true
	srv := NewServerWithOptions(opts)
	snapshot := srv.currentAnalyticsState()
	srv.SetAnalyticsInitializationActive(false)

	response := httptest.NewRecorder()
	ctx := context.WithValue(t.Context(), analyticsEngineContextKey{}, snapshot)
	srv.writeExploreUnavailable(ctx, response, query.CacheAbsent)

	requirements.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	var body ExploreCacheUnavailableResponse
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assertions.Equal(query.CacheReadiness("building"), body.Readiness)
}

func TestExploreServerStateBoundsAndExpiresTransientCapabilities(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	state := newExploreServerState(func() time.Time { return now })
	oldestOperationToken := state.issueOperation("oldest-selection", 1, "cache")
	var operationToken string
	for range exploreStateMaxEntries + 10 {
		now = now.Add(time.Millisecond)
		state.issueSnapshot(exploreCandidateSnapshot{RequestHash: "request", IDs: []int64{1}})
		operationToken = state.issueOperation("selection", 1, "cache")
		state.putMatchCounts(randomExploreToken(), map[string]int64{"row": 1})
	}
	state.mu.Lock()
	assertions.LessOrEqual(len(state.snapshots), exploreStateMaxEntries)
	assertions.LessOrEqual(len(state.operations), exploreStateMaxEntries)
	assertions.LessOrEqual(len(state.matchCounts), exploreStateMaxEntries)
	state.mu.Unlock()
	_, oldestOperationExists := state.operation(oldestOperationToken, "oldest-selection")
	assertions.False(oldestOperationExists, "the oldest operation grant is evicted at capacity")
	_, wrongSelectionExists := state.operation(operationToken, "other-selection")
	assertions.False(wrongSelectionExists, "operation grants stay bound to their preflight selection")
	grant, operationExists := state.operation(operationToken, "selection")
	assertions.True(operationExists)
	assertions.Equal(int64(1), grant.Count)
	assertions.Equal("cache", grant.Revision)

	now = now.Add(exploreCandidateSnapshotTTL + time.Second)
	state.issueSnapshot(exploreCandidateSnapshot{RequestHash: "fresh"})
	_, operationExists = state.operation(operationToken, "selection")
	assertions.False(operationExists)
	state.mu.Lock()
	assertions.Len(state.snapshots, 1)
	assertions.Empty(state.matchCounts)
	state.mu.Unlock()
}

func TestLiveExploreRejectsCursorAfterChatMoves(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	srv, st, _ := liveExploreTestServer(t)
	_, err := st.DB().ExecContext(t.Context(), `INSERT INTO conversations(id,source_id,source_conversation_id,conversation_type) VALUES(2,1,'chat','group_chat'); INSERT INTO messages(id,source_id,source_message_id,conversation_id,message_type,sent_at) VALUES(3,1,'chat-old',2,'whatsapp','2026-06-01T00:00:00Z')`)
	require.NoError(err)
	response := postExploreJSON(t, srv, "/api/v1/explore", `{"limit":1}`)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var first ExploreHTTPResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &first))
	require.NotEmpty(first.NextCursor)
	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO messages(id,source_id,source_message_id,conversation_id,message_type,sent_at) VALUES(4,1,'chat-new',2,'whatsapp','2026-07-03T00:00:00Z')`)
	require.NoError(err)
	response = postExploreJSON(t, srv, "/api/v1/explore", `{"limit":1,"cursor":"`+first.NextCursor+`"}`)
	assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "archive_revision_changed")
}
