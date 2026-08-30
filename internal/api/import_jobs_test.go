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

	mu        sync.Mutex
	active    *store.SyncRun
	latest    *store.SyncRun
	runErr    error
	started   chan CLISyncRequest
	release   chan struct{}
	releaseMu sync.Once
}

func newImportJobTestStore() *importJobTestStore {
	return &importJobTestStore{
		mockStore: &mockStore{
			sourcesByLookup: map[string][]*store.Source{
				"archive@example.com": {{
					ID:         42,
					SourceType: "gmail",
					Identifier: "archive@example.com",
				}},
			},
		},
		started: make(chan CLISyncRequest, 1),
		release: make(chan struct{}),
	}
}

func (s *importJobTestStore) RunCLISync(
	ctx context.Context,
	req CLISyncRequest,
	_ func(CLISyncEvent) error,
) error {
	select {
	case s.started <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-s.release:
		return s.runErr
	case <-ctx.Done():
		return ctx.Err()
	}
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

func (s *importJobTestStore) GetLatestSync(sourceID int64) (*store.SyncRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest == nil || s.latest.SourceID != sourceID {
		return nil, store.ErrSyncRunNotFound
	}
	clone := *s.latest
	return &clone, nil
}

func (s *importJobTestStore) setActive(run *store.SyncRun) {
	s.mu.Lock()
	s.active = run
	s.mu.Unlock()
}

func (s *importJobTestStore) setLatest(run *store.SyncRun) {
	s.mu.Lock()
	s.latest = run
	s.mu.Unlock()
}

func (s *importJobTestStore) finish() {
	s.releaseMu.Do(func() { close(s.release) })
}

type importJobTestResponse struct {
	JobID      string                `json:"job_id"`
	Account    string                `json:"account"`
	Status     string                `json:"status"`
	Processed  int64                 `json:"processed"`
	Added      int64                 `json:"added"`
	Skipped    int64                 `json:"skipped"`
	Error      string                `json:"error"`
	CreatedAt  time.Time             `json:"created_at"`
	StartedAt  *time.Time            `json:"started_at"`
	FinishedAt *time.Time            `json:"finished_at"`
	Summary    *importJobTestSummary `json:"summary"`
}

type importJobTestSummary struct {
	Processed int64 `json:"processed"`
	Added     int64 `json:"added"`
	Updated   int64 `json:"updated"`
	Skipped   int64 `json:"skipped"`
	Errors    int64 `json:"errors"`
}

func submitImportJob(t *testing.T, srv *Server, body string, apiKey string) importJobTestResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", applicationJSONMediaType)
	if apiKey != "" {
		req.Header.Set("X-Api-Key", apiKey)
	}
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

func TestImportJobLifecycleQueuesBehindArchiveWorkAndReportsTypedProgress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newImportJobTestStore()
	t.Cleanup(st.finish)
	gate := NewSerialOperationGate()
	releaseGate, ok := gate.BeginLabeledWorkContext(t.Context(), "another archive operation")
	require.True(ok)

	srv := NewServerWithOptions(ServerOptions{
		Config:        &config.Config{},
		Store:         st,
		OperationGate: gate,
		Logger:        testLogger(),
	})

	created := submitImportJob(t, srv, `{
		"account":"archive@example.com",
		"after":"2024-01-01",
		"before":"2024-02-01",
		"limit":25,
		"query":"has:attachment",
		"noresume":true
	}`, "")
	assert.Equal("archive@example.com", created.Account)
	assert.Equal("queued", created.Status)
	assert.False(created.CreatedAt.IsZero())
	assert.Nil(created.StartedAt)
	assert.Nil(created.FinishedAt)

	select {
	case req := <-st.started:
		require.FailNowf("import started while gate held", "request=%+v", req)
	case <-time.After(30 * time.Millisecond):
	}

	releaseGate()
	var runReq CLISyncRequest
	require.Eventually(func() bool {
		select {
		case runReq = <-st.started:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	assert.True(runReq.Full)
	assert.Empty(runReq.Email)
	assert.Equal(int64(42), runReq.SourceID)
	assert.True(runReq.SourceIDSet)
	assert.Equal("2024-01-01", runReq.After)
	assert.Equal("2024-02-01", runReq.Before)
	assert.Equal(25, runReq.Limit)
	assert.Equal("has:attachment", runReq.Query)
	assert.True(runReq.NoResume)

	code, runningBeforeProgress, body := getImportJob(t, srv, created.JobID)
	require.Equal(http.StatusOK, code, body)
	require.Equal("running", runningBeforeProgress.Status)
	require.NotNil(runningBeforeProgress.StartedAt)
	startedAt := runningBeforeProgress.StartedAt.Truncate(time.Second)
	st.setActive(&store.SyncRun{
		ID:                100,
		SourceID:          42,
		StartedAt:         startedAt,
		Status:            store.SyncStatusRunning,
		MessagesProcessed: 12,
		MessagesAdded:     7,
		MessagesUpdated:   2,
		ErrorsCount:       1,
	})

	require.Eventually(func() bool {
		code, running, _ := getImportJob(t, srv, created.JobID)
		return code == http.StatusOK && running.Status == "running" &&
			running.Processed == 12 && running.Added == 7 && running.Skipped == 3
	}, time.Second, 10*time.Millisecond)

	finishedAt := time.Now().UTC().Add(time.Second)
	st.setLatest(&store.SyncRun{
		ID:                100,
		SourceID:          42,
		StartedAt:         startedAt,
		CompletedAt:       sql.NullTime{Time: finishedAt, Valid: true},
		Status:            store.SyncStatusCompleted,
		MessagesProcessed: 20,
		MessagesAdded:     11,
		MessagesUpdated:   3,
		ErrorsCount:       2,
	})
	st.finish()

	var done importJobTestResponse
	require.Eventually(func() bool {
		code, result, _ := getImportJob(t, srv, created.JobID)
		done = result
		return code == http.StatusOK && result.Status == "done"
	}, time.Second, 10*time.Millisecond)
	require.NotNil(done.Summary)
	assert.Equal(int64(20), done.Processed)
	assert.Equal(int64(11), done.Added)
	assert.Equal(int64(6), done.Skipped)
	assert.Equal(int64(20), done.Summary.Processed)
	assert.Equal(int64(11), done.Summary.Added)
	assert.Equal(int64(3), done.Summary.Updated)
	assert.Equal(int64(6), done.Summary.Skipped)
	assert.Equal(int64(2), done.Summary.Errors)
	assert.NotNil(done.StartedAt)
	assert.NotNil(done.FinishedAt)
}

func TestImportJobCanCompleteResumedBaselineRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newImportJobTestStore()
	baselineStartedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	st.setLatest(&store.SyncRun{
		ID:        99,
		SourceID:  42,
		StartedAt: baselineStartedAt,
		Status:    store.SyncStatusRunning,
	})
	t.Cleanup(st.finish)
	srv := NewServer(&config.Config{}, st, nil, testLogger())

	created := submitImportJob(t, srv, `{"account":"archive@example.com"}`, "")
	var runReq CLISyncRequest
	require.Eventually(func() bool {
		select {
		case runReq = <-st.started:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	assert.False(runReq.NoResume)

	finishedAt := time.Now().UTC()
	st.setLatest(&store.SyncRun{
		ID:                99,
		SourceID:          42,
		StartedAt:         baselineStartedAt,
		CompletedAt:       sql.NullTime{Time: finishedAt, Valid: true},
		Status:            store.SyncStatusCompleted,
		MessagesProcessed: 10,
		MessagesAdded:     7,
		MessagesUpdated:   1,
	})
	st.finish()

	var done importJobTestResponse
	require.Eventually(func() bool {
		code, result, _ := getImportJob(t, srv, created.JobID)
		done = result
		return code == http.StatusOK && result.Status == "done"
	}, time.Second, 10*time.Millisecond)
	assert.Equal(int64(10), done.Processed)
	assert.Equal(int64(7), done.Added)
	require.NotNil(done.Summary)
	assert.Equal(int64(1), done.Summary.Updated)
}

func TestImportJobFailureDoesNotExposeRunnerError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newImportJobTestStore()
	st.runErr = errors.New("oauth token secret-value was rejected")
	t.Cleanup(st.finish)
	srv := NewServer(&config.Config{}, st, nil, testLogger())

	created := submitImportJob(t, srv, `{"account":"archive@example.com"}`, "")
	require.Eventually(func() bool {
		select {
		case <-st.started:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	st.finish()

	var failed importJobTestResponse
	require.Eventually(func() bool {
		code, result, _ := getImportJob(t, srv, created.JobID)
		failed = result
		return code == http.StatusOK && result.Status == "failed"
	}, time.Second, 10*time.Millisecond)
	assert.Equal("import failed", failed.Error)
	assert.NotContains(failed.Error, "secret-value")
	assert.Nil(failed.Summary)
	assert.NotNil(failed.FinishedAt)
}

func TestImportJobOutlivesSubmitRequest(t *testing.T) {
	require := require.New(t)
	st := newImportJobTestStore()
	t.Cleanup(st.finish)
	gate := NewSerialOperationGate()
	releaseGate, ok := gate.BeginLabeledWorkContext(t.Context(), "another archive operation")
	require.True(ok)
	var releaseGateOnce sync.Once
	releaseHeldGate := func() { releaseGateOnce.Do(releaseGate) }
	t.Cleanup(releaseHeldGate)

	srv := NewServerWithOptions(ServerOptions{
		Config:        &config.Config{},
		Store:         st,
		OperationGate: gate,
		Logger:        testLogger(),
	})

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports",
		strings.NewReader(`{"account":"archive@example.com"}`)).WithContext(requestCtx)
	req.Header.Set("Content-Type", applicationJSONMediaType)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(http.StatusAccepted, resp.Code, resp.Body.String())

	var created importJobTestResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&created))
	cancelRequest()
	releaseHeldGate()

	require.Eventually(func() bool {
		select {
		case <-st.started:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond, "background import must not inherit request cancellation")

	finishedAt := time.Now().UTC()
	st.setLatest(&store.SyncRun{
		ID:          101,
		SourceID:    42,
		StartedAt:   finishedAt,
		CompletedAt: sql.NullTime{Time: finishedAt, Valid: true},
		Status:      store.SyncStatusCompleted,
	})
	st.finish()
	require.Eventually(func() bool {
		code, result, _ := getImportJob(t, srv, created.JobID)
		return code == http.StatusOK && result.Status == "done"
	}, time.Second, 10*time.Millisecond)
}

func TestImportJobDoesNotAdoptStaleLatestRun(t *testing.T) {
	st := newImportJobTestStore()
	oldStartedAt := time.Now().UTC().Add(-time.Hour)
	st.setLatest(&store.SyncRun{
		ID:                7,
		SourceID:          42,
		StartedAt:         oldStartedAt,
		CompletedAt:       sql.NullTime{Time: oldStartedAt.Add(time.Minute), Valid: true},
		Status:            store.SyncStatusCompleted,
		MessagesProcessed: 900,
		MessagesAdded:     800,
	})
	t.Cleanup(st.finish)
	srv := NewServer(&config.Config{}, st, nil, testLogger())

	created := submitImportJob(t, srv, `{"account":"archive@example.com"}`, "")
	require.Eventually(t, func() bool {
		select {
		case <-st.started:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	st.finish()

	require.Eventually(t, func() bool {
		code, result, _ := getImportJob(t, srv, created.JobID)
		return code == http.StatusOK && result.Status == "failed" &&
			result.Processed == 0 && result.Added == 0 && result.Summary == nil
	}, time.Second, 10*time.Millisecond)
}

func TestImportJobShutdownCancelsRunningJob(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newImportJobTestStore()
	t.Cleanup(st.finish)
	srv := NewServer(&config.Config{}, st, nil, testLogger())

	created := submitImportJob(t, srv, `{"account":"archive@example.com"}`, "")
	require.Eventually(func() bool {
		select {
		case <-st.started:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(srv.Shutdown(shutdownCtx))

	code, failed, body := getImportJob(t, srv, created.JobID)
	require.Equal(http.StatusOK, code, body)
	assert.Equal("failed", failed.Status)
	assert.Equal("import failed", failed.Error)
	assert.NotNil(failed.FinishedAt)
}

func TestImportJobAcceptsAccountDisplayName(t *testing.T) {
	st := newImportJobTestStore()
	st.sourcesByLookup["Work Archive"] = []*store.Source{{
		ID:          54,
		SourceType:  "imap",
		Identifier:  "work@example.com",
		DisplayName: sql.NullString{String: "Work Archive", Valid: true},
	}}
	t.Cleanup(st.finish)
	srv := NewServer(&config.Config{}, st, nil, testLogger())

	created := submitImportJob(t, srv, `{"account":"Work Archive"}`, "")
	assert.Equal(t, "work@example.com", created.Account)

	var runReq CLISyncRequest
	require.Eventually(t, func() bool {
		select {
		case runReq = <-st.started:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, int64(54), runReq.SourceID)
}

func TestImportJobsRequireAPIAuthentication(t *testing.T) {
	const apiKey = "import-test-api-key"
	st := newImportJobTestStore()
	t.Cleanup(st.finish)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: apiKey}}, st, nil, testLogger())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports",
		strings.NewReader(`{"account":"archive@example.com"}`))
	req.Header.Set("Content-Type", applicationJSONMediaType)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	assert.Equal(t, http.StatusUnauthorized, resp.Code, resp.Body.String())

	created := submitImportJob(t, srv, `{"account":"archive@example.com"}`, apiKey)
	code, _, body := getImportJob(t, srv, created.JobID)
	assert.Equal(t, http.StatusUnauthorized, code, body)
}

func TestImportJobRequestValidation(t *testing.T) {
	st := newImportJobTestStore()
	t.Cleanup(st.finish)
	srv := NewServer(&config.Config{}, st, nil, testLogger())

	tests := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
	}{
		{name: "content type required", body: `{}`, wantStatus: http.StatusUnsupportedMediaType},
		{name: "malformed JSON", body: `{`, contentType: applicationJSONMediaType, wantStatus: http.StatusBadRequest},
		{name: "account required", body: `{}`, contentType: applicationJSONMediaType, wantStatus: http.StatusUnprocessableEntity},
		{name: "after is a date", body: `{"account":"archive@example.com","after":"yesterday"}`, contentType: applicationJSONMediaType, wantStatus: http.StatusUnprocessableEntity},
		{name: "before is a date", body: `{"account":"archive@example.com","before":"tomorrow"}`, contentType: applicationJSONMediaType, wantStatus: http.StatusUnprocessableEntity},
		{name: "window is ordered", body: `{"account":"archive@example.com","after":"2024-02-01","before":"2024-01-01"}`, contentType: applicationJSONMediaType, wantStatus: http.StatusUnprocessableEntity},
		{name: "limit is nonnegative", body: `{"account":"archive@example.com","limit":-1}`, contentType: applicationJSONMediaType, wantStatus: http.StatusUnprocessableEntity},
		{name: "unknown account", body: `{"account":"missing@example.com"}`, contentType: applicationJSONMediaType, wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/imports", strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)
			assert.Equal(t, tt.wantStatus, resp.Code, resp.Body.String())
		})
	}
}

func TestImportJobRejectsAmbiguousAndUnsyncableSources(t *testing.T) {
	st := newImportJobTestStore()
	st.sourcesByLookup["ambiguous@example.com"] = []*store.Source{
		{ID: 51, SourceType: "gmail", Identifier: "ambiguous@example.com"},
		{ID: 52, SourceType: "imap", Identifier: "ambiguous@example.com"},
	}
	st.sourcesByLookup["calendar@example.com"] = []*store.Source{
		{ID: 53, SourceType: "gcal", Identifier: "calendar@example.com"},
	}
	t.Cleanup(st.finish)
	srv := NewServer(&config.Config{}, st, nil, testLogger())

	tests := []struct {
		account    string
		wantStatus int
	}{
		{account: "ambiguous@example.com", wantStatus: http.StatusConflict},
		{account: "calendar@example.com", wantStatus: http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.account, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/imports",
				strings.NewReader(fmt.Sprintf(`{"account":%q}`, tt.account)))
			req.Header.Set("Content-Type", applicationJSONMediaType)
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)
			assert.Equal(t, tt.wantStatus, resp.Code, resp.Body.String())
		})
	}
}

func TestImportJobAdmissionIsBounded(t *testing.T) {
	st := newImportJobTestStore()
	gate := NewSerialOperationGate()
	releaseGate, ok := gate.BeginLabeledWorkContext(t.Context(), "another archive operation")
	require.True(t, ok)

	srv := NewServerWithOptions(ServerOptions{
		Config:        &config.Config{},
		Store:         st,
		OperationGate: gate,
		Logger:        testLogger(),
	})
	t.Cleanup(func() {
		st.finish()
		releaseGate()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, srv.Shutdown(shutdownCtx))
	})

	for i := range 16 {
		account := fmt.Sprintf("archive-%02d@example.com", i)
		st.sourcesByLookup[account] = []*store.Source{{
			ID: int64(100 + i), SourceType: "gmail", Identifier: account,
		}}
		submitImportJob(t, srv, fmt.Sprintf(`{"account":%q}`, account), "")
	}

	overflowAccount := "archive-overflow@example.com"
	st.sourcesByLookup[overflowAccount] = []*store.Source{{
		ID: 999, SourceType: "gmail", Identifier: overflowAccount,
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports",
		strings.NewReader(fmt.Sprintf(`{"account":%q}`, overflowAccount)))
	req.Header.Set("Content-Type", applicationJSONMediaType)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	assert.Equal(t, http.StatusTooManyRequests, resp.Code, resp.Body.String())

	duplicate := httptest.NewRequest(http.MethodPost, "/api/v1/imports",
		strings.NewReader(`{"account":"archive-00@example.com"}`))
	duplicate.Header.Set("Content-Type", applicationJSONMediaType)
	duplicateResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(duplicateResp, duplicate)
	assert.Equal(t, http.StatusConflict, duplicateResp.Code, duplicateResp.Body.String())
}

func TestGetImportJobReturnsNotFoundForUnknownID(t *testing.T) {
	srv := NewServer(&config.Config{}, newImportJobTestStore(), nil, testLogger())
	code, _, body := getImportJob(t, srv, "missing")
	assert.Equal(t, http.StatusNotFound, code, body)
}
