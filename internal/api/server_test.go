package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

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
	stats    *StoreStats
	messages []APIMessage
	total    int64

	// Call counts so tests can assert that bulk hydration paths use
	// GetMessagesSummariesByIDs (one round-trip) instead of looping
	// GetMessage (per-hit N+1).
	getMessageCalls          atomic.Int32
	getSummariesByIDsCalls   atomic.Int32
	getSummariesByIDsLastIDs []int64
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
	return m.messages, m.total, nil
}

func (m *mockStore) SearchMessagesQuery(q *search.Query, offset, limit int) ([]APIMessage, int64, error) {
	return m.messages, m.total, nil
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

	assertpkg.Equal(t, http.StatusOK, w.Code, "GET /health status")

	var resp map[string]string
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assertpkg.Equal(t, "ok", resp["status"], "health status")
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

	assertpkg.Equal(t, http.StatusOK, w.Code, "HEAD /health status")
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

			assertpkg.Equal(t, tt.wantStatus, w.Code, "status")
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

	assertpkg.Equal(t, http.StatusOK, w.Code, "status when no API key configured")
}

func TestSchedulerStatusEndpoint(t *testing.T) {
	assert := assertpkg.New(t)
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
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

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
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	assertpkg.False(t, resp.Running, "expected scheduler to NOT be running")
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

	assertpkg.Equal(t, http.StatusOK, w.Code, "status")

	var resp map[string][]AccountInfo
	requirepkg.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "failed to decode response")

	accounts := resp["accounts"]
	assertpkg.Len(t, accounts, 2, "expected 2 accounts")
}

func TestNilStoreReturns503(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{APIPort: 8080},
	}
	sched := newMockScheduler()
	srv := NewServer(cfg, nil, sched, testLogger())

	endpoints := []string{
		"/api/v1/stats",
		"/api/v1/messages",
		"/api/v1/messages/1",
		"/api/v1/search?q=test",
	}

	for _, path := range endpoints {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)

			assertpkg.Equal(t, http.StatusServiceUnavailable, w.Code, "%s", path)
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

			assertpkg.Equal(t, http.StatusServiceUnavailable, w.Code, "%s %s", ep.method, ep.path)
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
				assertpkg.Error(t, err, "ValidateSecure()")
			} else {
				assertpkg.NoError(t, err, "ValidateSecure()")
			}
		})
	}
}

func TestCORSFromConfig(t *testing.T) {
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

	assertpkg.Equal(t, "http://localhost:3000", w.Header().Get("Access-Control-Allow-Origin"),
		"expected CORS header for allowed origin")

	// Request from disallowed origin
	req2 := httptest.NewRequest(http.MethodGet, "/health", nil)
	req2.Header.Set("Origin", "http://evil.com")
	w2 := httptest.NewRecorder()
	srv.Router().ServeHTTP(w2, req2)

	assertpkg.Empty(t, w2.Header().Get("Access-Control-Allow-Origin"),
		"expected no CORS header for disallowed origin")
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

	assertpkg.Empty(t, w.Header().Get("Access-Control-Allow-Origin"),
		"expected no CORS header when no origins configured")
}
