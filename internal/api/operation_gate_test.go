package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
)

type recordingOperationGate struct {
	mu         sync.Mutex
	allow      bool
	beginCalls int
	doneCalls  int
}

func (g *recordingOperationGate) BeginWork() (func(), bool) {
	return g.BeginWorkContext(context.Background())
}

func (g *recordingOperationGate) BeginWorkContext(ctx context.Context) (func(), bool) {
	if ctx != nil && ctx.Err() != nil {
		return func() {}, false
	}
	g.mu.Lock()
	g.beginCalls++
	allow := g.allow
	g.mu.Unlock()
	if !allow {
		return func() {}, false
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.doneCalls++
			g.mu.Unlock()
		})
	}, true
}

func (g *recordingOperationGate) counts() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.beginCalls, g.doneCalls
}

func TestOperationGateMiddlewareSkipsReadMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			assert := assert.New(t)

			gate := &recordingOperationGate{allow: true}
			called := false
			handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))

			req := httptest.NewRequest(method, "/api/v1/messages", nil)
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			assert.True(called, "handler called")
			assert.Equal(http.StatusNoContent, resp.Code, "status")
			begin, done := gate.counts()
			assert.Equal(0, begin, "begin calls")
			assert.Equal(0, done, "done calls")
		})
	}
}

func TestOperationGateMiddlewareBypassesUnauthenticatedRequests(t *testing.T) {
	assert := assert.New(t)

	gate := &recordingOperationGate{allow: false}
	called := false
	authorized := func(r *http.Request) bool {
		return r.Header.Get("X-Api-Key") == "secret"
	}
	handler := operationGateMiddleware(gate, authorized)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusUnauthorized)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/sync", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	assert.True(called, "unauthenticated request passes through to the auth layer")
	assert.Equal(http.StatusUnauthorized, resp.Code, "status")
	begin, done := gate.counts()
	assert.Equal(0, begin, "unauthenticated request must not touch gate state")
	assert.Equal(0, done, "done calls")

	authedReq := httptest.NewRequest(http.MethodPost, "/api/v1/cli/sync", nil)
	authedReq.Header.Set("X-Api-Key", "secret")
	authedResp := httptest.NewRecorder()
	handler.ServeHTTP(authedResp, authedReq)
	begin, _ = gate.counts()
	assert.Equal(1, begin, "authenticated request is gated")
}

func TestOperationGateMiddlewareGatesMutatingMethods(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			gate := &recordingOperationGate{allow: true}
			handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))

			req := httptest.NewRequest(method, "/api/v1/cli/collections", nil)
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)

			assert.Equal(t, http.StatusNoContent, resp.Code, "status")
			begin, done := gate.counts()
			assert.Equal(t, 1, begin, "begin calls")
			assert.Equal(t, 1, done, "done calls")
		})
	}
}

func TestOperationGateMiddlewareSkipsDaemonShutdown(t *testing.T) {
	assert := assert.New(t)

	gate := &recordingOperationGate{allow: true}
	called := false
	handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	}))

	req := httptest.NewRequest(http.MethodPost, DaemonShutdownPath, nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	assert.True(called, "handler called")
	assert.Equal(http.StatusAccepted, resp.Code, "status")
	begin, done := gate.counts()
	assert.Equal(0, begin, "begin calls")
	assert.Equal(0, done, "done calls")
}

func TestOperationGateMiddlewareSkipsLogCLIRunAndRestoresBody(t *testing.T) {
	assert := assert.New(t)
	gate := &recordingOperationGate{allow: false}
	handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Args []string `json:"args"`
		}
		if assert.NoError(json.NewDecoder(r.Body).Decode(&req), "decode body") {
			assert.Equal([]string{"logs", "--follow"}, req.Args, "args")
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(`{"args":["logs","--follow"]}`))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	assert.Equal(http.StatusNoContent, resp.Code, "status")
	begin, done := gate.counts()
	assert.Equal(0, begin, "begin calls")
	assert.Equal(0, done, "done calls")
}

func TestOperationGateMiddlewareRejectsOversizedCLIRunInspectionBody(t *testing.T) {
	assert := assert.New(t)
	gate := &recordingOperationGate{allow: false}
	handlerCalled := false
	handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))

	body := `{"args":["logs"],"padding":"` + strings.Repeat("x", 2<<20) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(body))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	assert.Equal(http.StatusRequestEntityTooLarge, resp.Code, "status")
	assert.Equal("application/json", resp.Header().Get("Content-Type"), "content type")
	var errResp ErrorResponse
	if assert.NoError(json.Unmarshal(resp.Body.Bytes(), &errResp), "decode error envelope") {
		assert.Equal("request_too_large", errResp.Error, "error code")
	}
	assert.False(handlerCalled, "handler should not receive oversized classification body")
	begin, done := gate.counts()
	assert.Equal(0, begin, "begin calls")
	assert.Equal(0, done, "done calls")
}

func TestOperationGateMiddlewareStillGatesMutatingCLIRun(t *testing.T) {
	assert := assert.New(t)
	gate := &recordingOperationGate{allow: true}
	handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(`{"args":["import-mbox","archive.mbox"]}`))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	assert.Equal(http.StatusNoContent, resp.Code, "status")
	begin, done := gate.counts()
	assert.Equal(1, begin, "begin calls")
	assert.Equal(1, done, "done calls")
}

func TestOperationGateMiddlewareRejectsUnavailableGate(t *testing.T) {
	assert := assert.New(t)

	gate := &recordingOperationGate{allow: false}
	called := false
	handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	assert.False(called, "handler should not run")
	assert.Equal(http.StatusServiceUnavailable, resp.Code, "status")
	assert.Equal("application/json", resp.Header().Get("Content-Type"), "content type")
	var errResp ErrorResponse
	if assert.NoError(json.Unmarshal(resp.Body.Bytes(), &errResp), "decode error envelope") {
		assert.Equal("server_busy", errResp.Error, "error code")
		assert.Equal("server is busy or shutting down", errResp.Message, "error message")
	}
	begin, done := gate.counts()
	assert.Equal(1, begin, "begin calls")
	assert.Equal(0, done, "done calls")
}

func TestOperationGateMiddlewareStopsWaitingWhenRequestContextCancels(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	gate := NewSerialOperationGate()
	release, ok := gate.BeginWork()
	require.True(ok, "occupy gate")

	handlerCalled := make(chan struct{}, 1)
	handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		handlerCalled <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", nil).WithContext(ctx)
	resp := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(resp, req)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		release()
		require.FailNow("handler did not return after request cancellation")
	}
	release()

	select {
	case <-handlerCalled:
		assert.Fail("handler should not run after request cancellation")
	default:
	}
	assert.Equal(http.StatusServiceUnavailable, resp.Code, "status")
}

func TestSerialOperationGateDrainRejectsQueuedWorkAndWaitsForActive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	gate := NewSerialOperationGate()

	releaseActive, ok := gate.BeginWork()
	require.True(ok, "begin active work")

	queuedDone := make(chan bool, 1)
	go func() {
		releaseQueued, queuedOK := gate.BeginWorkContext(context.Background())
		if queuedOK {
			releaseQueued()
		}
		queuedDone <- queuedOK
	}()

	select {
	case queuedOK := <-queuedDone:
		assert.Fail("queued work returned before drain", "ok=%v", queuedOK)
	case <-time.After(25 * time.Millisecond):
	}

	drainDone := make(chan error, 1)
	go func() {
		drainDone <- gate.Drain(context.Background())
	}()

	select {
	case queuedOK := <-queuedDone:
		assert.False(queuedOK, "queued work should be rejected by drain")
	case <-time.After(500 * time.Millisecond):
		releaseActive()
		require.FailNow("queued work did not return after drain started")
	}

	select {
	case err := <-drainDone:
		assert.Fail("drain returned before active work released", "err=%v", err)
	case <-time.After(25 * time.Millisecond):
	}

	releaseActive()
	select {
	case err := <-drainDone:
		require.NoError(err, "drain")
	case <-time.After(500 * time.Millisecond):
		require.FailNow("drain did not finish after active work released")
	}

	releaseAfterDrain, ok := gate.BeginWork()
	if ok {
		releaseAfterDrain()
	}
	assert.False(ok, "new work should be rejected after drain")
}

func TestServerOperationGateWrapsMutatingRequests(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	gate := &recordingOperationGate{allow: true}
	srv := NewServerWithOptions(ServerOptions{
		Config:        &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Logger:        testLogger(),
		OperationGate: gate,
	})

	getReq := httptest.NewRequest(http.MethodGet, "/health", nil)
	getResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(getResp, getReq)
	assert.Equal(http.StatusOK, getResp.Code, "health status")

	postReq := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", nil)
	postResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(postResp, postReq)
	assert.Equal(http.StatusBadRequest, postResp.Code, "bad account request status")

	begin, done := gate.counts()
	require.Equal(1, begin, "mutating request should enter gate")
	assert.Equal(1, done, "mutating request should release gate")
}

func TestOperationGateMiddlewareSkipsReadOnlyCLIRunCommands(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"embeddings list", `{"args":["embeddings","list"]}`},
		{"list-deletions", `{"args":["list-deletions"]}`},
		{"show-deletion with id", `{"args":["show-deletion","batch-123"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			gate := &recordingOperationGate{allow: false}
			called := false
			handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))

			req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(tc.body))
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)

			assert.True(called, "read-only command should bypass the gate")
			assert.Equal(http.StatusNoContent, resp.Code, "status")
			begin, _ := gate.counts()
			assert.Equal(0, begin, "begin calls")
		})
	}
}

func TestOperationGateMiddlewareSkipsSelfGatedCLIRunCommands(t *testing.T) {
	assert := assert.New(t)
	gate := &recordingOperationGate{allow: false}
	called := false
	handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(`{"args":["backup","create"]}`))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	assert.True(called, "self-gated command should bypass the middleware gate even while another holder is active")
	assert.Equal(http.StatusNoContent, resp.Code, "status")
	begin, _ := gate.counts()
	assert.Equal(0, begin, "begin calls")
}

func TestOperationGateMiddlewareSkipsReadOnlyPaths(t *testing.T) {
	paths := []string{
		"/api/v1/query",
		"/api/v1/cli/add-calendar/plan",
		"/api/v1/cli/delete-staged/plan",
		"/api/v1/cli/embeddings/plan",
		"/api/v1/cli/deduplicate/plan",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			assert := assert.New(t)
			gate := &recordingOperationGate{allow: false}
			called := false
			handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))

			req := httptest.NewRequest(http.MethodPost, path, nil)
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)

			assert.True(called, "read-only path should bypass the gate")
			begin, _ := gate.counts()
			assert.Equal(0, begin, "begin calls")
		})
	}
}

func TestOperationGateMiddlewareSkipsReadOnlyAnalyticalPosts(t *testing.T) {
	paths := []string{
		"/api/v1/explore",
		"/api/v1/explore/groups",
		"/api/v1/explore/preflight",
		"/api/v1/explore/match-counts",
		"/api/v1/explore/files",
		"/api/v1/files/search",
		"/api/v1/files/groups",
		"/api/v1/people/search",
		"/api/v1/people/7/summary",
		"/api/v1/people/7/timeline",
		"/api/v1/people/7/files/search",
		"/api/v1/domains/search",
		"/api/v1/domains/example.com/summary",
		"/api/v1/domains/example.com/timeline",
		"/api/v1/domains/example.com/files/search",
		"/api/v1/relationships",
		"/api/v1/relationships/7/timeline",
		"/api/v1/search/coverage",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			assert := assert.New(t)
			gate := &recordingOperationGate{allow: false}
			called := false
			handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))

			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)

			assert.True(called, "read-only analytical POST should bypass the gate")
			assert.Equal(http.StatusNoContent, resp.Code, "status")
			begin, _ := gate.counts()
			assert.Equal(0, begin, "begin calls")
		})
	}
}

// TestReadOnlyPostRoutePatternsMatchExplorationRoutes pins the gate's
// read-only POST table to the registered analytical routes: every POST
// operation tagged "Exploration" (registerExploreRoute and the search
// coverage route) must be classified read-only, and the table must not
// carry stale entries for routes that no longer exist. The remote-image
// proxy is the one pinned non-Exploration entry — POST purely for the CSRF
// unsafe-method machinery, reading no archive state — and it must remain a
// registered POST route for its table entry to stay valid.
func TestReadOnlyPostRoutePatternsMatchExplorationRoutes(t *testing.T) {
	require := require.New(t)
	doc := OpenAPIDocument()
	remoteImage := doc.Paths[remoteImagePath]
	require.NotNil(remoteImage, "remote-image proxy route must exist")
	require.NotNil(remoteImage.Post, "remote-image proxy must be registered as POST")
	require.Nil(remoteImage.Get, "remote-image proxy must not be reachable via GET")

	expected := []string{remoteImagePath}
	for path, item := range doc.Paths {
		if item.Post != nil && slices.Contains(item.Post.Tags, "Exploration") {
			expected = append(expected, path)
		}
	}
	slices.Sort(expected)
	table := slices.Clone(readOnlyPostRoutePatterns)
	slices.Sort(table)
	assert.Equal(t, expected, table,
		"readOnlyPostRoutePatterns must match the POST routes tagged Exploration plus the "+
			"remote-image proxy; classify new analytical routes consciously in operation_gate.go")
}

// gateAnalyticsEngine backs the read-only analytical routes with minimal
// successful responses so gate-bypass tests can assert 200s end to end.
type gateAnalyticsEngine struct {
	*querytest.MockEngine
}

func (e *gateAnalyticsEngine) Explore(context.Context, query.ExploreRequest) (*query.ExploreResponse, error) {
	return &query.ExploreResponse{CacheRevision: "rev"}, nil
}

func (e *gateAnalyticsEngine) ExploreCoverage(context.Context, query.ExploreCoverageRequest, func([]int64) error) (*query.ExploreCoverageResult, error) {
	return &query.ExploreCoverageResult{}, nil
}

func (e *gateAnalyticsEngine) ExploreGroups(context.Context, query.ExploreGroupRequest) (*query.ExploreGroupResponse, error) {
	return &query.ExploreGroupResponse{}, nil
}

func (e *gateAnalyticsEngine) ExploreSelectionStats(context.Context, query.ExploreSelectionRequest) (*query.ExploreSelectionStats, error) {
	return &query.ExploreSelectionStats{}, nil
}

func (e *gateAnalyticsEngine) ExploreFiles(context.Context, query.ExploreFilesRequest) (*query.ExploreFilesResponse, error) {
	return &query.ExploreFilesResponse{}, nil
}

func (e *gateAnalyticsEngine) ExploreMatchCounts(context.Context, query.ExploreMatchCountsRequest) (*query.ExploreMatchCountsResponse, error) {
	return &query.ExploreMatchCountsResponse{}, nil
}

func (e *gateAnalyticsEngine) SearchPeople(context.Context, query.PersonSearchRequest) (*query.PersonSearchResponse, error) {
	return &query.PersonSearchResponse{}, nil
}

func (e *gateAnalyticsEngine) GetPerson(context.Context, int64, query.Context, []int64) (*query.PersonSummary, error) {
	return &query.PersonSummary{ID: 7}, nil
}

func (e *gateAnalyticsEngine) GetPersonSummary(context.Context, int64, query.ExploreRequest, []int64) (*query.PersonSearchResponse, error) {
	return &query.PersonSearchResponse{Rows: []query.PersonSummary{{ID: 7}}}, nil
}

func (e *gateAnalyticsEngine) SearchDomains(context.Context, query.DomainSearchRequest) (*query.DomainSearchResponse, error) {
	return &query.DomainSearchResponse{}, nil
}

func (e *gateAnalyticsEngine) GetDomain(context.Context, string, query.Context) (*query.DomainSummary, error) {
	return &query.DomainSummary{}, nil
}

func (e *gateAnalyticsEngine) GetDomainSummary(context.Context, string, query.ExploreRequest) (*query.DomainSearchResponse, error) {
	return &query.DomainSearchResponse{}, nil
}

func (e *gateAnalyticsEngine) Relationships(context.Context, query.RelationshipsRequest) (*query.RelationshipsResponse, error) {
	return &query.RelationshipsResponse{}, nil
}

func (e *gateAnalyticsEngine) RelationshipTimeline(context.Context, query.RelationshipTimelineRequest) (*query.RelationshipTimelineResponse, error) {
	return &query.RelationshipTimelineResponse{}, nil
}

func (e *gateAnalyticsEngine) ResolveCanonicalParticipant(_ context.Context, id int64) (int64, error) {
	return id, nil
}

func (e *gateAnalyticsEngine) SearchFiles(context.Context, query.FileSearchRequest) (*query.FileSearchResponse, error) {
	return &query.FileSearchResponse{}, nil
}

// gateFilesStore adds the file-metadata catalog capability the files search
// route requires so the gate-bypass test can exercise it end to end.
type gateFilesStore struct {
	*mockStore
}

func (s *gateFilesStore) GetFileMetadata(context.Context, int64) (*store.FileMetadata, error) {
	return &store.FileMetadata{}, nil
}

func (s *gateFilesStore) GetFileMetadataBatch(context.Context, []int64) (map[int64]store.FileMetadata, error) {
	return map[int64]store.FileMetadata{}, nil
}

// TestServerReadOnlyAnalyticalPostsBypassHeldOperationGate is the regression
// for read-only analytical POSTs queueing behind archive operations: while a
// long operation holds the gate, representative analytical endpoints must
// answer immediately instead of returning operation_in_progress, and a
// mutating POST must still be turned away naming the holder.
func TestServerReadOnlyAnalyticalPostsBypassHeldOperationGate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	oldLimit := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = oldLimit })

	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(context.Background(), "msgvault embeddings build")
	require.True(ok, "occupy gate")
	defer release()

	srv := NewServerWithOptions(ServerOptions{
		Config:        &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:         &gateFilesStore{mockStore: &mockStore{}},
		Engine:        &gateAnalyticsEngine{MockEngine: &querytest.MockEngine{}},
		Logger:        testLogger(),
		OperationGate: gate,
	})

	readOnly := []string{
		"/api/v1/explore",
		"/api/v1/relationships",
		"/api/v1/people/search",
		"/api/v1/files/search",
		"/api/v1/people/7/summary",
	}
	for _, path := range readOnly {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		assert.Equal(http.StatusOK, resp.Code, "%s must succeed while the gate is held: %s", path, resp.Body.String())
	}

	mutating := httptest.NewRequest(http.MethodPost, "/api/v1/deletions", strings.NewReader(`{}`))
	mutating.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, mutating)
	require.Equal(http.StatusServiceUnavailable, resp.Code, "mutating POST must still gate")
	var errResp ErrorResponse
	require.NoError(json.Unmarshal(resp.Body.Bytes(), &errResp), "decode error envelope")
	assert.Equal("operation_in_progress", errResp.Error, "error code")
	assert.Contains(errResp.Message, "msgvault embeddings build", "message names the holder")
}

func TestOperationGateMiddlewareNamesHolderWhenBusy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	oldLimit := operationGateWaitLimit
	operationGateWaitLimit = 20 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = oldLimit })

	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(context.Background(), "msgvault embeddings build")
	require.True(ok, "occupy gate")
	defer release()

	handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(`{"args":["sync","user@example.com"]}`))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	assert.Equal(http.StatusServiceUnavailable, resp.Code, "status")
	var errResp ErrorResponse
	require.NoError(json.Unmarshal(resp.Body.Bytes(), &errResp), "decode error envelope")
	assert.Equal("operation_in_progress", errResp.Error, "error code")
	assert.Contains(errResp.Message, "msgvault embeddings build", "message names the holder")
}

func TestOperationGateMiddlewareReportsShutdownWhenDraining(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	gate := NewSerialOperationGate()
	gate.StartDrain()

	handler := operationGateMiddleware(gate, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	assert.Equal(http.StatusServiceUnavailable, resp.Code, "status")
	var errResp ErrorResponse
	require.NoError(json.Unmarshal(resp.Body.Bytes(), &errResp), "decode error envelope")
	assert.Equal("server_busy", errResp.Error, "error code")
}

func TestSerialOperationGateHolderTracksLabel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	gate := NewSerialOperationGate()
	_, _, held := gate.Holder()
	assert.False(held, "idle gate has no holder")

	release, ok := gate.BeginLabeledWorkContext(context.Background(), "a scheduled sync")
	require.True(ok, "acquire gate")
	label, since, held := gate.Holder()
	assert.True(held, "held while acquired")
	assert.Equal("a scheduled sync", label, "holder label")
	assert.False(since.IsZero(), "holder since")

	release()
	_, _, held = gate.Holder()
	assert.False(held, "released gate has no holder")
}

func TestSerialOperationGateCountsRequestWaiters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(context.Background(), "a scheduled sync")
	require.True(ok, "occupy gate")
	assert.False(gate.HasRequestWaiters(), "no waiters yet")

	acquired := make(chan bool, 1)
	go func() {
		waiterRelease, waiterOK := gate.BeginRequestWorkContext(context.Background(), "msgvault sync")
		if waiterOK {
			waiterRelease()
		}
		acquired <- waiterOK
	}()

	require.Eventually(gate.HasRequestWaiters, time.Second, time.Millisecond,
		"queued request counts as waiter")

	release()
	select {
	case waiterOK := <-acquired:
		assert.True(waiterOK, "waiter acquires after release")
	case <-time.After(time.Second):
		require.FailNow("waiter did not acquire gate")
	}
	assert.False(gate.HasRequestWaiters(), "waiter count returns to zero")
}

func TestBeginLabeledOperationGateWorkCountsAsRequestWaiter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	gate := NewSerialOperationGate()
	srv := &Server{operationGate: gate}

	holderRelease, ok := gate.BeginLabeledWorkContext(context.Background(), "a scheduled sync")
	require.True(ok, "occupy gate")

	acquired := make(chan bool, 1)
	go func() {
		release, workOK := srv.beginLabeledOperationGateWork(context.Background(), "a search index build")
		if workOK {
			release()
		}
		acquired <- workOK
	}()

	require.Eventually(gate.HasRequestWaiters, time.Second, time.Millisecond,
		"in-handler gate work must count as a request waiter so scheduled jobs yield")

	holderRelease()
	select {
	case workOK := <-acquired:
		assert.True(workOK, "handler work acquires after release")
	case <-time.After(time.Second):
		require.FailNow("handler work did not acquire gate")
	}
}

func TestCLIRunEnvAllowedPermitsConfiguredAPIKeyEnv(t *testing.T) {
	assert := assert.New(t)
	srv := &Server{cfg: &config.Config{}}
	srv.cfg.Vector.Embeddings.APIKeyEnv = "MSGVAULT_EMBED_API_KEY"

	assert.True(srv.cliRunEnvAllowed("MSGVAULT_IMAP_PASSWORD"), "static allowlist entry")
	assert.True(srv.cliRunEnvAllowed("MSGVAULT_EMBED_API_KEY"), "configured api_key_env")
	assert.False(srv.cliRunEnvAllowed("PATH"), "arbitrary env stays rejected")

	unconfigured := &Server{cfg: &config.Config{}}
	assert.False(unconfigured.cliRunEnvAllowed("MSGVAULT_EMBED_API_KEY"),
		"key env rejected when not configured")
}

func healthResponseForServer(t *testing.T, srv *Server) HealthResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code, "health status")
	var body HealthResponse
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &body), "decode health body")
	return body
}

func newOperationHealthTestServer(gate OperationGate) *Server {
	return NewServerWithOptions(ServerOptions{
		Config:        &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Logger:        testLogger(),
		OperationGate: gate,
	})
}

func TestHealthReportsActiveOperation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	gate := NewSerialOperationGate()
	srv := newOperationHealthTestServer(gate)

	release, ok := gate.BeginLabeledWorkContext(context.Background(), "POST /api/v1/auth/token/alice@example.com")
	require.True(ok, "acquire gate")
	defer release()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(http.StatusOK, resp.Code, "health status")
	var body map[string]any
	require.NoError(json.Unmarshal(resp.Body.Bytes(), &body), "decode health body")
	operation, ok := body["operation"].(map[string]any)
	require.True(ok, "health must report the gate holder")
	assert.Equal(true, operation["busy"], "health must report busy state")
	assert.NotContains(operation, "label", "public health must not leak operation labels")
	assert.NotContains(operation, "started_at", "public health must not leak operation start times")
}

func TestAuthenticatedHealthReportsActiveOperationDetails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	gate := NewSerialOperationGate()
	srv := NewServerWithOptions(ServerOptions{
		Config:        &config.Config{Server: config.ServerConfig{APIPort: 8080, APIKey: "secret-key"}},
		Logger:        testLogger(),
		OperationGate: gate,
	})

	release, ok := gate.BeginLabeledWorkContext(context.Background(), "POST /api/v1/auth/token/alice@example.com")
	require.True(ok, "acquire gate")
	defer release()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("X-Api-Key", "secret-key")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(http.StatusOK, resp.Code, "health status")
	var body map[string]any
	require.NoError(json.Unmarshal(resp.Body.Bytes(), &body), "decode health body")
	operation, ok := body["operation"].(map[string]any)
	require.True(ok, "health must report the gate holder")
	assert.Equal(true, operation["busy"], "health must report busy state")
	assert.Equal("POST /api/v1/auth/token/alice@example.com", operation["label"])
	assert.Contains(operation, "started_at")
}

func TestHealthLabelsUnlabeledGateHolder(t *testing.T) {
	require := require.New(t)
	gate := NewSerialOperationGate()
	srv := newOperationHealthTestServer(gate)

	release, ok := gate.BeginWork()
	require.True(ok, "acquire gate")
	defer release()

	body := healthResponseForServer(t, srv)
	require.NotNil(body.Operation, "health must report the gate holder")
	assert.True(t, body.Operation.Busy, "health must report busy state")
	assert.Empty(t, body.Operation.Label, "public health must not include operation labels")
	assert.Nil(t, body.Operation.StartedAt, "public health must not include operation start times")
}

func TestHealthOmitsOperationWhenGateIdle(t *testing.T) {
	srv := newOperationHealthTestServer(NewSerialOperationGate())

	body := healthResponseForServer(t, srv)
	assert.Nil(t, body.Operation, "idle gate must not report an operation")
}

// TestOperationHealthPrefersActivityOverGateLabel verifies a request-scoped
// activity (which can carry live progress) wins over the gate holder's static
// label, and that health falls back to the gate label once the activity ends.
func TestOperationHealthPrefersActivityOverGateLabel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	gate := NewSerialOperationGate()
	srv := newOperationHealthTestServer(gate)

	release, ok := gate.BeginLabeledWorkContext(context.Background(), "a search index build")
	require.True(ok, "acquire gate")
	defer release()

	end := srv.beginActivity("building the search index")
	srv.setActivityLabel("building the search index (2/4 messages)")

	op := srv.operationHealth()
	require.NotNil(op, "activity must be reported")
	assert.Equal("building the search index (2/4 messages)", op.Label,
		"activity label must win over the gate label")

	end()
	op = srv.operationHealth()
	require.NotNil(op, "gate holder must still be reported after the activity ends")
	assert.Equal("a search index build", op.Label, "gate label after activity end")
}

// TestBeginActivityNestingAndIdempotentEnd verifies overlapping activities
// share one label (first begin wins, last end clears) and that calling an end
// func twice does not clear a newer activity.
func TestBeginActivityNestingAndIdempotentEnd(t *testing.T) {
	assert := assert.New(t)
	srv := newOperationHealthTestServer(nil)

	endFirst := srv.beginActivity("checking the search index")
	endSecond := srv.beginActivity("building the search index")

	label, _, active := srv.currentActivity()
	assert.True(active, "activity must be active while begun")
	assert.Equal("checking the search index", label, "first begin sets the label")

	endFirst()
	endFirst() // second call must be a no-op, not a double decrement
	_, _, active = srv.currentActivity()
	assert.True(active, "activity must stay active until the last end")

	endSecond()
	_, _, active = srv.currentActivity()
	assert.False(active, "last end must clear the activity")
}
