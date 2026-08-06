package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonauth"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

const cliTimeoutTestAPIKey = "cli-timeout-test-key"

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
	require := require.New(t)
	assert := assert.New(t)
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

	require.Eventually(func() bool {
		return strings.Contains(buf.String(), "http request in progress")
	}, 2*time.Second, 10*time.Millisecond, "no in-progress WARN emitted")

	close(release)
	<-done

	inProgress := findJSONLogLine(t, buf.String(), "http request in progress")
	assert.Equal("WARN", inProgress["level"])
	assert.NotEmpty(inProgress["request_id"], "in-progress line must carry request_id")
	assert.Equal(queryEndpointPath, inProgress["path"])
}

func TestPprofEndpointLoopbackOnly(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)

	cases := []struct {
		name       string
		remoteAddr string
		wantStatus int
	}{
		{"loopback allowed", "127.0.0.1:54321", http.StatusOK},
		{"ipv6 loopback allowed", "[::1]:54321", http.StatusOK},
		{"remote blocked", "8.8.8.8:54321", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
			req.RemoteAddr = tc.remoteAddr
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)
			assert.Equal(t, tc.wantStatus, resp.Code, "body: %s", resp.Body.String())
		})
	}
}

// TestPprofEndpointRequiresAuthWhenKeyConfigured covers the same-host reverse
// proxy case: when an API key is configured, unauthenticated traffic that
// arrives as loopback (e.g. forwarded by a local TLS terminator) must not read
// profiles; only a request carrying the valid key is served.
func TestPprofEndpointRequiresAuthWhenKeyConfigured(t *testing.T) {
	const key = "secret-key"
	srv := NewServer(
		&config.Config{Server: config.ServerConfig{APIKey: key}},
		nil, nil, testLogger(),
	)

	cases := []struct {
		name       string
		remoteAddr string
		reqKey     string
		wantStatus int
	}{
		{"loopback valid key allowed", "127.0.0.1:54321", key, http.StatusOK},
		{"loopback missing key blocked", "127.0.0.1:54321", "", http.StatusNotFound},
		{"loopback bad key blocked", "127.0.0.1:54321", "wrong", http.StatusNotFound},
		{"remote valid key blocked", "8.8.8.8:54321", key, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.reqKey != "" {
				req.Header.Set("X-Api-Key", tc.reqKey)
			}
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)
			assert.Equal(t, tc.wantStatus, resp.Code, "body: %s", resp.Body.String())
		})
	}
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
	scheduled     map[string]bool
	running       bool
	statuses      []AccountStatus
	jobStatuses   []JobStatus     // generic (non-account) job statuses, see JobStatus()
	scheduledJobs map[string]bool // generic job names recognized by IsJobScheduled
	triggeredJobs []string        // generic job names passed to TriggerJob
	triggerJobFn  func(name string) error
	startedJobs   []string // generic job names passed to StartJob
	startJobFn    func(name string) error
	triggerFn     func(email string) error
	addedAccts    []string // emails added via AddAccount
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

func (m *mockScheduler) JobStatus() []JobStatus {
	return m.jobStatuses
}

func (m *mockScheduler) IsJobScheduled(name string) bool {
	return m.scheduledJobs[name]
}

func (m *mockScheduler) TriggerJob(name string) error {
	m.triggeredJobs = append(m.triggeredJobs, name)
	if m.triggerJobFn != nil {
		return m.triggerJobFn(name)
	}
	return nil
}

func (m *mockScheduler) StartJob(name string) error {
	m.startedJobs = append(m.startedJobs, name)
	if m.startJobFn != nil {
		return m.startJobFn(name)
	}
	return nil
}

// mockStore implements MessageStore for tests.
type mockStore struct {
	stats            *StoreStats
	messages         []APIMessage
	total            int64
	needsFTSBackfill bool
	// needsFTSBackfillQuick is the cheap tail-check answer; independent of
	// the full probe so tests can exercise their divergence.
	needsFTSBackfillQuick bool
	// needsFTSBackfillFunc overrides the needsFTSBackfill field when set, so
	// tests can block inside the probe or vary its answer per call.
	needsFTSBackfillFunc func() bool
	backfillFTSFunc      func(func(done, total int64)) (int64, error)
	rebuildFTSFunc       func(func(done, total int64)) (int64, error)
	buildCacheFunc       func(context.Context, bool, func(CLICacheBuildEvent) error) error
	syncFunc             func(context.Context, CLISyncRequest, func(CLISyncEvent) error) error
	verifyFunc           func(context.Context, CLIVerifyRequest, func(CLIVerifyEvent) error) error
	repairFunc           func(context.Context, func(CLIRepairEncodingEvent) error) error
	runFunc              func(context.Context, CLIRunRequest, func(CLIRunEvent) error) error
	planCalendarFunc     func(context.Context, CLIAddCalendarPlanRequest) (CLIAddCalendarPlanResponse, error)
	planEmbedsFunc       func(context.Context, CLIEmbeddingsPlanRequest) (CLIEmbeddingsPlanResponse, error)
	planDeleteFunc       func(context.Context, CLIDeleteStagedPlanRequest) (CLIDeleteStagedPlanResponse, error)
	planDedupFunc        func(context.Context, CLIDeduplicatePlanRequest) (CLIDeduplicatePlanResponse, error)
	saveManifestFunc     func(context.Context, *deletion.Manifest) error

	// Error injection for the context-aware read paths, used to verify
	// handlers map context deadline/cancellation to a structured 503.
	statsErr              error
	getStatsFunc          func()
	getStatsContextFunc   func(context.Context) error
	getScopedStatsFunc    func([]int64)
	getScopedStatsCtxFunc func(context.Context, []int64) error
	summariesErr          error

	// Call counts so tests can assert that bulk hydration paths use
	// GetMessagesSummariesByIDs (one round-trip) instead of looping
	// GetMessage (per-hit N+1).
	getMessageCalls                atomic.Int32
	getSummariesByIDsCalls         atomic.Int32
	getSummariesByIDsLastIDs       []int64
	searchMessagesCalls            atomic.Int32
	searchMessagesQueryCalls       atomic.Int32
	searchMessagesQueryLast        *search.Query
	searchMessagesQueryFunc        func(*search.Query, int, int) ([]APIMessage, int64, error)
	searchMessagesQueryLimits      []int
	searchMessagesQueryTransferred int
	needsFTSBackfillCalls          atomic.Int32

	sourcesByLookup    map[string][]*store.Source
	sourcesByLookupErr error
	collections        map[string]*store.CollectionWithSources
}

func (m *mockStore) GetStats() (*StoreStats, error) {
	if m.getStatsFunc != nil {
		m.getStatsFunc()
		return &StoreStats{}, nil
	}
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

// The Context variants make mockStore satisfy CtxMessageStore, so handler
// tests exercise the same context-aware read path used in production.
func (m *mockStore) GetStatsContext(ctx context.Context) (*StoreStats, error) {
	if m.getStatsContextFunc != nil {
		return nil, m.getStatsContextFunc(ctx)
	}
	if m.statsErr != nil {
		return nil, m.statsErr
	}
	return m.GetStats()
}

func (m *mockStore) ListMessagesContext(_ context.Context, offset, limit int) ([]APIMessage, int64, error) {
	return m.ListMessages(offset, limit)
}

func (m *mockStore) GetMessageContext(_ context.Context, id int64) (*APIMessage, error) {
	return m.GetMessage(id)
}

func (m *mockStore) GetMessagesSummariesByIDsContext(_ context.Context, ids []int64) ([]APIMessage, error) {
	if m.summariesErr != nil {
		return nil, m.summariesErr
	}
	return m.GetMessagesSummariesByIDs(ids)
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
	m.searchMessagesQueryLimits = append(m.searchMessagesQueryLimits, limit)
	if q != nil {
		cp := *q
		cp.AccountIDs = append([]int64(nil), q.AccountIDs...)
		cp.TextTerms = append([]string(nil), q.TextTerms...)
		m.searchMessagesQueryLast = &cp
	} else {
		m.searchMessagesQueryLast = nil
	}
	if m.searchMessagesQueryFunc != nil {
		messages, total, err := m.searchMessagesQueryFunc(q, offset, limit)
		m.searchMessagesQueryTransferred += len(messages)
		return messages, total, err
	}
	m.searchMessagesQueryTransferred += len(m.messages)
	return m.messages, m.total, nil
}

func (m *mockStore) GetStatsForScope(sourceIDs []int64) (*store.Stats, error) {
	if m.getScopedStatsFunc != nil {
		m.getScopedStatsFunc(sourceIDs)
		return &store.Stats{}, nil
	}
	if m.stats == nil {
		return &store.Stats{}, nil
	}
	return m.stats, nil
}

func (m *mockStore) GetStatsForScopeContext(
	ctx context.Context,
	sourceIDs []int64,
) (*store.Stats, error) {
	if m.getScopedStatsCtxFunc != nil {
		return nil, m.getScopedStatsCtxFunc(ctx, sourceIDs)
	}
	return m.GetStatsForScope(sourceIDs)
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
	m.needsFTSBackfillCalls.Add(1)
	if m.needsFTSBackfillFunc != nil {
		return m.needsFTSBackfillFunc()
	}
	return m.needsFTSBackfill
}

func (m *mockStore) NeedsFTSBackfillQuick() bool {
	return m.needsFTSBackfillQuick
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

func TestDaemonIdentityEndpointProvesRuntimeSecretWithoutReceivingIt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	challenge, err := daemonauth.NewChallenge()
	require.NoError(err, "create challenge")

	srv := NewServerWithOptions(ServerOptions{
		Config:        &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Scheduler:     newMockScheduler(),
		Logger:        testLogger(),
		ShutdownToken: "runtime-secret",
	})
	req := httptest.NewRequest(http.MethodGet, DaemonIdentityPath, nil)
	req.Header.Set(DaemonIdentityChallengeHeader, challenge)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusNoContent, w.Code, "identity proof status")
	proof := w.Header().Get(DaemonIdentityProofHeader)
	assert.True(daemonauth.VerifyProof("runtime-secret", challenge, os.Getpid(), proof),
		"proof authenticates this daemon PID")
	assert.NotContains(proof, "runtime-secret", "response does not disclose the runtime secret")

	invalid := httptest.NewRequest(http.MethodGet, DaemonIdentityPath, nil)
	invalid.Header.Set(DaemonIdentityChallengeHeader, "invalid")
	invalidResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(invalidResp, invalid)
	assert.Equal(http.StatusBadRequest, invalidResp.Code, "malformed challenge status")
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

// deadlineClearingRecorder records deadline calls so tests can verify the
// timeout middleware clears absolute connection deadlines for long requests.
type deadlineClearingRecorder struct {
	*httptest.ResponseRecorder

	readDeadlines  []time.Time
	writeDeadlines []time.Time
}

func (w *deadlineClearingRecorder) SetReadDeadline(deadline time.Time) error {
	w.readDeadlines = append(w.readDeadlines, deadline)
	return nil
}

func (w *deadlineClearingRecorder) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadlines = append(w.writeDeadlines, deadline)
	return nil
}

func TestTimeoutMiddlewareDeadlinePolicy(t *testing.T) {
	const apiKey = "deadline-policy-test-key"
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{
			APIPort: 8080,
			APIKey:  apiKey,
		}},
		Logger: testLogger(),
	})

	handler := srv.timeoutMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	longPath := httptest.NewRequest(http.MethodPost, "/api/v1/cli/sync-full", nil)
	marked := httptest.NewRequest(http.MethodGet, "/api/v1/cli/stats", nil)
	marked.RemoteAddr = "127.0.0.1:4242"
	marked.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
	marked.Header.Set("X-Api-Key", apiKey)
	markedDeleteDeduped := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/delete-deduped",
		nil,
	)
	markedDeleteDeduped.RemoteAddr = "127.0.0.1:4242"
	markedDeleteDeduped.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
	markedDeleteDeduped.Header.Set("X-Api-Key", apiKey)
	meetingImport := httptest.NewRequest(http.MethodPost, "/api/v1/import/meeting", nil)
	meetingImport.Header.Set("X-Api-Key", apiKey)
	unauthorizedMeetingImport := httptest.NewRequest(http.MethodPost, "/api/v1/import/meeting", nil)
	unauthorizedMeetingImport.Header.Set("X-Api-Key", "invalid")
	bounded := httptest.NewRequest(http.MethodGet, "/api/v1/cli/stats", nil)

	tests := []struct {
		name             string
		request          *http.Request
		wantReadClear    bool
		wantReadDeadline bool
		wantWriteClear   bool
	}{
		{name: "unmarked long path", request: longPath, wantWriteClear: true},
		{name: "marked request", request: marked, wantReadClear: true, wantWriteClear: true},
		{
			name:           "marked atomic dedup deletion",
			request:        markedDeleteDeduped,
			wantReadClear:  true,
			wantWriteClear: true,
		},
		{
			name:             "meeting import",
			request:          meetingImport,
			wantReadDeadline: true,
			wantWriteClear:   true,
		},
		{
			name:           "unauthorized meeting import",
			request:        unauthorizedMeetingImport,
			wantWriteClear: true,
		},
		{name: "bounded request", request: bounded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			recorder := &deadlineClearingRecorder{ResponseRecorder: httptest.NewRecorder()}
			handler.ServeHTTP(recorder, tt.request)
			if tt.wantReadClear {
				require.Len(recorder.readDeadlines, 1, "read deadline changes")
				assert.True(recorder.readDeadlines[0].IsZero(), "read deadline cleared, not extended")
			} else if tt.wantReadDeadline {
				require.Len(recorder.readDeadlines, 1, "read deadline changes")
				remaining := time.Until(recorder.readDeadlines[0])
				assert.Greater(remaining, DaemonLongRequestTimeout-time.Second)
				assert.LessOrEqual(remaining, DaemonLongRequestTimeout)
			} else {
				assert.Empty(recorder.readDeadlines, "request keeps the server read deadline")
			}
			if tt.wantWriteClear {
				require.Len(recorder.writeDeadlines, 1, "write deadline changes")
				assert.True(recorder.writeDeadlines[0].IsZero(), "write deadline cleared, not extended")
			} else {
				assert.Empty(recorder.writeDeadlines, "request keeps the server write deadline")
			}
		})
	}
}

func TestCLIRequestDurationPolicy(t *testing.T) {
	tests := []struct {
		name        string
		apiKey      string
		bindAddr    string
		allowUnsafe bool
		configure   func(*Server, *http.Request)
		wantTimeout bool
	}{
		{
			name: "keyless loopback CLI",
			configure: func(_ *Server, req *http.Request) {
				req.RemoteAddr = "127.0.0.1:4242"
				req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
			},
		},
		{
			name:        "keyless remote CLI remains bounded",
			bindAddr:    "0.0.0.0",
			allowUnsafe: true,
			wantTimeout: true,
			configure: func(srv *Server, req *http.Request) {
				req.RemoteAddr = "198.51.100.23:4242"
				req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
				assert.True(t, srv.apiRequestAuthorized(req), "allow-insecure keyless remote request stays authorized")
			},
		},
		{
			name:        "forwarded loopback cannot spoof keyless remote CLI",
			bindAddr:    "0.0.0.0",
			allowUnsafe: true,
			wantTimeout: true,
			configure: func(srv *Server, req *http.Request) {
				req.RemoteAddr = "198.51.100.23:4242"
				req.Header.Set("Forwarded", "for=127.0.0.1")
				req.Header.Set("X-Forwarded-For", "127.0.0.1")
				req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
				assert.True(t, srv.apiRequestAuthorized(req), "allow-insecure keyless remote request stays authorized")
			},
		},
		{
			name:   "API key CLI",
			apiKey: cliTimeoutTestAPIKey,
			configure: func(_ *Server, req *http.Request) {
				req.RemoteAddr = "198.51.100.23:4242"
				req.Header.Set("X-Api-Key", cliTimeoutTestAPIKey)
				req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
			},
		},
		{
			name:        "unmarked API request",
			wantTimeout: true,
		},
		{
			name:        "browser session cannot opt in",
			apiKey:      cliTimeoutTestAPIKey,
			wantTimeout: true,
			configure: func(srv *Server, req *http.Request) {
				id, _, err := srv.sessions.create()
				require.NoError(t, err, "create session")
				req.AddCookie(&http.Cookie{
					Name:     sessionCookieName,
					Value:    id,
					Secure:   true,
					HttpOnly: true,
					SameSite: http.SameSiteStrictMode,
				})
				req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServerWithOptions(ServerOptions{
				Config: &config.Config{Server: config.ServerConfig{
					APIPort:       8080,
					APIKey:        tt.apiKey,
					BindAddr:      tt.bindAddr,
					AllowInsecure: tt.allowUnsafe,
				}},
				Logger:         testLogger(),
				RequestTimeout: 5 * time.Millisecond,
			})
			t.Cleanup(func() {
				require.NoError(t, srv.Shutdown(context.Background()))
			})

			handlerResult := make(chan error, 1)
			handler := srv.timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				select {
				case <-time.After(40 * time.Millisecond):
					handlerResult <- nil
				case <-r.Context().Done():
					handlerResult <- r.Context().Err()
				}
			}))
			req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/stats", nil)
			if tt.configure != nil {
				tt.configure(srv, req)
			}

			handler.ServeHTTP(httptest.NewRecorder(), req)
			err := <-handlerResult
			if tt.wantTimeout {
				assert.ErrorIs(t, err, context.DeadlineExceeded)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestTimeoutMiddlewareMarkedRequestPreservesCallerCancellation(t *testing.T) {
	require := require.New(t)
	srv := NewServerWithOptions(ServerOptions{
		Config:         &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Logger:         testLogger(),
		RequestTimeout: 5 * time.Millisecond,
	})

	started := make(chan struct{})
	handlerResult := make(chan error, 1)
	handler := srv.timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		handlerResult <- r.Context().Err()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/stats", nil).WithContext(ctx)
	req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
	requestDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), req)
		close(requestDone)
	}()

	require.Eventually(func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond, "handler starts")
	cancel()

	select {
	case err := <-handlerResult:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(time.Second):
		require.FailNow("handler did not observe caller cancellation")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		require.FailNow("marked request did not return after caller cancellation")
	}
}

func TestMarkedCLIProtectiveCeilingInventory(t *testing.T) {
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{
			APIPort: 8080,
			APIKey:  cliTimeoutTestAPIKey,
		}},
		Logger: testLogger(),
	})

	protectiveRoutes := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/v1/cli/cache-stats"},
		{method: http.MethodPost, path: "/api/v1/cli/build-cache"},
		{method: http.MethodPost, path: "/api/v1/cli/add-calendar/plan"},
		{method: http.MethodPost, path: "/api/v1/cli/delete-staged/plan"},
		{method: http.MethodPost, path: "/api/v1/cli/deletion-manifests"},
		{method: http.MethodPost, path: "/api/v1/cli/embeddings/plan"},
		{method: http.MethodGet, path: "/api/v1/cli/message"},
		{method: http.MethodGet, path: "/api/v1/cli/message/raw"},
		{method: http.MethodGet, path: "/api/v1/cli/attachment"},
		{method: http.MethodGet, path: "/api/v1/cli/search"},
		{method: http.MethodPost, path: "/api/v1/cli/deduplicate/plan"},
		{method: http.MethodPost, path: "/api/v1/cli/identities"},
		{method: http.MethodDelete, path: "/api/v1/cli/identities"},
		{method: http.MethodPost, path: "/api/v1/cli/identities/import"},
	}

	for _, route := range protectiveRoutes {
		name := route.method + " " + route.path
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var deadline time.Time
			var hasDeadline bool
			handler := srv.timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				deadline, hasDeadline = r.Context().Deadline()
			}))
			req := httptest.NewRequest(route.method, route.path, nil)
			req.Header.Set("X-Api-Key", cliTimeoutTestAPIKey)
			req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
			recorder := &deadlineClearingRecorder{ResponseRecorder: httptest.NewRecorder()}

			started := time.Now()
			handler.ServeHTTP(recorder, req)

			require.True(hasDeadline, "protective route receives a context deadline")
			remaining := time.Until(deadline)
			assert.Greater(remaining, DaemonLongRequestTimeout-time.Second)
			assert.LessOrEqual(remaining, DaemonLongRequestTimeout)
			assert.False(deadline.Before(started), "protective deadline is in the future")
			require.Len(recorder.readDeadlines, 1, "protective route extends the server read deadline")
			readRemaining := time.Until(recorder.readDeadlines[0])
			assert.Greater(readRemaining, DaemonLongRequestTimeout-time.Second)
			assert.LessOrEqual(readRemaining, DaemonLongRequestTimeout)
			assert.Empty(recorder.writeDeadlines, "protective route keeps the server write deadline")
		})
	}
}

func TestMarkedCLIProtectiveRouteCanReadBodyPastOrdinaryServerTimeout(t *testing.T) {
	const ordinaryReadTimeout = 100 * time.Millisecond

	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{
			APIPort: 8080,
			APIKey:  cliTimeoutTestAPIKey,
		}},
		Logger: testLogger(),
	})
	srv.readTimeout = ordinaryReadTimeout
	handlerEntered := make(chan struct{}, 1)
	srv.router = srv.timeoutMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerEntered <- struct{}{}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestTimeout)
			return
		}
		if string(body) != "ab" {
			http.Error(w, "unexpected body", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen")
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.StartOnListener(listener)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx), "shutdown")
		require.ErrorIs(t, <-serveErr, http.ErrServerClosed, "serve result")
	})

	slowBodyStatus := func(t *testing.T, path string, marked bool) (int, error) {
		t.Helper()
		conn, err := net.Dial("tcp", listener.Addr().String())
		require.NoError(t, err, "dial server")
		defer func() { _ = conn.Close() }()
		require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)), "bound test connection")

		marker := ""
		if marked {
			marker = fmt.Sprintf("%s: %s\r\n",
				apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
		}
		_, err = fmt.Fprintf(conn,
			"POST %s HTTP/1.1\r\n"+
				"Host: %s\r\n"+
				"X-Api-Key: %s\r\n"+
				"%s"+
				"Content-Length: 2\r\n\r\n"+
				"a",
			path,
			listener.Addr().String(),
			cliTimeoutTestAPIKey,
			marker,
		)
		require.NoError(t, err, "write headers and first body byte")

		select {
		case <-handlerEntered:
		case <-time.After(2 * time.Second):
			require.FailNow(t, "inner handler did not start")
		}
		time.Sleep(2 * ordinaryReadTimeout)
		if marked {
			_, err = conn.Write([]byte("b"))
			require.NoError(t, err, "write delayed body byte")
		} else {
			// The server may close the connection as soon as the ordinary
			// read deadline expires, so a late control write can return
			// EPIPE/closed socket before the 408 response is read.
			_, _ = conn.Write([]byte("b"))
		}

		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
		if err != nil {
			return 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode, nil
	}

	t.Run("protective route extends read deadline", func(t *testing.T) {
		status, err := slowBodyStatus(t, "/api/v1/cli/build-cache", true)
		require.NoError(t, err, "read marked response")
		assert.Equal(t, http.StatusNoContent, status)
	})
	t.Run("unmarked route keeps ordinary read deadline", func(t *testing.T) {
		status, err := slowBodyStatus(t, "/api/v1/cli/stats", false)
		if err != nil {
			// net/http may write a 408 before closing the connection, or the
			// platform may abort the connection as soon as the read deadline
			// expires. Either outcome proves the delayed body was bounded.
			var netErr *net.OpError
			require.ErrorAs(t, err, &netErr, "read deadline closes the connection")
			return
		}
		assert.Equal(t, http.StatusRequestTimeout, status)
	})
}
