package msgraph

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A connection that breaks in the middle of a body is retried, like a 5xx.
func TestGetRetriesTruncatedBody(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("partial"))
			return // the server closes the connection 93 bytes short
		}
		_, _ = w.Write([]byte("full body"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 1000)
	body, err := c.GetRaw(context.Background(), "/me/messages/m1/$value")
	require.NoError(t, err)
	assert.Equal(t, "full body", string(body))
	assert.EqualValues(t, 2, calls.Load())
}

func TestClientContextCancelDuringRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30") // long wait so cancellation wins
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		httpClient := server.Client()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c := NewClient(server.URL, func(context.Context) (string, error) { return "t", nil }, 50)
		c.http.Transport = httpClient.Transport
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		_, err := c.GetRaw(ctx, "/x")
		require.ErrorIs(t, err, context.Canceled)
	})
}
