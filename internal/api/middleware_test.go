package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
)

func TestCORSMiddleware(t *testing.T) {
	cfg := DefaultCORSConfig()
	middleware := CORSMiddleware(cfg)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name           string
		method         string
		origin         string
		wantStatus     int
		wantCORSHeader bool
	}{
		{
			name:           "no origin",
			method:         "GET",
			origin:         "",
			wantStatus:     http.StatusOK,
			wantCORSHeader: false,
		},
		{
			name:           "with origin",
			method:         "GET",
			origin:         "http://localhost:3000",
			wantStatus:     http.StatusOK,
			wantCORSHeader: true,
		},
		{
			name:           "preflight request",
			method:         "OPTIONS",
			origin:         "http://localhost:3000",
			wantStatus:     http.StatusNoContent,
			wantCORSHeader: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/api/v1/stats", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			assertpkg.Equal(t, tt.wantStatus, w.Code, "status")

			corsHeader := w.Header().Get("Access-Control-Allow-Origin")
			if tt.wantCORSHeader {
				assertpkg.NotEmpty(t, corsHeader, "expected CORS header to be set")
			} else {
				assertpkg.Empty(t, corsHeader, "unexpected CORS header")
			}
		})
	}
}

func TestCORSPreflightHeaders(t *testing.T) {
	assert := assertpkg.New(t)
	cfg := DefaultCORSConfig()
	middleware := CORSMiddleware(cfg)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("OPTIONS", "/api/v1/stats", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Check all preflight headers
	assert.NotEmpty(w.Header().Get("Access-Control-Allow-Origin"), "missing Access-Control-Allow-Origin")
	assert.NotEmpty(w.Header().Get("Access-Control-Allow-Methods"), "missing Access-Control-Allow-Methods")
	assert.NotEmpty(w.Header().Get("Access-Control-Allow-Headers"), "missing Access-Control-Allow-Headers")
	assert.NotEmpty(w.Header().Get("Access-Control-Max-Age"), "missing Access-Control-Max-Age")
}

func TestRateLimiter(t *testing.T) {
	assert := assertpkg.New(t)
	rl := NewRateLimiter(2, 2) // 2 req/sec with burst of 2

	// First two requests should succeed (burst)
	assert.True(rl.Allow("127.0.0.1"), "first request should be allowed")
	assert.True(rl.Allow("127.0.0.1"), "second request should be allowed (burst)")

	// Third request should be rate limited
	assert.False(rl.Allow("127.0.0.1"), "third request should be rate limited")

	// Different IP should still be allowed
	assert.True(rl.Allow("192.168.1.1"), "different IP should be allowed")
}

func TestRateLimiterCloseConcurrent(t *testing.T) {
	rl := NewRateLimiter(10, 10)

	// Spawn many goroutines calling Close() concurrently — must not panic.
	const n = 50
	start := make(chan struct{})
	done := make(chan struct{}, n)
	for range n {
		go func() {
			<-start
			rl.Close()
			done <- struct{}{}
		}()
	}
	close(start) // release all at once
	for range n {
		<-done
	}
	// If we get here without a panic, the test passes.
}

func TestRateLimitMiddleware(t *testing.T) {
	rl := NewRateLimiter(1, 1) // Very restrictive for testing
	middleware := RateLimitMiddleware(rl)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request should succeed
	req1 := httptest.NewRequest("GET", "/test", nil)
	req1.RemoteAddr = "127.0.0.1:1234"
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	assertpkg.Equal(t, http.StatusOK, w1.Code, "first request status")

	// Second immediate request should be rate limited
	req2 := httptest.NewRequest("GET", "/test", nil)
	req2.RemoteAddr = "127.0.0.1:1234"
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	assertpkg.Equal(t, http.StatusTooManyRequests, w2.Code, "second request status")

	// Check Retry-After header
	assertpkg.NotEmpty(t, w2.Header().Get("Retry-After"), "missing Retry-After header on rate limited response")
}
