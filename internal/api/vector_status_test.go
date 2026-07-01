package api

import (
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
			opts := testServerOptions(t, nil)
			opts.VectorStatus = tt.status
			srv := NewServerWithOptions(opts)
			if tt.initErr != nil {
				srv.SetVectorInitError(tt.initErr)
			}

			req := httptest.NewRequest(http.MethodGet, "/api/v1/search/similar?message_id=1", nil)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)

			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			var body struct {
				Error   string `json:"error"`
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, tt.wantCode, body.Error)
			assert.Contains(t, body.Message, tt.wantMessage)
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := testServerOptions(t, nil)
			opts.VectorStatus = tt.status
			srv := NewServerWithOptions(opts)
			if tt.initErr != nil {
				srv.SetVectorInitError(tt.initErr)
			}

			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			var body HealthResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, "ok", body.Status)
			assert.Equal(t, tt.wantVector, body.Vector)
		})
	}
}

func TestStatsReportsVectorStatus(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)
	srv.vectorMu.Lock()
	srv.vectorStatus = VectorStatusInitializing
	srv.vectorMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body StatsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "initializing", body.VectorStatus)
}
