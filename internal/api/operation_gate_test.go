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
