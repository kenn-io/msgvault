package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// minimalVectorBackend is a minimal vector.Backend for status tests. Embed the
// interface so only the methods a test touches need implementations; the
// status tests never call any of them.
type minimalVectorBackend struct {
	vector.Backend
}

func testServerOptions(t *testing.T, backend vector.Backend) ServerOptions {
	t.Helper()
	return ServerOptions{
		Config:  &config.Config{},
		Logger:  slog.New(slog.DiscardHandler),
		Backend: backend,
	}
}

func TestVectorStatusDerivedFromOptions(t *testing.T) {
	tests := []struct {
		name string
		opts ServerOptions
		want VectorStatus
	}{
		{"no backend defaults to disabled", testServerOptions(t, nil), VectorStatusDisabled},
		{"backend defaults to ready", testServerOptions(t, &minimalVectorBackend{}), VectorStatusReady},
		{
			"explicit initializing wins",
			func() ServerOptions {
				o := testServerOptions(t, nil)
				o.VectorStatus = VectorStatusInitializing
				return o
			}(),
			VectorStatusInitializing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServerWithOptions(tt.opts)
			status, errMsg := srv.VectorStatus()
			assert.Equal(t, tt.want, status)
			assert.Empty(t, errMsg)
		})
	}
}

func TestSetVectorFeaturesTransitionsToReady(t *testing.T) {
	opts := testServerOptions(t, nil)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)

	backend := &minimalVectorBackend{}
	srv.SetVectorFeatures(nil, nil, backend, vector.Config{})

	status, errMsg := srv.VectorStatus()
	assert.Equal(t, VectorStatusReady, status)
	assert.Empty(t, errMsg)
	_, gotBackend, _ := srv.vectorComponents()
	require.NotNil(t, gotBackend)
}

func TestSetVectorInitErrorTransitionsToError(t *testing.T) {
	opts := testServerOptions(t, nil)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)

	srv.SetVectorInitError(errors.New("migration exploded"))

	status, errMsg := srv.VectorStatus()
	assert.Equal(t, VectorStatusError, status)
	assert.Contains(t, errMsg, "migration exploded")
}

func TestSetVectorInitErrorNilIsNoOp(t *testing.T) {
	opts := testServerOptions(t, nil)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)

	srv.SetVectorInitError(nil)

	status, errMsg := srv.VectorStatus()
	assert.Equal(t, VectorStatusInitializing, status)
	assert.Empty(t, errMsg)
}

func TestSetVectorStaleTransitionsToStale(t *testing.T) {
	opts := testServerOptions(t, &minimalVectorBackend{})
	srv := NewServerWithOptions(opts)

	srv.SetVectorStale("active=\"old:1\" configured=\"new:2\"; run rebuild")

	status, errMsg := srv.VectorStatus()
	assert.Equal(t, VectorStatusStale, status)
	assert.Contains(t, errMsg, "old:1")
}

func TestSetVectorStaleEmptyIsNoOp(t *testing.T) {
	opts := testServerOptions(t, &minimalVectorBackend{})
	srv := NewServerWithOptions(opts)

	srv.SetVectorStale("")

	status, errMsg := srv.VectorStatus()
	assert.Equal(t, VectorStatusReady, status)
	assert.Empty(t, errMsg)
}

func TestSimilarSearchStale503(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	opts := testServerOptions(t, nil)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)
	srv.SetVectorStale("active=\"old:1\" configured=\"new:2\"; run `msgvault embeddings build --full-rebuild`")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/similar?message_id=1", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(http.StatusServiceUnavailable, rec.Code)
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal("index_stale", body.Error)
	assert.Contains(body.Message, "old:1")
}

func TestHealthReportsStaleVectorStatus(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	opts := testServerOptions(t, nil)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)
	srv.SetVectorStale("active=\"old:1\" configured=\"new:2\"")

	// The unauthenticated /health carries the status but hides the detail
	// (it can name configured account identifiers); the authenticated
	// /api/v1/health carries the full detail.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(http.StatusOK, rec.Code)
	var body HealthResponse
	require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(body.Vector)
	assert.Equal("stale", body.Vector.Status)
	assert.NotContains(body.Vector.Error, "old:1", "public health must not expose the stale detail")
	assert.Contains(body.Vector.Error, "authenticated health")

	rec = httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	require.Equal(http.StatusOK, rec.Code)
	require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(body.Vector)
	assert.Equal("stale", body.Vector.Status)
	assert.Contains(body.Vector.Error, "old:1", "authenticated health carries the detail")
}

// resolvingVectorBackend reports a single active generation with a fixed
// fingerprint, so refreshVectorStatusIfStale can re-run the same generation
// check the query path uses and clear a stale status once the index matches.
type resolvingVectorBackend struct {
	vector.Backend

	fingerprint string
}

func (b *resolvingVectorBackend) ActiveGeneration(context.Context) (vector.Generation, error) {
	return vector.Generation{ID: 1, Fingerprint: b.fingerprint}, nil
}

// TestHealthClearsStaleAfterReactivation verifies the latched stale status is
// re-validated when reporting health: once the active generation's fingerprint
// matches the configured one again (e.g. after a --full-rebuild), /health flips
// back to ready without a daemon restart.
func TestHealthClearsStaleAfterReactivation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg := vector.Config{}
	backend := &resolvingVectorBackend{fingerprint: cfg.GenerationFingerprint()}
	opts := testServerOptions(t, backend)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)
	srv.SetVectorFeatures(nil, nil, backend, cfg)
	srv.SetVectorStale("active=\"old:1\" configured=\"new:2\"")

	// Sanity: status is stale before the health check re-validates.
	status, _ := srv.VectorStatus()
	require.Equal(VectorStatusStale, status, "precondition: latched stale")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(http.StatusOK, rec.Code)
	var body HealthResponse
	require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(body.Vector)
	assert.Equal("ready", body.Vector.Status, "stale must clear once the index matches again")
}

// TestScopeDriftStaleSurvivesRefresh pins the scope-drift latch: after
// SetVectorScopeDrift, the active generation still matches the STARTUP
// fingerprint, so the ordinary stale refresh would clear the status on the
// next request while searches keep serving the wrongly-scoped index. The
// latch must hold through health checks and clear only on reinit
// (SetVectorFeatures).
func TestScopeDriftStaleSurvivesRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg := vector.Config{}
	backend := &resolvingVectorBackend{fingerprint: cfg.GenerationFingerprint()}
	opts := testServerOptions(t, backend)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)
	srv.SetVectorFeatures(nil, nil, backend, cfg)
	srv.SetVectorScopeDrift("configured embedding scope now resolves to \"src-9\" but vector search was initialized with \"src-7\"")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(http.StatusOK, rec.Code)
	var body HealthResponse
	require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(body.Vector)
	assert.Equal("stale", body.Vector.Status,
		"a matching startup fingerprint must not clear scope-drift stale")
	assert.Contains(body.Vector.Error, "src-9")

	srv.SetVectorFeatures(nil, nil, backend, cfg)
	status, errMsg := srv.VectorStatus()
	assert.Equal(VectorStatusReady, status, "reinit clears the latch")
	assert.Empty(errMsg)
}

// TestVectorSearchEndpointsGateOnScopeDrift pins the preflight gate: the
// installed engine/backend validate only the STARTUP fingerprint at query
// time, which still matches after embedding-scope drift, so every vector
// search entry point must consult the stale status itself and 503 with
// index_stale instead of serving the wrongly-scoped index.
func TestVectorSearchEndpointsGateOnScopeDrift(t *testing.T) {
	newDriftedServer := func(t *testing.T) *Server {
		t.Helper()
		cfg := vector.Config{}
		backend := &resolvingVectorBackend{fingerprint: cfg.GenerationFingerprint()}
		opts := testServerOptions(t, backend)
		opts.VectorStatus = VectorStatusInitializing
		opts.Store = &mockStore{}
		srv := NewServerWithOptions(opts)
		srv.SetVectorFeatures(hybrid.NewEngine(backend, nil, nil, hybrid.Config{}), nil, backend, cfg)
		srv.SetVectorScopeDrift("configured embedding scope now resolves to \"src-9\" but vector search was initialized with \"src-7\"")
		return srv
	}
	assertIndexStale := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		var body struct {
			Error string `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "index_stale", body.Error)
	}

	t.Run("hybrid search", func(t *testing.T) {
		srv := newDriftedServer(t)
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=hello&mode=hybrid", nil))
		assertIndexStale(t, rec)
	})

	t.Run("similar search", func(t *testing.T) {
		srv := newDriftedServer(t)
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search/similar?message_id=1", nil))
		assertIndexStale(t, rec)
	})

	t.Run("explore semantic including cached snapshot", func(t *testing.T) {
		srv := newDriftedServer(t)
		rec := httptest.NewRecorder()
		_, _, ok := srv.resolveExploreVectorSearch(context.Background(), rec, ExploreHTTPRequest{
			Query: "hello", SearchMode: exploreSearchModeSemantic, CandidateSnapshotID: "cached-snapshot",
		})
		assert.False(t, ok, "a drifted scope must not resolve a semantic search spec")
		assertIndexStale(t, rec)
	})
}

// TestVectorSearchPreflightDetectsScopeDrift pins the search-path drift
// detection: with an empty embed cron and run_after_sync=false the embed
// job never runs, so a search request must be able to be the event that
// re-resolves the scope and latches the stale status. The check is
// throttled to at most once per interval.
func TestVectorSearchPreflightDetectsScopeDrift(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg := vector.Config{}
	backend := &resolvingVectorBackend{fingerprint: cfg.GenerationFingerprint()}
	opts := testServerOptions(t, backend)
	opts.VectorStatus = VectorStatusInitializing
	opts.Store = &mockStore{}
	srv := NewServerWithOptions(opts)
	srv.SetVectorFeatures(hybrid.NewEngine(backend, nil, nil, hybrid.Config{}), nil, backend, cfg)

	checks := 0
	srv.SetVectorScopeCheck(func(context.Context) (string, error) {
		checks++
		return "configured embedding scope now resolves to \"src-9\" but vector search was initialized with \"src-7\"", nil
	})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search/similar?message_id=1", nil))
	require.Equal(http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal("index_stale", body.Error)
	assert.Equal(1, checks, "the first search runs the drift check")

	status, errMsg := srv.VectorStatus()
	assert.Equal(VectorStatusStale, status, "drift found by the preflight latches stale")
	assert.Contains(errMsg, "src-9")

	rec = httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search/similar?message_id=1", nil))
	require.Equal(http.StatusServiceUnavailable, rec.Code)
	assert.Equal(1, checks, "a latched stale must not re-run the check")
}

// TestVectorSearchPreflightThrottlesScopeCheck pins the throttle for the
// no-drift case: consecutive searches within the interval run the (main-DB)
// resolution once, and a resolution error neither blocks the search nor
// changes the status.
func TestVectorSearchPreflightThrottlesScopeCheck(t *testing.T) {
	assert := assert.New(t)

	cfg := vector.Config{}
	backend := &resolvingVectorBackend{fingerprint: cfg.GenerationFingerprint()}
	opts := testServerOptions(t, backend)
	opts.VectorStatus = VectorStatusInitializing
	opts.Store = &mockStore{}
	srv := NewServerWithOptions(opts)
	srv.SetVectorFeatures(hybrid.NewEngine(backend, nil, nil, hybrid.Config{}), nil, backend, cfg)

	checks := 0
	srv.SetVectorScopeCheck(func(context.Context) (string, error) {
		checks++
		return "", errors.New("database is locked")
	})

	for range 3 {
		rec := httptest.NewRecorder()
		srv.vectorSearchPreflight(context.Background(), rec)
	}
	assert.Equal(1, checks, "the scope check runs at most once per interval")
	status, _ := srv.VectorStatus()
	assert.Equal(VectorStatusReady, status, "a resolution error must not change the status")
}

// TestHealthFlipsReadyToStaleAfterForeignActivation pins the ready→stale
// revalidation: a daemon-proxied one-off scoped build activates a generation
// whose fingerprint no longer matches the installed configuration without
// changing the configured scope, so nothing latches drift — searches 503
// with index_stale from the query-time check while health kept reporting
// ready. Health (and the search preflight) must re-run the freshness check
// and flip the status. The check is throttled, and only a fingerprint
// mismatch flips it — a matching index stays ready.
func TestHealthFlipsReadyToStaleAfterForeignActivation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg := vector.Config{
		Enabled:    true,
		Embeddings: vector.EmbeddingsConfig{Model: "test", Dimension: 2},
	}
	backend := &resolvingVectorBackend{fingerprint: cfg.GenerationFingerprint()}
	opts := testServerOptions(t, backend)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)
	srv.SetVectorFeatures(nil, nil, backend, cfg)

	health := func() string {
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		require.Equal(http.StatusOK, rec.Code)
		var body HealthResponse
		require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
		require.NotNil(body.Vector)
		return body.Vector.Status
	}

	assert.Equal("ready", health(), "matching active generation stays ready")

	// A one-off scoped build activates a differently-fingerprinted
	// generation behind the daemon's back.
	backend.fingerprint = cfg.GenerationFingerprint() + ":ssrc-3"
	assert.Equal("ready", health(), "throttled: the flip waits for the next check window")

	srv.vectorMu.Lock()
	srv.vectorFreshNextCheck = time.Time{}
	srv.vectorMu.Unlock()
	assert.Equal("stale", health(), "a mismatched active generation must flip health to stale")

	status, detail := srv.VectorStatus()
	assert.Equal(VectorStatusStale, status)
	assert.Contains(detail, "one-off account-scoped generation", "detail explains the recovery")

	// The plain stale clears through the ordinary refresh once a matching
	// generation activates again.
	backend.fingerprint = cfg.GenerationFingerprint()
	assert.Equal("ready", health(), "reactivating a matching generation clears the stale")
}

// TestHealthDetectsScopeDrift pins that the status-reporting endpoints run
// the scope-drift check themselves: with embed scheduling disabled, a
// removed or remapped configured account must flip health to stale on the
// next poll rather than waiting for a vector search or coverage request to
// fire the preflight.
func TestHealthDetectsScopeDrift(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg := vector.Config{}
	backend := &resolvingVectorBackend{fingerprint: cfg.GenerationFingerprint()}
	opts := testServerOptions(t, backend)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)
	srv.SetVectorFeatures(nil, nil, backend, cfg)
	checks := 0
	srv.SetVectorScopeCheck(func(context.Context) (string, error) {
		checks++
		return "configured embedding scope now resolves to \"src-9\" but vector search was initialized with \"src-7\"", nil
	})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	require.Equal(http.StatusOK, rec.Code)
	var body HealthResponse
	require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(body.Vector)
	assert.Equal("stale", body.Vector.Status, "a health poll must detect scope drift itself")
	assert.Equal(1, checks)

	status, detail := srv.VectorStatus()
	assert.Equal(VectorStatusStale, status)
	assert.Contains(detail, "src-9")
}

// TestHealthKeepsStaleWhenStillMismatched verifies the refresh leaves the stale
// status in place while the active generation's fingerprint still mismatches.
func TestHealthKeepsStaleWhenStillMismatched(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg := vector.Config{}
	backend := &resolvingVectorBackend{fingerprint: "still-old:1"}
	opts := testServerOptions(t, backend)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)
	srv.SetVectorFeatures(nil, nil, backend, cfg)
	srv.SetVectorStale("active=\"still-old:1\" configured=\"new:2\"")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(http.StatusOK, rec.Code)
	var body HealthResponse
	require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(body.Vector)
	assert.Equal("stale", body.Vector.Status, "still-mismatched index stays stale")
}

func TestSetVectorFeaturesConcurrentReads(t *testing.T) {
	opts := testServerOptions(t, nil)
	opts.VectorStatus = VectorStatusInitializing
	srv := NewServerWithOptions(opts)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			_, _, _ = srv.vectorComponents()
			_, _ = srv.VectorStatus()
		}
	}()
	srv.SetVectorFeatures(nil, nil, &fakeVectorBackend{}, vector.Config{})
	<-done

	status, _ := srv.VectorStatus()
	assert.Equal(t, VectorStatusReady, status)
}

func TestSimilarSearchStatusAware503(t *testing.T) {
	tests := []struct {
		name        string
		status      VectorStatus
		initErr     error
		wantCode    string
		wantMessage string
	}{
		{"initializing", VectorStatusInitializing, nil, "vector_initializing", "initializing"},
		{"error", VectorStatusError, errors.New("migration exploded"), "vector_init_failed", "migration exploded"},
		{"disabled", VectorStatusDisabled, nil, "vector_not_enabled", "not configured"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			opts := testServerOptions(t, nil)
			opts.VectorStatus = tt.status
			srv := NewServerWithOptions(opts)
			if tt.initErr != nil {
				srv.SetVectorInitError(tt.initErr)
			}

			req := httptest.NewRequest(http.MethodGet, "/api/v1/search/similar?message_id=1", nil)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)

			require.Equal(http.StatusServiceUnavailable, rec.Code)
			var body struct {
				Error   string `json:"error"`
				Message string `json:"message"`
			}
			require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(tt.wantCode, body.Error)
			assert.Contains(body.Message, tt.wantMessage)
		})
	}
}

func TestHybridSearchInitializing503(t *testing.T) {
	opts := testServerOptions(t, nil)
	opts.VectorStatus = VectorStatusInitializing
	opts.Store = &mockStore{}
	srv := NewServerWithOptions(opts)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=hello&mode=hybrid", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "vector_initializing", body.Error)
}

func TestHealthReportsVectorStatus(t *testing.T) {
	tests := []struct {
		name       string
		status     VectorStatus
		initErr    error
		wantVector *VectorHealth
	}{
		{"disabled omits vector", VectorStatusDisabled, nil, nil},
		{"initializing", VectorStatusInitializing, nil, &VectorHealth{Status: "initializing"}},
		// The public endpoint reports the error STATUS but not the detail:
		// init errors can name configured account identifiers.
		{"error carries generic message", VectorStatusError, errors.New("no account found for wes@example.com"),
			&VectorHealth{Status: "error", Error: "vector search is unavailable; see the authenticated health endpoint or daemon logs for details"}},
		{"ready", VectorStatusReady, nil, &VectorHealth{Status: "ready"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			opts := testServerOptions(t, nil)
			opts.VectorStatus = tt.status
			srv := NewServerWithOptions(opts)
			if tt.initErr != nil {
				srv.SetVectorInitError(tt.initErr)
			}

			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)

			require.Equal(http.StatusOK, rec.Code)
			var body HealthResponse
			require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal("ok", body.Status)
			assert.Equal(tt.wantVector, body.Vector)
		})
	}
}

func TestStatsReportsVectorStatus(t *testing.T) {
	tests := []struct {
		name   string
		status VectorStatus
	}{
		{"initializing", VectorStatusInitializing},
		{"ready", VectorStatusReady},
		{"stale", VectorStatusStale},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv, _ := newTestServerWithMockStore(t)
			srv.vectorMu.Lock()
			srv.vectorStatus = tt.status
			srv.vectorMu.Unlock()

			req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)

			require.Equal(http.StatusOK, rec.Code)
			var body StatsResponse
			require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(string(tt.status), body.VectorStatus)
		})
	}
}

func TestStatsReportsTextVectorMessageScope(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)
	vectorCfg := vector.Config{Enabled: true}
	vectorCfg.Embed.Scope.MessageTypes = []string{"sms", "mms"}
	srv.SetVectorFeatures(nil, nil, &fakeVectorBackend{}, vectorCfg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body StatsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, []string{"mms", "sms"}, body.VectorTextMessageTypes)
}

// TestHealthReportsAnalyticsMode pins that /health carries the analytics
// engine mode the daemon selected at startup, and omits the field when the
// server was built without one, so clients can distinguish live-SQL
// fallback from cache-backed aggregates.
func TestHealthReportsAnalyticsMode(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	opts := testServerOptions(t, nil)
	opts.AnalyticsMode = AnalyticsModeSQLFallback
	srv := NewServerWithOptions(opts)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(http.StatusOK, rec.Code)
	var body HealthResponse
	require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(AnalyticsModeSQLFallback, body.AnalyticsEngine)

	bare := NewServerWithOptions(testServerOptions(t, nil))
	rec = httptest.NewRecorder()
	bare.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	require.Equal(http.StatusOK, rec.Code)
	assert.NotContains(rec.Body.String(), "analytics_engine",
		"field must be omitted when no mode was configured")
}
