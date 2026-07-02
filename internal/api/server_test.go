package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

// syncBuffer is a concurrency-safe buffer for capturing slog output written
// from the logger goroutine while the test goroutine reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("syncBuffer write: %w", err)
	}
	return n, nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestLoggerMiddlewareLogsInProgressRequest verifies that a request which
// overruns the in-progress threshold emits a repeating WARN carrying the
// request id, and that the watcher goroutine does not fire for fast requests.
func TestLoggerMiddlewareLogsInProgressRequest(t *testing.T) {
	buf := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	release := make(chan struct{})
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Logger: logger,
		SQLQueryRunner: func(_ context.Context, _ string) (*query.QueryResult, error) {
			<-release // hold the request open past the in-progress threshold
			return &query.QueryResult{}, nil
		},
	})
	srv.inProgressThreshold = 20 * time.Millisecond
	srv.inProgressInterval = 20 * time.Millisecond

	req := httptest.NewRequest(http.MethodPost, queryEndpointPath,
		bytes.NewReader([]byte(`{"sql":"SELECT 1"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.Router().ServeHTTP(resp, req)
		close(done)
	}()

	require.Eventually(t, func() bool {
		return strings.Contains(buf.String(), "http request in progress")
	}, 2*time.Second, 10*time.Millisecond, "no in-progress WARN emitted")

	close(release)
	<-done

	inProgress := findJSONLogLine(t, buf.String(), "http request in progress")
	assert.Equal(t, "WARN", inProgress["level"])
	assert.NotEmpty(t, inProgress["request_id"], "in-progress line must carry request_id")
	assert.Equal(t, queryEndpointPath, inProgress["path"])
}

// findJSONLogLine returns the first JSON slog record whose msg matches.
func findJSONLogLine(t *testing.T, out, msg string) map[string]any {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == msg {
			return rec
		}
	}
	require.FailNowf(t, "log line not found", "msg=%q out=%s", msg, out)
	return nil
}

// testLogger returns a logger for tests that discards output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// mockScheduler implements SyncScheduler for tests.
type mockScheduler struct {
	scheduled  map[string]bool
	running    bool
	statuses   []AccountStatus
	triggerFn  func(email string) error
	addedAccts []string // emails added via AddAccount
}

func newMockScheduler() *mockScheduler {
	return &mockScheduler{
		scheduled: make(map[string]bool),
		running:   true,
	}
}

func (m *mockScheduler) IsScheduled(email string) bool {
	return m.scheduled[email]
}

func (m *mockScheduler) TriggerSync(email string) error {
	if m.triggerFn != nil {
		return m.triggerFn(email)
	}
	return nil
}

func (m *mockScheduler) AddAccount(email, schedule string) error {
	m.scheduled[email] = true
	m.addedAccts = append(m.addedAccts, email)
	return nil
}

func (m *mockScheduler) Status() []AccountStatus {
	return m.statuses
}

func (m *mockScheduler) IsRunning() bool {
	return m.running
}

// mockStore implements MessageStore for tests.
type mockStore struct {
	stats            *StoreStats
	messages         []APIMessage
	total            int64
	needsFTSBackfill bool
	backfillFTSFunc  func(func(done, total int64)) (int64, error)
	rebuildFTSFunc   func(func(done, total int64)) (int64, error)
	buildCacheFunc   func(context.Context, bool, func(CLICacheBuildEvent) error) error
	syncFunc         func(context.Context, CLISyncRequest, func(CLISyncEvent) error) error
	verifyFunc       func(context.Context, CLIVerifyRequest, func(CLIVerifyEvent) error) error
	repairFunc       func(context.Context, func(CLIRepairEncodingEvent) error) error
	runFunc          func(context.Context, CLIRunRequest, func(CLIRunEvent) error) error
	planCalendarFunc func(context.Context, CLIAddCalendarPlanRequest) (CLIAddCalendarPlanResponse, error)
	planEmbedsFunc   func(context.Context, CLIEmbeddingsPlanRequest) (CLIEmbeddingsPlanResponse, error)
	planDeleteFunc   func(context.Context, CLIDeleteStagedPlanRequest) (CLIDeleteStagedPlanResponse, error)
	planDedupFunc    func(context.Context, CLIDeduplicatePlanRequest) (CLIDeduplicatePlanResponse, error)
	saveManifestFunc func(context.Context, *deletion.Manifest) error

	// Call counts so tests can assert that bulk hydration paths use
	// GetMessagesSummariesByIDs (one round-trip) instead of looping
	// GetMessage (per-hit N+1).
	getMessageCalls          atomic.Int32
	getSummariesByIDsCalls   atomic.Int32
	getSummariesByIDsLastIDs []int64
	searchMessagesCalls      atomic.Int32
	searchMessagesQueryCalls atomic.Int32
	searchMessagesQueryLast  *search.Query

	sourcesByLookup    map[string][]*store.Source
	sourcesByLookupErr error
	collections        map[string]*store.CollectionWithSources
}

func (m *mockStore) GetStats() (*StoreStats, error) {
	if m.stats == nil {
		return &StoreStats{}, nil
	}
	return m.stats, nil
}

func (m *mockStore) ListMessages(offset, limit int) ([]APIMessage, int64, error) {
	return m.messages, m.total, nil
}

func (m *mockStore) GetMessage(id int64) (*APIMessage, error) {
	m.getMessageCalls.Add(1)
	for _, msg := range m.messages {
		if msg.ID == id {
			return &msg, nil
		}
	}
	return nil, store.ErrMessageNotFound
}

func (m *mockStore) GetMessagesSummariesByIDs(ids []int64) ([]APIMessage, error) {
	m.getSummariesByIDsCalls.Add(1)
	m.getSummariesByIDsLastIDs = append([]int64(nil), ids...)
	byID := make(map[int64]APIMessage, len(m.messages))
	for _, msg := range m.messages {
		byID[msg.ID] = msg
	}
	out := make([]APIMessage, 0, len(ids))
	for _, id := range ids {
		if msg, ok := byID[id]; ok {
			out = append(out, msg)
		}
	}
	return out, nil
}

func (m *mockStore) SearchMessages(query string, offset, limit int) ([]APIMessage, int64, error) {
	m.searchMessagesCalls.Add(1)
	return m.messages, m.total, nil
}

func (m *mockStore) SearchMessagesContext(_ context.Context, query string, offset, limit int) ([]APIMessage, int64, error) {
	return m.SearchMessages(query, offset, limit)
}

func (m *mockStore) SearchMessagesQueryContext(ctx context.Context, q *search.Query, offset, limit int) ([]APIMessage, int64, error) {
	return m.SearchMessagesQuery(q, offset, limit)
}

func (m *mockStore) SearchMessagesQuery(q *search.Query, offset, limit int) ([]APIMessage, int64, error) {
	m.searchMessagesQueryCalls.Add(1)
	if q != nil {
		cp := *q
		cp.AccountIDs = append([]int64(nil), q.AccountIDs...)
		cp.TextTerms = append([]string(nil), q.TextTerms...)
		m.searchMessagesQueryLast = &cp
	} else {
		m.searchMessagesQueryLast = nil
	}
	return m.messages, m.total, nil
}

func (m *mockStore) GetStatsForScope([]int64) (*store.Stats, error) {
	if m.stats == nil {
		return &store.Stats{}, nil
	}
	return m.stats, nil
}

func (m *mockStore) GetSourcesByIdentifierOrDisplayName(input string) ([]*store.Source, error) {
	if m.sourcesByLookupErr != nil {
		return nil, m.sourcesByLookupErr
	}
	if m.sourcesByLookup != nil {
		return m.sourcesByLookup[input], nil
	}
	return nil, nil
}

func (m *mockStore) GetSourcesByTypeAndAccount(string, string) ([]*store.Source, error) {
	return nil, nil
}

func (m *mockStore) GetCollectionByName(name string) (*store.CollectionWithSources, error) {
	if m.collections != nil {
		if coll, ok := m.collections[name]; ok {
			return coll, nil
		}
	}
	return nil, store.ErrCollectionNotFound
}

func (m *mockStore) ListCollections() ([]*store.CollectionWithSources, error) {
	return nil, nil
}

func (m *mockStore) CreateCollection(
	string,
	string,
	[]int64,
) (*store.Collection, error) {
	return &store.Collection{}, nil
}

func (m *mockStore) AddSourcesToCollection(string, []int64) error {
	return nil
}

func (m *mockStore) RemoveSourcesFromCollection(string, []int64) error {
	return nil
}

func (m *mockStore) DeleteCollection(string) error {
	return nil
}

func (m *mockStore) UpdateSourceDisplayName(int64, string) error {
	return nil
}

func (m *mockStore) ListSources(string) ([]*store.Source, error) {
	return nil, nil
}

func (m *mockStore) GetSourceByID(int64) (*store.Source, error) {
	return nil, store.ErrSourceNotFound
}

func (m *mockStore) ListAccountIdentities(int64) ([]store.AccountIdentity, error) {
	return nil, nil
}

func (m *mockStore) AddAccountIdentity(int64, string, string) error {
	return nil
}

func (m *mockStore) RemoveAccountIdentity(int64, string) (int64, error) {
	return 0, nil
}

func (m *mockStore) CountMessagesForSource(int64) (int64, error) {
	return 0, nil
}

func (m *mockStore) CountSourceDeletedMessages(...int64) (int64, error) {
	return 0, nil
}

func (m *mockStore) NeedsFTSBackfill() bool {
	return m.needsFTSBackfill
}

func (m *mockStore) BackfillFTS(progress func(done, total int64)) (int64, error) {
	if m.backfillFTSFunc != nil {
		return m.backfillFTSFunc(progress)
	}
	return 0, nil
}

func (m *mockStore) RebuildFTS(progress func(done, total int64)) (int64, error) {
	if m.rebuildFTSFunc != nil {
		return m.rebuildFTSFunc(progress)
	}
	return 0, nil
}

func (m *mockStore) BuildCLICache(
	ctx context.Context,
	fullRebuild bool,
	emit func(CLICacheBuildEvent) error,
) error {
	if m.buildCacheFunc != nil {
		return m.buildCacheFunc(ctx, fullRebuild, emit)
	}
	return nil
}

func (m *mockStore) RunCLISync(
	ctx context.Context,
	req CLISyncRequest,
	emit func(CLISyncEvent) error,
) error {
	if m.syncFunc != nil {
		return m.syncFunc(ctx, req, emit)
	}
	return nil
}

func (m *mockStore) RunCLIVerify(
	ctx context.Context,
	req CLIVerifyRequest,
	emit func(CLIVerifyEvent) error,
) error {
	if m.verifyFunc != nil {
		return m.verifyFunc(ctx, req, emit)
	}
	return nil
}

func (m *mockStore) RunCLIRepairEncoding(
	ctx context.Context,
	emit func(CLIRepairEncodingEvent) error,
) error {
	if m.repairFunc != nil {
		return m.repairFunc(ctx, emit)
	}
	return nil
}

func (m *mockStore) RunCLICommand(
	ctx context.Context,
	req CLIRunRequest,
	emit func(CLIRunEvent) error,
) error {
	if m.runFunc != nil {
		return m.runFunc(ctx, req, emit)
	}
	return nil
}

func (m *mockStore) PlanCLIAddCalendar(
	ctx context.Context,
	req CLIAddCalendarPlanRequest,
) (CLIAddCalendarPlanResponse, error) {
	if m.planCalendarFunc != nil {
		return m.planCalendarFunc(ctx, req)
	}
	return CLIAddCalendarPlanResponse{}, nil
}

func (m *mockStore) PlanCLIEmbeddings(
	ctx context.Context,
	req CLIEmbeddingsPlanRequest,
) (CLIEmbeddingsPlanResponse, error) {
	if m.planEmbedsFunc != nil {
		return m.planEmbedsFunc(ctx, req)
	}
	return CLIEmbeddingsPlanResponse{}, nil
}

func (m *mockStore) PlanCLIDeleteStaged(
	ctx context.Context,
	req CLIDeleteStagedPlanRequest,
) (CLIDeleteStagedPlanResponse, error) {
	if m.planDeleteFunc != nil {
		return m.planDeleteFunc(ctx, req)
	}
	return CLIDeleteStagedPlanResponse{}, nil
}

func (m *mockStore) PlanCLIDeduplicate(
	ctx context.Context,
	req CLIDeduplicatePlanRequest,
) (CLIDeduplicatePlanResponse, error) {
	if m.planDedupFunc != nil {
		return m.planDedupFunc(ctx, req)
	}
	return CLIDeduplicatePlanResponse{}, nil
}

func (m *mockStore) SaveCLIDeletionManifest(ctx context.Context, manifest *deletion.Manifest) error {
	if m.saveManifestFunc != nil {
		return m.saveManifestFunc(ctx, manifest)
	}
	return nil
}

func TestHealthEndpoint(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "GET /health status")

	var resp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.Equal(t, "ok", resp["status"], "health status")
}

func TestHealthEndpoint_HEAD(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	req := httptest.NewRequest(http.MethodHead, "/health", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "HEAD /health status")
}

func TestDaemonPingEndpoint(t *testing.T) {
	assert := assert.
		New(t)

	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:        cfg,
		Scheduler:     newMockScheduler(),
		Logger:        testLogger(),
		DaemonVersion: "v-test",
	})

	req := httptest.NewRequest(http.MethodGet, daemon.DefaultPingPath, nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)
	assert.Equal(http.StatusOK, w.Code, "daemon ping status")

	var info daemon.PingInfo
	require.NoError(t, json.NewDecoder(w.Body).Decode(&info), "decode daemon ping")
	assert.True(info.OK, "ping ok")
	assert.Equal("msgvault", info.Service, "service")
	assert.Equal("v-test", info.Version, "version")
	assert.Equal(os.Getpid(), info.PID, "pid")
}

func TestDaemonShutdownEndpointRequiresRuntimeToken(t *testing.T) {
	assert := assert.
		New(t)

	called := make(chan struct{}, 1)
	srv := NewServerWithOptions(ServerOptions{
		Config:        &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Scheduler:     newMockScheduler(),
		Logger:        testLogger(),
		ShutdownToken: "runtime-token",
		ShutdownFunc: func() {
			called <- struct{}{}
		},
	})

	missing := httptest.NewRequest(http.MethodPost, DaemonShutdownPath, nil)
	missingResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(missingResp, missing)
	assert.Equal(http.StatusUnauthorized, missingResp.Code, "missing token status")
	assert.Empty(called, "shutdown must not run without token")

	req := httptest.NewRequest(http.MethodPost, DaemonShutdownPath, nil)
	req.Header.Set(DaemonShutdownTokenHeader, "runtime-token")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	assert.Equal(http.StatusAccepted, w.Code, "valid token status")
	require.Eventually(t, func() bool {
		select {
		case <-called:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond, "shutdown callback")
}

func TestAuthMiddleware(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			APIPort: 8080,
			APIKey:  "secret-key",
		},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{"no auth", "", http.StatusUnauthorized},
		{"wrong key", "wrong-key", http.StatusUnauthorized},
		{"correct key", "secret-key", http.StatusServiceUnavailable}, // 503 because scheduler returns statuses but no store
		{"bearer prefix", "Bearer secret-key", http.StatusServiceUnavailable},
		{"x-api-key header", "secret-key", http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
			if tt.authHeader != "" {
				if tt.name == "x-api-key header" {
					req.Header.Set("X-Api-Key", tt.authHeader)
				} else {
					req.Header.Set("Authorization", tt.authHeader)
				}
			}
			w := httptest.NewRecorder()

			srv.Router().ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code, "status")
		})
	}
}

func TestAuthMiddlewareNoKeyConfigured(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			APIPort: 8080,
			APIKey:  "", // No key configured
		},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	// Should allow access without auth when no key is configured
	req := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "status when no API key configured")
}

func TestSchedulerStatusEndpoint(t *testing.T) {
	assert := assert.New(t)
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	sched.running = true
	sched.statuses = []AccountStatus{
		{
			Email:    "test@gmail.com",
			Running:  false,
			Schedule: "0 2 * * *",
			NextRun:  time.Now().Add(time.Hour),
		},
	}

	srv := NewServer(cfg, nil, sched, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/scheduler/status", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code, "status")

	var resp SchedulerStatusResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.True(resp.Running, "expected scheduler to be running")
	assert.Len(resp.Accounts, 1, "expected 1 account")
}

func TestSchedulerStatusNotRunning(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	sched.running = false

	srv := NewServer(cfg, nil, sched, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/scheduler/status", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	var resp SchedulerStatusResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assert.False(t, resp.Running, "expected scheduler to NOT be running")
}

func TestListAccountsEndpoint(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
		Accounts: []config.AccountSchedule{
			{Email: "user1@gmail.com", Schedule: "0 2 * * *", Enabled: true},
			{Email: "user2@gmail.com", Schedule: "0 3 * * *", Enabled: false},
		},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "status")

	var resp map[string][]AccountInfo
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	accounts := resp["accounts"]
	assert.Len(t, accounts, 2, "expected 2 accounts")
}

func TestNilStoreReturns503(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	endpoints := []string{
		"/api/v1/stats",
		"/api/v1/cli/stats",
		"/api/v1/messages",
		"/api/v1/messages/1",
		"/api/v1/search?q=test",
	}

	for _, path := range endpoints {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)

			assert.Equal(t, http.StatusServiceUnavailable, w.Code, "%s", path)
		})
	}
}

func TestNilSchedulerReturns503(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	srv := NewServer(cfg, nil, nil, testLogger())

	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/api/v1/accounts"},
		{"POST", "/api/v1/sync/test@gmail.com"},
		{"GET", "/api/v1/scheduler/status"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)

			assert.Equal(t, http.StatusServiceUnavailable, w.Code, "%s %s", ep.method, ep.path)
		})
	}
}

func TestSecurityValidation(t *testing.T) {
	tests := []struct {
		name      string
		cfg       config.ServerConfig
		wantError bool
	}{
		{"loopback no key", config.ServerConfig{BindAddr: "127.0.0.1"}, false},
		{"loopback 127.0.0.2 no key", config.ServerConfig{BindAddr: "127.0.0.2"}, false},
		{"loopback 127.255.255.254 no key", config.ServerConfig{BindAddr: "127.255.255.254"}, false},
		{"ipv6 loopback no key", config.ServerConfig{BindAddr: "::1"}, false},
		{"localhost no key", config.ServerConfig{BindAddr: "localhost"}, false},
		{"empty addr no key", config.ServerConfig{BindAddr: ""}, false},
		{"non-loopback with key", config.ServerConfig{BindAddr: "0.0.0.0", APIKey: "secret"}, false},
		{"non-loopback no key", config.ServerConfig{BindAddr: "0.0.0.0"}, true},
		{"non-loopback ipv6 no key", config.ServerConfig{BindAddr: "::"}, true},
		{"non-loopback insecure override", config.ServerConfig{BindAddr: "0.0.0.0", AllowInsecure: true}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.ValidateSecure()
			if tt.wantError {
				assert.Error(t, err, "ValidateSecure()")
			} else {
				assert.NoError(t, err, "ValidateSecure()")
			}
		})
	}
}

func TestCORSFromConfig(t *testing.T) {
	assert := assert.
		New(t)

	cfg := &config.Config{
		Server: config.ServerConfig{
			APIPort:     8080,
			CORSOrigins: []string{"http://localhost:3000", "http://example.com"},
		},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	// Request from allowed origin
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	assert.Equal("http://localhost:3000", w.Header().Get("Access-Control-Allow-Origin"),
		"expected CORS header for allowed origin")

	// Request from disallowed origin
	req2 := httptest.NewRequest(http.MethodGet, "/health", nil)
	req2.Header.Set("Origin", "http://evil.com")
	w2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(w2, req2)
	assert.Empty(w2.Header().Get("Access-Control-Allow-Origin"),
		"expected no CORS header for disallowed origin")

	// Preflight requests from allowed origins should advertise every API method.
	req3 := httptest.NewRequest(http.MethodOptions, "/api/v1/cli/collections/Team/sources", nil)
	req3.Header.Set("Origin", "http://localhost:3000")
	w3 := httptest.NewRecorder()
	srv.Router().ServeHTTP(w3, req3)
	assert.Equal(http.StatusNoContent, w3.Code, "preflight status")
	assert.Contains(w3.Header().Get("Access-Control-Allow-Methods"), http.MethodPatch,
		"expected PATCH in allowed methods")
}

func TestCORSDisabledByDefault(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"),
		"expected no CORS header when no origins configured")
}
