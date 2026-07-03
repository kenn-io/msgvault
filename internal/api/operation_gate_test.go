package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
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
			handler := operationGateMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

func TestOperationGateMiddlewareGatesMutatingMethods(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			gate := &recordingOperationGate{allow: true}
			handler := operationGateMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	handler := operationGateMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	handler := operationGateMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	handler := operationGateMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	handler := operationGateMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	handler := operationGateMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	handler := operationGateMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
			handler := operationGateMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

func TestOperationGateMiddlewareSkipsReadOnlyPaths(t *testing.T) {
	paths := []string{
		"/api/v1/cli/verify",
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
			handler := operationGateMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	handler := operationGateMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	handler := operationGateMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
