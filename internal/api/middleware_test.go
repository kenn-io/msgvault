package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	webapp "go.kenn.io/msgvault/internal/web"
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

			assert.Equal(t, tt.wantStatus, w.Code, "status")

			corsHeader := w.Header().Get("Access-Control-Allow-Origin")
			if tt.wantCORSHeader {
				assert.NotEmpty(t, corsHeader, "expected CORS header to be set")
			} else {
				assert.Empty(t, corsHeader, "unexpected CORS header")
			}
		})
	}
}

func TestCORSMiddlewareCredentialsMatrix(t *testing.T) {
	tests := []struct {
		name             string
		allowedOrigins   []string
		allowCredentials bool
		origin           string
		wantAllowOrigin  string
		wantCredentials  string
	}{
		{
			name:             "wildcard with credentials never reflects origin or allows credentials",
			allowedOrigins:   []string{"*"},
			allowCredentials: true,
			origin:           "http://attacker.example:3000",
			wantAllowOrigin:  "*",
			wantCredentials:  "",
		},
		{
			name:            "wildcard without credentials emits literal wildcard",
			allowedOrigins:  []string{"*"},
			origin:          "http://localhost:3000",
			wantAllowOrigin: "*",
			wantCredentials: "",
		},
		{
			name:             "exact origin with credentials reflects origin and allows credentials",
			allowedOrigins:   []string{"http://dashboard.example"},
			allowCredentials: true,
			origin:           "http://dashboard.example",
			wantAllowOrigin:  "http://dashboard.example",
			wantCredentials:  "true",
		},
		{
			name:            "exact origin without credentials reflects origin only",
			allowedOrigins:  []string{"http://dashboard.example"},
			origin:          "http://dashboard.example",
			wantAllowOrigin: "http://dashboard.example",
			wantCredentials: "",
		},
		{
			name:             "exact match wins over wildcard and keeps credentials",
			allowedOrigins:   []string{"*", "http://dashboard.example"},
			allowCredentials: true,
			origin:           "http://dashboard.example",
			wantAllowOrigin:  "http://dashboard.example",
			wantCredentials:  "true",
		},
		{
			name:             "unlisted origin gets no CORS headers",
			allowedOrigins:   []string{"http://dashboard.example"},
			allowCredentials: true,
			origin:           "http://attacker.example",
			wantAllowOrigin:  "",
			wantCredentials:  "",
		},
		{
			name:             "literal star Origin header never matches exactly",
			allowedOrigins:   []string{"*"},
			allowCredentials: true,
			origin:           "*",
			wantAllowOrigin:  "*",
			wantCredentials:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := CORSConfig{
				AllowedOrigins:   tt.allowedOrigins,
				AllowedMethods:   defaultCORSAllowedMethods(),
				AllowedHeaders:   defaultCORSAllowedHeaders(),
				AllowCredentials: tt.allowCredentials,
			}
			handler := CORSMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
			req.Header.Set("Origin", tt.origin)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			assert.Equal(t, tt.wantAllowOrigin, w.Header().Get("Access-Control-Allow-Origin"),
				"Access-Control-Allow-Origin")
			assert.Equal(t, tt.wantCredentials, w.Header().Get("Access-Control-Allow-Credentials"),
				"Access-Control-Allow-Credentials")
			assert.Contains(t, w.Header().Values("Vary"), "Origin",
				"CORS responses must vary on Origin")
		})
	}
}

func TestCORSPreflightHeaders(t *testing.T) {
	assert := assert.New(t)
	cfg := DefaultCORSConfig()
	middleware := CORSMiddleware(cfg)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/settings", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Check all preflight headers
	assert.NotEmpty(w.Header().Get("Access-Control-Allow-Origin"), "missing Access-Control-Allow-Origin")
	methods := w.Header().Get("Access-Control-Allow-Methods")
	assert.NotEmpty(methods, "missing Access-Control-Allow-Methods")
	assert.Contains(methods, http.MethodPatch, "preflight methods should include PATCH")
	headers := w.Header().Get("Access-Control-Allow-Headers")
	assert.NotEmpty(headers, "missing Access-Control-Allow-Headers")
	assert.Contains(headers, "If-Match",
		"preflight must allow If-Match for settings and saved-view concurrency tokens")
	assert.Contains(headers, "X-Request-Id",
		"preflight must allow X-Request-Id for task-creation idempotency keys")
	assert.NotEmpty(w.Header().Get("Access-Control-Max-Age"), "missing Access-Control-Max-Age")
}

func TestCORSExposeHeaders(t *testing.T) {
	tests := []struct {
		name           string
		allowedOrigins []string
		origin         string
		wantExposed    string
	}{
		{
			name:           "exact origin exposes ETag",
			allowedOrigins: []string{"http://dashboard.example"},
			origin:         "http://dashboard.example",
			wantExposed:    "ETag",
		},
		{
			name:           "wildcard origin exposes ETag",
			allowedOrigins: []string{"*"},
			origin:         "http://localhost:3000",
			wantExposed:    "ETag",
		},
		{
			name:           "unlisted origin gets no expose header",
			allowedOrigins: []string{"http://dashboard.example"},
			origin:         "http://attacker.example",
			wantExposed:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := CORSConfig{
				AllowedOrigins: tt.allowedOrigins,
				AllowedMethods: defaultCORSAllowedMethods(),
				AllowedHeaders: defaultCORSAllowedHeaders(),
			}
			handler := CORSMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"etag-a"`)
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
			req.Header.Set("Origin", tt.origin)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			assert.Equal(t, tt.wantExposed, w.Header().Get("Access-Control-Expose-Headers"),
				"Access-Control-Expose-Headers")
		})
	}
}

func TestRateLimiter(t *testing.T) {
	assert := assert.New(t)
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

// exemptNever is a rate-limit exempt predicate that trusts no request, so
// every request runs through the limiter regardless of origin.
func exemptNever(*http.Request) bool { return false }

// exemptLoopback mirrors the keyless-mode exemption: any loopback request is
// trusted (no API key configured means apiRequestAuthorized always passes).
func exemptLoopback(r *http.Request) bool { return isLoopbackRequest(r) }

func TestRateLimitMiddleware(t *testing.T) {
	rl := NewRateLimiter(1, 1) // Very restrictive for testing
	middleware := RateLimitMiddleware(rl, exemptNever)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request should succeed
	req1 := httptest.NewRequest(http.MethodGet, "/test", nil)
	req1.RemoteAddr = "203.0.113.7:1234"
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	assert.Equal(t, http.StatusOK, w1.Code, "first request status")

	// Second immediate request should be rate limited
	req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	req2.RemoteAddr = "203.0.113.7:1234"
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	assert.Equal(t, http.StatusTooManyRequests, w2.Code, "second request status")

	// Check Retry-After header
	assert.NotEmpty(t, w2.Header().Get("Retry-After"), "missing Retry-After header on rate limited response")
}

func TestRateLimitMiddlewareExemptsLoopback(t *testing.T) {
	rl := NewRateLimiter(1, 1) // would reject the second request if applied
	middleware := RateLimitMiddleware(rl, exemptLoopback)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Keyless local mode: the TUI/CLI bursts far past the remote budget (daemon
	// discovery alone fires a dozen parallel pings), so trusted loopback clients
	// must never see 429 regardless of request rate.
	for _, remoteAddr := range []string{"127.0.0.1:1234", "[::1]:1234"} {
		for range 30 {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.RemoteAddr = remoteAddr
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code, "loopback request from %s must not be rate limited", remoteAddr)
		}
	}
}

func TestLoopbackRateLimitExempt(t *testing.T) {
	const key = "secret-key"

	newReq := func(remoteAddr, apiKey string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
		req.RemoteAddr = remoteAddr
		if apiKey != "" {
			req.Header.Set("X-Api-Key", apiKey)
		}
		return req
	}

	tests := []struct {
		name       string
		apiKey     string
		remoteAddr string
		reqKey     string
		want       bool
	}{
		{"keyless loopback exempt", "", "127.0.0.1:1234", "", true},
		{"key configured valid key loopback exempt", key, "127.0.0.1:1234", key, true},
		{"key configured missing key loopback limited", key, "127.0.0.1:1234", "", false},
		{"key configured bad key loopback limited", key, "127.0.0.1:1234", "wrong", false},
		{"key configured valid key non-loopback limited", key, "203.0.113.7:1234", key, false},
		{"keyless non-loopback limited", "", "203.0.113.7:1234", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServer(
				&config.Config{Server: config.ServerConfig{APIKey: tt.apiKey}},
				nil, nil, testLogger(),
			)
			got := srv.loopbackRateLimitExempt(newReq(tt.remoteAddr, tt.reqKey))
			require.Equal(t, tt.want, got, "loopbackRateLimitExempt")
		})
	}
}

func TestRateLimitMiddlewareLimitsUntrustedLoopback(t *testing.T) {
	rl := NewRateLimiter(1, 1) // rejects the second request within a second
	// Untrusted loopback: an API key is configured but the request has none, so
	// the predicate refuses the exemption (models brute-force through a local
	// proxy that forwards to 127.0.0.1).
	middleware := RateLimitMiddleware(rl, exemptNever)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req1 := httptest.NewRequest(http.MethodGet, "/test", nil)
	req1.RemoteAddr = "127.0.0.1:1234"
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	assert.Equal(t, http.StatusOK, w1.Code, "first loopback request status")

	req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	req2.RemoteAddr = "127.0.0.1:1234"
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusTooManyRequests, w2.Code,
		"untrusted loopback request must be rate limited")
}

func TestExistingAPIKeyAuthenticationCompatibility(t *testing.T) {
	const apiKey = "compatibility-test-key"
	tests := []struct {
		name    string
		headers http.Header
	}{
		{
			name:    "X-Api-Key",
			headers: http.Header{"X-Api-Key": []string{apiKey}},
		},
		{
			name:    "bearer authorization",
			headers: http.Header{"Authorization": []string{"Bearer " + apiKey}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServer(
				&config.Config{Server: config.ServerConfig{APIKey: apiKey}},
				nil, nil, testLogger(),
			)
			t.Cleanup(func() {
				require.NoError(t, srv.Shutdown(context.Background()))
			})

			req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
			for name, values := range tt.headers {
				for _, value := range values {
					req.Header.Add(name, value)
				}
			}
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)

			assert.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
		})
	}
}

func TestExistingAPIKeyAuthenticationKeepsKeylessLoopbackTrusted(t *testing.T) {
	srv := NewServer(&config.Config{}, nil, nil, testLogger())
	t.Cleanup(func() {
		require.NoError(t, srv.Shutdown(context.Background()))
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.RemoteAddr = "127.0.0.1:4242"
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	assert.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
}

// TestAPICacheControlDefaultsToNoStore covers the shared-cache bypass: every
// /api/ response — streamed file bytes and JSON alike — must default to
// Cache-Control: no-store so a shared proxy never stores an authenticated
// payload under its predictable URL and replays it to an unauthenticated
// requester. Web routes outside /api/ keep their own caching policy.
func TestAPICacheControlDefaultsToNoStore(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	hash := strings.Repeat("ab", 32)
	catalog := &fileCatalogStore{mockStore: &mockStore{}, files: map[int64]store.FileMetadata{
		7: {ID: 7, MessageID: 11, ConversationID: 21, Filename: "statement.pdf",
			MimeType: "application/pdf", ContentHash: hash},
	}}
	spa := webapp.NewHandler(fstest.MapFS{
		"index.html":  &fstest.MapFile{Data: []byte("<!doctype html><title>shell</title>")},
		"favicon.svg": &fstest.MapFile{Data: []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>")},
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "No route matches "+r.URL.Path)
	}))
	srv := NewServerWithOptions(ServerOptions{
		Config:     &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:      catalog,
		Engine:     &querytest.MockEngine{},
		BlobStore:  fixedFileBlobStore{content: []byte("pdf bytes")},
		Logger:     testLogger(),
		SPAHandler: spa,
	})

	download := httptest.NewRecorder()
	srv.Router().ServeHTTP(download, httptest.NewRequest(http.MethodGet, "/api/v1/files/7/content", nil))
	requirements.Equal(http.StatusOK, download.Code, download.Body.String())
	assertions.Equal("no-store", download.Header().Get("Cache-Control"), "file download must not be cacheable")

	metadata := httptest.NewRecorder()
	srv.Router().ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, "/api/v1/files/7", nil))
	requirements.Equal(http.StatusOK, metadata.Code, metadata.Body.String())
	assertions.Equal("no-store", metadata.Header().Get("Cache-Control"), "JSON API response must not be cacheable")

	unknown := httptest.NewRecorder()
	srv.Router().ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/api/v1/not-a-real-route", nil))
	requirements.Equal(http.StatusNotFound, unknown.Code)
	assertions.Equal("no-store", unknown.Header().Get("Cache-Control"), "API error response must not be cacheable")

	asset := httptest.NewRecorder()
	srv.Router().ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/favicon.svg", nil))
	requirements.Equal(http.StatusOK, asset.Code)
	assertions.Empty(asset.Header().Get("Cache-Control"), "static assets keep the web handler's caching policy")
}
