package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestSessionCSRFRequestMatrix(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		origin     string
		csrfToken  func(SessionStatus) string
		apiKey     bool
		wantStatus int
		forwarded  http.Header
	}{
		{
			name:       "safe method needs neither origin nor token",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
		},
		{
			name:       "session mutation accepts matching origin and token",
			method:     http.MethodDelete,
			origin:     "http://example.com",
			csrfToken:  func(status SessionStatus) string { return status.CSRFToken },
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "session mutation rejects missing origin",
			method:     http.MethodDelete,
			csrfToken:  func(status SessionStatus) string { return status.CSRFToken },
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "session mutation rejects cross origin",
			method:     http.MethodDelete,
			origin:     "https://attacker.example",
			csrfToken:  func(status SessionStatus) string { return status.CSRFToken },
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "session mutation rejects missing token",
			method:     http.MethodDelete,
			origin:     "http://example.com",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "session mutation rejects token from another session",
			method:     http.MethodDelete,
			origin:     "http://example.com",
			csrfToken:  func(SessionStatus) string { return "another-session-token" },
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "API key mutation bypasses browser checks without cookie",
			method:     http.MethodDelete,
			apiKey:     true,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "API key wins and bypasses browser checks with cookie",
			method:     http.MethodDelete,
			apiKey:     true,
			wantStatus: http.StatusNoContent,
		},
		{
			name:      "untrusted forwarded origin is ignored",
			method:    http.MethodDelete,
			origin:    "http://example.com",
			csrfToken: func(status SessionStatus) string { return status.CSRFToken },
			forwarded: http.Header{
				"Forwarded":         []string{"proto=https;host=public.example"},
				"X-Forwarded-Proto": []string{"https"},
				"X-Forwarded-Host":  []string{"public.example"},
			},
			wantStatus: http.StatusNoContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newSessionTestServer(t, testSessionAPIKey)
			login := performSessionRequest(t, srv, http.MethodPost, sessionLoginPath,
				[]byte(`{"api_key":"`+testSessionAPIKey+`"}`), nil, false)
			require.Equal(t, http.StatusOK, login.Code, login.Body.String())
			status := decodeSessionStatus(t, login)
			cookie := requireSessionCookie(t, login)

			headers := tt.forwarded.Clone()
			if headers == nil {
				headers = make(http.Header)
			}
			if tt.name != "API key mutation bypasses browser checks without cookie" {
				headers.Set("Cookie", cookie.String())
			}
			if tt.origin != "" {
				headers.Set("Origin", tt.origin)
			}
			if tt.csrfToken != nil {
				headers.Set(csrfHeaderName, tt.csrfToken(status))
			}
			if tt.apiKey {
				headers.Set("X-Api-Key", testSessionAPIKey)
			}

			resp := performSessionRequest(t, srv, tt.method, sessionPath, nil, headers, false)
			assert.Equal(t, tt.wantStatus, resp.Code, resp.Body.String())
		})
	}
}

func TestSessionCookieRequiresSameOrigin(t *testing.T) {
	tests := []struct {
		name            string
		corsOrigins     []string
		corsCredentials bool
		path            string
		origin          string
		useCookie       bool
		useAPIKey       bool
		wantStatus      int
		wantAllowOrigin string
		wantCredentials string
	}{
		{
			name:            "wildcard cross-origin cookie GET is refused",
			corsOrigins:     []string{"*"},
			corsCredentials: true,
			origin:          "http://attacker.example",
			useCookie:       true,
			wantStatus:      http.StatusForbidden,
			wantAllowOrigin: "*",
		},
		{
			name:            "wildcard same-site other port cookie GET is refused",
			corsOrigins:     []string{"*"},
			corsCredentials: true,
			origin:          "http://example.com:3000",
			useCookie:       true,
			wantStatus:      http.StatusForbidden,
			wantAllowOrigin: "*",
		},
		{
			name:            "wildcard cross-origin cookie session bootstrap is refused",
			corsOrigins:     []string{"*"},
			corsCredentials: true,
			path:            sessionPath,
			origin:          "http://attacker.example",
			useCookie:       true,
			wantStatus:      http.StatusForbidden,
			wantAllowOrigin: "*",
		},
		{
			name:            "wildcard same-origin cookie GET works",
			corsOrigins:     []string{"*"},
			corsCredentials: true,
			origin:          "http://example.com",
			useCookie:       true,
			wantStatus:      http.StatusOK,
			wantAllowOrigin: "*",
		},
		{
			name:            "wildcard cookie GET without Origin header works",
			corsOrigins:     []string{"*"},
			corsCredentials: true,
			useCookie:       true,
			wantStatus:      http.StatusOK,
		},
		{
			name:            "wildcard cross-origin API-key GET works without credentials header",
			corsOrigins:     []string{"*"},
			corsCredentials: true,
			origin:          "http://attacker.example",
			useAPIKey:       true,
			wantStatus:      http.StatusOK,
			wantAllowOrigin: "*",
		},
		{
			name:            "API key exempts a cross-origin request that also carries a cookie",
			corsOrigins:     []string{"*"},
			corsCredentials: true,
			origin:          "http://attacker.example",
			useCookie:       true,
			useAPIKey:       true,
			wantStatus:      http.StatusOK,
			wantAllowOrigin: "*",
		},
		{
			name:            "explicitly listed origin still cannot use cookies cross-origin",
			corsOrigins:     []string{"http://dashboard.example"},
			corsCredentials: true,
			origin:          "http://dashboard.example",
			useCookie:       true,
			wantStatus:      http.StatusForbidden,
			wantAllowOrigin: "http://dashboard.example",
			wantCredentials: "true",
		},
		{
			name:            "explicitly listed origin keeps credentialed CORS headers for API-key clients",
			corsOrigins:     []string{"http://dashboard.example"},
			corsCredentials: true,
			origin:          "http://dashboard.example",
			useAPIKey:       true,
			wantStatus:      http.StatusOK,
			wantAllowOrigin: "http://dashboard.example",
			wantCredentials: "true",
		},
		{
			name:       "no CORS config same-origin cookie GET works",
			origin:     "http://example.com",
			useCookie:  true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "no CORS config cross-origin cookie GET is refused",
			origin:     "http://attacker.example",
			useCookie:  true,
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			srv := NewServer(&config.Config{Server: config.ServerConfig{
				APIKey:          testSessionAPIKey,
				CORSOrigins:     tt.corsOrigins,
				CORSCredentials: tt.corsCredentials,
			}}, nil, nil, testLogger())
			t.Cleanup(func() {
				require.NoError(t, srv.Shutdown(context.Background()))
			})

			login := performSessionRequest(t, srv, http.MethodPost, sessionLoginPath,
				[]byte(`{"api_key":"`+testSessionAPIKey+`"}`), nil, false)
			require.Equal(t, http.StatusOK, login.Code, login.Body.String())
			cookie := requireSessionCookie(t, login)

			headers := make(http.Header)
			if tt.useCookie {
				headers.Set("Cookie", cookie.String())
			}
			if tt.useAPIKey {
				headers.Set("X-Api-Key", testSessionAPIKey)
			}
			if tt.origin != "" {
				headers.Set("Origin", tt.origin)
			}
			path := tt.path
			if path == "" {
				path = "/api/v1/health"
			}

			resp := performSessionRequest(t, srv, http.MethodGet, path, nil, headers, false)
			assertions.Equal(tt.wantStatus, resp.Code, resp.Body.String())
			assertions.Equal(tt.wantAllowOrigin, resp.Header().Get("Access-Control-Allow-Origin"),
				"Access-Control-Allow-Origin")
			assertions.Equal(tt.wantCredentials, resp.Header().Get("Access-Control-Allow-Credentials"),
				"Access-Control-Allow-Credentials")
		})
	}
}

func TestRejectedSessionMutationDoesNotReachOperationGate(t *testing.T) {
	gate := &recordingOperationGate{allow: true}
	srv := NewServerWithOptions(ServerOptions{
		Config:        &config.Config{Server: config.ServerConfig{APIKey: testSessionAPIKey}},
		Logger:        testLogger(),
		OperationGate: gate,
	})
	t.Cleanup(func() {
		require.NoError(t, srv.Shutdown(context.Background()))
	})

	login := performSessionRequest(t, srv, http.MethodPost, sessionLoginPath,
		[]byte(`{"api_key":"`+testSessionAPIKey+`"}`), nil, false)
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	status := decodeSessionStatus(t, login)
	cookie := requireSessionCookie(t, login)

	tests := []struct {
		name      string
		origin    string
		csrfToken string
	}{
		{
			name:      "rejected origin",
			origin:    "https://attacker.example",
			csrfToken: status.CSRFToken,
		},
		{
			name:      "rejected token",
			origin:    "http://example.com",
			csrfToken: "another-session-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := http.Header{
				"Cookie":       []string{cookie.String()},
				"Origin":       []string{tt.origin},
				csrfHeaderName: []string{tt.csrfToken},
			}
			resp := performSessionRequest(t, srv, http.MethodPost, "/api/v1/collections", nil, headers, false)
			assert.Equal(t, http.StatusForbidden, resp.Code, resp.Body.String())
		})
	}

	begin, done := gate.counts()
	assert.Equal(t, 0, begin, "CSRF rejection must not acquire or wait on the operation gate")
	assert.Equal(t, 0, done, "CSRF rejection must not release an operation gate it never acquired")
}

func TestTrustedProxyForwardedHTTPSDefinesBrowserOrigin(t *testing.T) {
	tests := []struct {
		name      string
		forwarded http.Header
	}{
		{
			name: "Forwarded",
			forwarded: http.Header{
				"Forwarded": []string{`for=192.0.2.20;proto=https;host=archive.example`},
			},
		},
		{
			name: "X-Forwarded",
			forwarded: http.Header{
				"X-Forwarded-Proto": []string{"https"},
				"X-Forwarded-Host":  []string{"archive.example"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			srv := newTrustedProxySessionTestServer(t)
			login := performProxyRequest(t, srv, http.MethodPost, sessionLoginPath,
				[]byte(`{"api_key":"`+testSessionAPIKey+`"}`), tt.forwarded)
			require.Equal(t, http.StatusOK, login.Code, login.Body.String())
			status := decodeSessionStatus(t, login)
			assertions.True(status.HTTPS)
			assertions.True(requireSessionCookie(t, login).Secure)

			headers := tt.forwarded.Clone()
			headers.Set("Cookie", requireSessionCookie(t, login).String())
			headers.Set("Origin", "https://archive.example")
			headers.Set(csrfHeaderName, status.CSRFToken)
			logout := performProxyRequest(t, srv, http.MethodDelete, sessionPath, nil, headers)
			assertions.Equal(http.StatusNoContent, logout.Code, logout.Body.String())
		})
	}
}

func TestTrustedProxyRejectsAmbiguousForwardedSchemeOrHost(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
	}{
		{
			name: "multiple Forwarded elements",
			headers: http.Header{
				"Forwarded": []string{"proto=https;host=one.example, proto=https;host=two.example"},
			},
		},
		{
			name: "multiple X-Forwarded schemes",
			headers: http.Header{
				"X-Forwarded-Proto": []string{"https", "http"},
			},
		},
		{
			name: "comma-separated X-Forwarded hosts",
			headers: http.Header{
				"X-Forwarded-Host": []string{"one.example, two.example"},
			},
		},
		{
			name: "mixed forwarding standards",
			headers: http.Header{
				"Forwarded":         []string{"proto=https;host=one.example"},
				"X-Forwarded-Proto": []string{"https"},
				"X-Forwarded-Host":  []string{"one.example"},
			},
		},
		{
			name: "empty Forwarded scheme",
			headers: http.Header{
				"Forwarded": []string{"proto=;host=archive.example"},
			},
		},
		{
			name: "empty Forwarded host",
			headers: http.Header{
				"Forwarded": []string{"proto=https;host="},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTrustedProxySessionTestServer(t)
			resp := performProxyRequest(t, srv, http.MethodGet, sessionPath, nil, tt.headers)
			assert.Equal(t, http.StatusBadRequest, resp.Code, resp.Body.String())
		})
	}
}

func TestMalformedForwardedLoginRemainsNoStoreAndRateLimited(t *testing.T) {
	assertions := assert.New(t)
	srv := newTrustedProxySessionTestServer(t)
	srv.rateLimiter.rate = 0
	srv.rateLimiter.burst = 1
	headers := http.Header{
		"Forwarded": []string{"proto=https;host=one.example, proto=https;host=two.example"},
	}
	body := []byte(`{"api_key":"` + testSessionAPIKey + `"}`)

	first := performProxyRequest(t, srv, http.MethodPost, sessionLoginPath, body, headers)
	assertions.Equal(http.StatusBadRequest, first.Code, first.Body.String())
	assertions.Equal("no-store", first.Header().Get("Cache-Control"))

	second := performProxyRequest(t, srv, http.MethodPost, sessionLoginPath, body, headers)
	assertions.Equal(http.StatusTooManyRequests, second.Code, second.Body.String())
	assertions.Equal("no-store", second.Header().Get("Cache-Control"))
}

func newTrustedProxySessionTestServer(t *testing.T) *Server {
	t.Helper()
	srv := NewServer(&config.Config{Server: config.ServerConfig{
		APIKey:         testSessionAPIKey,
		TrustedProxies: []string{"127.0.0.1/32"},
	}}, nil, nil, testLogger())
	t.Cleanup(func() {
		require.NoError(t, srv.Shutdown(context.Background()))
	})
	return srv
}

func performProxyRequest(
	t *testing.T,
	srv *Server,
	method string,
	path string,
	body []byte,
	headers http.Header,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:4242"
	req.Host = "proxy.internal:8080"
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	if req.URL.Scheme == "https" {
		req.TLS = &tls.ConnectionState{}
	}
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	return resp
}
