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

// Remote instances must use HTTPS so the API token never crosses the network
// in clear text; loopback development instances may use HTTP.
func TestCanonicalURL(t *testing.T) {
	for raw, want := range map[string]string{
		"HTTPS://CHATWOOT.example.com:443/support/": "https://chatwoot.example.com/support",
		"http://localhost:3000/support":             "http://localhost:3000/support",
		"http://127.0.0.1:3000/support":             "http://127.0.0.1:3000/support",
		"http://[::1]:3000/support":                 "http://[::1]:3000/support",
		"http://chatwoot.example.com":               "",
		"http://192.0.2.10/support":                 "",
		"chatwoot.example.com":                      "",
		"https://secret@chatwoot.example.com":       "",
		"https://chatwoot.example.com?token=secret": "",
		"https://chatwoot.example.com#secret":       "",
	} {
		t.Run(raw, func(t *testing.T) {
			got, err := CanonicalURL(raw)
			if want == "" {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "secret")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestClientAPIDoesNotFollowRedirects(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	targetHits := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits++ }))
	defer target.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/inboxes", http.StatusFound)
	}))
	defer api.Close()
	c, err := NewClient(api.URL, 9, "synthetic-token")
	require.NoError(err)
	c.limiter = rate.NewLimiter(rate.Inf, 1)
	_, err = c.ListInboxes(t.Context())
	require.Error(err, "authenticated API requests must not redirect")
	assert.Zero(targetHits, "a redirect must not carry the API token to its target")
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
