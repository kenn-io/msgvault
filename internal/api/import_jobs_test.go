package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

type importJobTestStore struct {
	*mockStore

	mu         sync.Mutex
	sources    map[int64]*store.Source
	active     *store.SyncRun
	operations map[string]*store.SyncOperation
	entered    chan CLISyncRequest
	started    chan CLISyncRequest
	allowStart <-chan struct{}
	release    chan struct{}
	releaseMu  sync.Once
	runErr     error
	createErr  error
}

func newImportJobTestStore() *importJobTestStore {
	source := &store.Source{ID: 42, SourceType: "gmail", Identifier: "archive@example.com"}
	return &importJobTestStore{
		mockStore: &mockStore{sourcesByLookup: map[string][]*store.Source{
			"archive@example.com": {source},
		}},
		sources: map[int64]*store.Source{42: source}, operations: make(map[string]*store.SyncOperation),
		entered: make(chan CLISyncRequest, 1), started: make(chan CLISyncRequest, 1), release: make(chan struct{}),
	}
}

func (s *importJobTestStore) RunCLISync(ctx context.Context, req CLISyncRequest, _ func(CLISyncEvent) error) error {
	s.entered <- req
	if s.allowStart != nil {
		select {
		case <-s.allowStart:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	startedAt := time.Now().UTC()
	s.mu.Lock()
	op := s.operations[req.OperationID]
	op.Status = "running"
	op.StartedAt = sql.NullTime{Time: startedAt, Valid: true}
	op.Runs = []*store.SyncRun{{ID: 100, SourceID: req.SourceID, StartedAt: startedAt, Status: store.SyncStatusRunning}}
	s.mu.Unlock()
	s.started <- req
	select {
	case <-s.release:
	case <-ctx.Done():
		s.runErr = ctx.Err()
	}
	s.mu.Lock()
	op = s.operations[req.OperationID]
	finishedAt := time.Now().UTC()
	op.FinishedAt = sql.NullTime{Time: finishedAt, Valid: true}
	if s.runErr != nil {
		op.Status = "failed"
	} else {
		op.Status = "done"
		for _, run := range op.Runs {
			run.Status = store.SyncStatusCompleted
			run.CompletedAt = sql.NullTime{Time: finishedAt, Valid: true}
		}
	}
	s.mu.Unlock()
	return s.runErr
}

func (s *importJobTestStore) CreateSyncOperation(sourceID int64, id string) (*store.SyncOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return nil, s.createErr
	}
	if s.active != nil && s.active.SourceID == sourceID {
		return nil, store.ErrSyncAlreadyActive
	}
	createdAt := time.Now().UTC()
	op := &store.SyncOperation{
		ID: id, SourceID: sourceID, Status: "pending", CreatedAt: createdAt,
	}
	s.operations[id] = op
	clone := *op
	return &clone, nil
}

func TestImportJobCreationDoesNotWaitForWorkerStartup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newImportJobTestStore()
	allowStart := make(chan struct{})
	st.allowStart = allowStart
	t.Cleanup(func() {
		close(allowStart)
		st.finish()
	})
	srv := NewServer(&config.Config{}, st, nil, testLogger())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports", strings.NewReader(`{"account":"archive@example.com"}`))
	req.Header.Set("Content-Type", applicationJSONMediaType)
	resp := httptest.NewRecorder()
	responseDone := make(chan struct{})
	go func() {
		srv.Router().ServeHTTP(resp, req)
		close(responseDone)
	}()

	<-st.entered
	select {
	case <-responseDone:
		assert.Equal(http.StatusAccepted, resp.Code, resp.Body.String())
		var result importJobTestResponse
		require.NoError(json.NewDecoder(resp.Body).Decode(&result))
		assert.Equal("pending", result.Status)
		assert.Nil(result.StartedAt)
	case <-time.After(time.Second):
		require.Fail("POST /imports waited for worker startup")
	}
}

func (s *importJobTestStore) GetSourceByID(id int64) (*store.Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	source := s.sources[id]
	if source == nil {
		return nil, store.ErrSourceNotFound
	}
	clone := *source
	return &clone, nil
}

func (s *importJobTestStore) GetActiveSync(sourceID int64) (*store.SyncRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil || s.active.SourceID != sourceID {
		return nil, store.ErrSyncRunNotFound
	}
	clone := *s.active
	return &clone, nil
}

func (s *importJobTestStore) GetSyncOperation(id string) (*store.SyncOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.operations[id]
	if op == nil {
		return nil, store.ErrSyncRunNotFound
	}
	clone := *op
	clone.Runs = make([]*store.SyncRun, len(op.Runs))
	for i, run := range op.Runs {
		runClone := *run
		clone.Runs[i] = &runClone
	}
	return &clone, nil
}

func (s *importJobTestStore) finish() { s.releaseMu.Do(func() { close(s.release) }) }

type importJobTestResponse struct {
	JobID      string            `json:"job_id"`
	Account    string            `json:"account"`
	Status     string            `json:"status"`
	Processed  int64             `json:"processed"`
	Added      int64             `json:"added"`
	Skipped    int64             `json:"skipped"`
	Error      string            `json:"error"`
	CreatedAt  time.Time         `json:"created_at"`
	StartedAt  *time.Time        `json:"started_at"`
	FinishedAt *time.Time        `json:"finished_at"`
	Summary    *ImportJobSummary `json:"summary"`
}

func submitImportJob(t *testing.T, srv *Server, body string) importJobTestResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", applicationJSONMediaType)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(t, http.StatusAccepted, resp.Code, resp.Body.String())
	var result importJobTestResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.NotEmpty(t, result.JobID)
	return result
}

func getImportJob(t *testing.T, srv *Server, jobID string) (int, importJobTestResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/imports/"+jobID, nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	var result importJobTestResponse
	if resp.Code == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	}
	return resp.Code, result, resp.Body.String()
}

func TestImportJobUsesDurableSyncOperationForProgressAndSummary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newImportJobTestStore()
	t.Cleanup(st.finish)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, OperationGate: NewSerialOperationGate(), Logger: testLogger()})

	created := submitImportJob(t, srv, `{"account":"archive@example.com","after":"2024-01-01","limit":25}`)
	assert.Equal("pending", created.Status)
	assert.False(created.CreatedAt.IsZero())
	assert.Nil(created.StartedAt)
	req := <-st.started
	assert.Equal(created.JobID, req.OperationID)
	assert.Equal("2024-01-01", req.After)
	assert.Equal(25, req.Limit)

	st.mu.Lock()
	op := st.operations[created.JobID]
	op.Runs[0].MessagesProcessed = 12
	op.Runs[0].MessagesAdded = 7
	op.Runs[0].MessagesUpdated = 2
	op.Runs = append(op.Runs, &store.SyncRun{
		ID: 101, SourceID: 42, StartedAt: time.Now().UTC(), Status: store.SyncStatusRunning,
		MessagesProcessed: 5, MessagesAdded: 2, MessagesUpdated: 1, ErrorsCount: 1,
	})
	st.mu.Unlock()

	code, running, body := getImportJob(t, srv, created.JobID)
	require.Equal(http.StatusOK, code, body)
	assert.Equal("running", running.Status)
	assert.Equal(int64(17), running.Processed)
	assert.Equal(int64(9), running.Added)
	assert.Equal(int64(5), running.Skipped)

	st.finish()
	require.Eventually(func() bool {
		_, result, _ := getImportJob(t, srv, created.JobID)
		return result.Status == "done" && result.Summary != nil
	}, time.Second, 10*time.Millisecond)
}

func TestImportJobHoldsIdleWorkLeaseUntilWorkerFinishes(t *testing.T) {
	st := newImportJobTestStore()
	t.Cleanup(st.finish)
	tracker := NewIdleTracker(time.Hour, func() {})
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{}, Store: st, IdleTracker: tracker, Logger: testLogger(),
	})

	submitImportJob(t, srv, `{"account":"archive@example.com"}`)
	<-st.started
	tracker.mu.Lock()
	activeWork := tracker.activeWork
	tracker.mu.Unlock()
	assert.Equal(t, 1, activeWork)

	st.finish()
	require.Eventually(t, func() bool {
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		return tracker.activeWork == 0
	}, time.Second, 10*time.Millisecond)
}

func TestImportJobRejectsAccountWithActiveSync(t *testing.T) {
	st := newImportJobTestStore()
	st.active = &store.SyncRun{ID: 7, SourceID: 42, Status: store.SyncStatusRunning}
	t.Cleanup(st.finish)
	srv := NewServer(&config.Config{}, st, nil, testLogger())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports", strings.NewReader(`{"account":"archive@example.com"}`))
	req.Header.Set("Content-Type", applicationJSONMediaType)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	assert.Equal(t, http.StatusConflict, resp.Code, resp.Body.String())
	assert.Empty(t, st.started)
}

func TestImportJobReturnsConflictWhenSourceReservationFails(t *testing.T) {
	st := newImportJobTestStore()
	st.createErr = store.ErrSyncAlreadyActive
	t.Cleanup(st.finish)
	srv := NewServer(&config.Config{}, st, nil, testLogger())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports", strings.NewReader(`{"account":"archive@example.com"}`))
	req.Header.Set("Content-Type", applicationJSONMediaType)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	assert.Equal(t, http.StatusConflict, resp.Code, resp.Body.String())
	assert.Empty(t, st.operations)
	assert.Empty(t, st.entered)
}

func TestImportJobRejectsQueryForIMAPSource(t *testing.T) {
	st := newImportJobTestStore()
	st.sources[42].SourceType = "imap"
	srv := NewServer(&config.Config{}, st, nil, testLogger())
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/imports",
		strings.NewReader(`{"account":"archive@example.com","query":"from:alice@example.com"}`),
	)
	req.Header.Set("Content-Type", applicationJSONMediaType)
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	assert.Equal(t, http.StatusUnprocessableEntity, resp.Code, resp.Body.String())
	assert.Empty(t, st.operations)
	assert.Empty(t, st.entered)
}

func TestImportJobTreatsLegacyEmptySourceTypeAsGmail(t *testing.T) {
	st := newImportJobTestStore()
	st.sources[42].SourceType = ""
	t.Cleanup(st.finish)
	srv := NewServer(&config.Config{}, st, nil, testLogger())

	created := submitImportJob(t, srv, `{"account":"archive@example.com"}`)
	assert.Equal(t, "pending", created.Status)
	request := <-st.started
	assert.Equal(t, int64(42), request.SourceID)
}

func TestImportJobCreationHasNoOrdinaryRequestDeadline(t *testing.T) {
	srv := NewServer(&config.Config{}, nil, nil, testLogger())
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	_, bounded := srv.requestTimeoutForPath(importJobsEndpointPath)
	assert.False(t, bounded)
}

func TestImportJobFailureIsSanitized(t *testing.T) {
	st := newImportJobTestStore()
	st.runErr = errors.New("oauth secret-value rejected")
	t.Cleanup(st.finish)
	srv := NewServer(&config.Config{}, st, nil, testLogger())
	created := submitImportJob(t, srv, `{"account":"archive@example.com"}`)
	st.finish()
	require.Eventually(t, func() bool {
		_, result, _ := getImportJob(t, srv, created.JobID)
		return result.Status == "failed"
	}, time.Second, 10*time.Millisecond)
	_, failed, _ := getImportJob(t, srv, created.JobID)
	assert.Equal(t, "import failed", failed.Error)
	assert.NotContains(t, failed.Error, "secret-value")
}

func TestImportJobReturnsBusyInsteadOfBuildingASecondQueue(t *testing.T) {
	st := newImportJobTestStore()
	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "another archive operation")
	require.True(t, ok)
	defer release()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, OperationGate: gate, Logger: testLogger()})
	oldLimit := operationGateWaitLimit
	operationGateWaitLimit = 10 * time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = oldLimit })

	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports", strings.NewReader(`{"account":"archive@example.com"}`))
	req.Header.Set("Content-Type", applicationJSONMediaType)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	assert.Equal(t, http.StatusServiceUnavailable, resp.Code, resp.Body.String())
}

func TestImportJobRequestValidation(t *testing.T) {
	st := newImportJobTestStore()
	srv := NewServer(&config.Config{}, st, nil, testLogger())
	tests := []struct {
		body, contentType string
		want              int
	}{
		{`{}`, "", http.StatusUnsupportedMediaType},
		{`{`, applicationJSONMediaType, http.StatusBadRequest},
		{`{}`, applicationJSONMediaType, http.StatusUnprocessableEntity},
		{`{"account":"archive@example.com","after":"yesterday"}`, applicationJSONMediaType, http.StatusUnprocessableEntity},
		{`{"account":"archive@example.com","after":"2024-02-01","before":"2024-01-01"}`, applicationJSONMediaType, http.StatusUnprocessableEntity},
		{`{"account":"missing@example.com"}`, applicationJSONMediaType, http.StatusNotFound},
	}
	for i, tt := range tests {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/imports", strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)
			assert.Equal(t, tt.want, resp.Code, resp.Body.String())
		})
	}
}

func TestImportJobsRequireAuthentication(t *testing.T) {
	st := newImportJobTestStore()
	t.Cleanup(st.finish)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "key"}}, st, nil, testLogger())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports", strings.NewReader(`{"account":"archive@example.com"}`))
	req.Header.Set("Content-Type", applicationJSONMediaType)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	assert.Equal(t, http.StatusUnauthorized, resp.Code)
}

func TestGetImportJobReturnsNotFoundForUnknownID(t *testing.T) {
	srv := NewServer(&config.Config{}, newImportJobTestStore(), nil, testLogger())
	code, _, body := getImportJob(t, srv, "missing")
	assert.Equal(t, http.StatusNotFound, code, body)
}
