package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
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
	srv.SetVectorFeatures(nil, backend, vector.Config{})

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

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(http.StatusOK, rec.Code)
	var body HealthResponse
	require.NoError(json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotNil(body.Vector)
	assert.Equal("stale", body.Vector.Status)
	assert.Contains(body.Vector.Error, "old:1")
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
	srv.SetVectorFeatures(nil, backend, cfg)
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
	srv.SetVectorFeatures(nil, backend, cfg)
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
	srv.SetVectorFeatures(nil, &fakeVectorBackend{}, vector.Config{})
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
		{"error carries message", VectorStatusError, errors.New("migration exploded"),
			&VectorHealth{Status: "error", Error: "migration exploded"}},
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
