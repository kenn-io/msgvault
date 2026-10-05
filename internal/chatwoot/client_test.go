package chatwoot

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func TestClientListsEveryConversationUnderInstancePath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/support/api/v1/accounts/9/conversations", r.URL.Path)
		query := r.URL.Query()
		assert.Equal("all", query.Get("status"))
		assert.Equal("all", query.Get("assignee_type"))
		assert.Equal("7", query.Get("inbox_id"))
		_, _ = io.WriteString(w, `{"data":{"payload":[{"id":42,"account_id":9,"inbox_id":7}]}}`)
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL+"/support/", 9, "synthetic-token")
	require.NoError(err)
	c.limiter = rate.NewLimiter(rate.Inf, 1)
	conversations, err := c.ListConversations(t.Context(), 2, 7, sortByCreated)
	require.NoError(err)
	require.Len(conversations, 1)
	assert.Equal(int64(42), conversations[0].ID)
}

func TestClientRequiresHTTPSOutsideLoopback(t *testing.T) {
	for _, raw := range []string{
		"http://chatwoot.example.com",
		"http://192.0.2.10/support",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := NewClient(raw, 9, "synthetic-token")
			require.Error(t, err, "remote API tokens must not be sent over HTTP")
		})
	}
	for _, raw := range []string{
		"http://localhost:3000/support",
		"http://127.0.0.1:3000/support",
		"http://[::1]:3000/support",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := NewClient(raw, 9, "synthetic-token")
			require.NoError(t, err, "local Chatwoot development instances may use loopback HTTP")
		})
	}
}

func TestClientMediaAndAPIRedirectIsolation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(r.Header.Get("Api_access_token"))
		assert.Empty(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "audio/ogg")
		_, _ = io.WriteString(w, "synthetic audio")
	}))
	defer media.Close()
	mediaHits := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/audio" {
			mediaHits++
			assert.Empty(r.Header.Get("Api_access_token"))
			assert.Empty(r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "audio/ogg")
			_, _ = io.WriteString(w, "synthetic audio")
			return
		}
		http.Redirect(w, r, media.URL+"/audio?signature=secret", http.StatusFound)
	}))
	defer api.Close()
	c, err := NewClient(api.URL, 9, "synthetic-token")
	require.NoError(err)
	c.limiter = rate.NewLimiter(rate.Inf, 1)
	_, err = c.ListInboxes(t.Context())
	require.Error(err, "authenticated API requests must not redirect")
	assert.NotContains(err.Error(), "signature")
	assert.Zero(mediaHits, "authenticated API redirects must not fetch the target")
	reader, size, mime, err := c.OpenMedia(t.Context(), api.URL+"/audio", 1<<20)
	require.NoError(err)
	defer func() { require.NoError(reader.Close()) }()
	body, err := io.ReadAll(reader)
	require.NoError(err)
	assert.Equal("synthetic audio", string(body))
	assert.Equal(int64(len(body)), size)
	assert.Equal("audio/ogg", mime)
	invalidReader, _, _, err := c.OpenMedia(t.Context(), "file:///etc/passwd", 1<<20)
	require.Error(err)
	assert.Nil(invalidReader)
}

func TestClientRejectsUntrustedPrivateMediaHost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	hits := 0
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = io.WriteString(w, "synthetic internal response")
	}))
	defer media.Close()

	c, err := NewClient("https://chatwoot.example.com", 9, "synthetic-token")
	require.NoError(err)
	body, _, _, err := c.OpenMedia(t.Context(), media.URL+"/internal", 1<<20)
	if body != nil {
		require.NoError(body.Close())
	}
	require.Error(err, "media URLs from message data must not reach an untrusted private destination")
	assert.Zero(hits)
}

func TestClientRejectsPrivateMediaRedirect(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	privateHits := 0
	privateServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateHits++
		_, _ = io.WriteString(w, "synthetic media response")
	}))
	defer privateServer.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, privateServer.URL+"/internal", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "synthetic media response")
	}))
	defer server.Close()
	baseURL := server.URL
	c, err := NewClient(baseURL, 9, "synthetic-token")
	require.NoError(err)
	body, _, _, err := c.OpenMedia(t.Context(), baseURL+"/start", 1<<20)
	if body != nil {
		require.NoError(body.Close())
	}
	require.Error(err, "every redirect target must pass the destination policy")
	assert.Zero(privateHits)
}

func TestClientRetriesRateLimitAndReportsNotFound(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, 9, "token")
	require.NoError(t, err)
	c.limiter = rate.NewLimiter(rate.Inf, 1)
	_, err = c.ListInboxes(t.Context())
	require.ErrorIs(t, err, ErrNotFound, "deleted conversations retire their saved work")
	assert.Equal(t, 2, attempts)
}
