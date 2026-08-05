package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func newChangeFeedGuardTestServer(
	t *testing.T, apiKey string,
) (*Server, *stubChangedMessageLister) {
	t.Helper()
	lister := &stubChangedMessageLister{mockStore: &mockStore{}}
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{
			APIPort: 8080,
			APIKey:  apiKey,
		}},
		Store:  lister,
		Logger: testLogger(),
	})
	t.Cleanup(func() {
		srv.rateLimiter.Close()
		srv.changesRateLimiter.Close()
	})
	return srv, lister
}

func requestChangeFeed(
	srv *Server, origin string, fetchSites []string, apiKey string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/changes?limit=1", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for _, fetchSite := range fetchSites {
		req.Header.Add("Sec-Fetch-Site", fetchSite)
	}
	if apiKey != "" {
		req.Header.Set("X-Api-Key", apiKey)
	}
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	return w
}

func TestChangeFeedGuard_KeylessBrowserOrigin(t *testing.T) {
	tests := []struct {
		name       string
		origin     string
		fetchSites []string
		wantStatus int
		wantCalls  int
	}{
		{"cross-origin Origin", "https://evil.example", nil, http.StatusForbidden, 0},
		{"cross-site metadata", "", []string{"cross-site"}, http.StatusForbidden, 0},
		{"same-site metadata", "", []string{"same-site"}, http.StatusForbidden, 0},
		{"unknown metadata", "", []string{"unexpected"}, http.StatusForbidden, 0},
		{"multiple metadata values", "", []string{"same-origin", "cross-site"}, http.StatusForbidden, 0},
		{"same origin", "http://example.com", nil, http.StatusOK, 1},
		{"same-origin metadata", "", []string{"same-origin"}, http.StatusOK, 1},
		{"navigation metadata", "", []string{"none"}, http.StatusOK, 1},
		{"headerless client", "", nil, http.StatusOK, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, lister := newChangeFeedGuardTestServer(t, "")

			w := requestChangeFeed(srv, tt.origin, tt.fetchSites, "")

			require.Equal(t, tt.wantStatus, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, tt.wantCalls, lister.calls,
				"a rejected browser request must not reach the lock-taking store path")
			if tt.wantStatus == http.StatusForbidden {
				assert.Equal(t, "cross_origin_loopback", decodeError(t, w).Error)
			}
		})
	}
}

func TestChangeFeedGuard_APIKeyAllowsCrossOrigin(t *testing.T) {
	const apiKey = "change-feed-test-key"
	srv, lister := newChangeFeedGuardTestServer(t, apiKey)

	w := requestChangeFeed(srv, "https://dashboard.example", []string{"cross-site"}, apiKey)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, 1, lister.calls, "an explicit API key is not an ambient browser credential")
}

func TestChangeFeedGuard_LoopbackCannotBypassRateLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv, lister := newChangeFeedGuardTestServer(t, "")

	for requestNumber := 1; requestNumber <= 4; requestNumber++ {
		w := requestChangeFeed(srv, "", nil, "")
		require.Equalf(http.StatusOK, w.Code, "request %d body: %s", requestNumber, w.Body.String())
	}

	w := requestChangeFeed(srv, "", nil, "")
	require.Equal(http.StatusTooManyRequests, w.Code, "body: %s", w.Body.String())
	assert.NotEmpty(w.Header().Get("Retry-After"))
	assert.Equal(4, lister.calls,
		"the rejected request must not reach the lock-taking store path")
}
