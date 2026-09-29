package msgraph

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
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

func TestGetGraphErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"expired_404", http.StatusNotFound, `{"error":{"code":"syncStateNotFound"}}`, ErrGone},
		{"expired_400", http.StatusBadRequest, `{"error":{"code":"syncStateNotFound"}}`, ErrGone},
		{"gone", http.StatusGone, "expired", ErrGone},
		{"missing", http.StatusNotFound, `{"error":{"code":"ErrorItemNotFound","message":"syncStateNotFound is not the error code"}}`, ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 1000)
			_, err := c.GetRaw(t.Context(), "/message")
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestGetStopsAfterLastAttempt(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(map[bool]string{false: "503", true: "truncated_body"}[truncated], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					if truncated {
						w.Header().Set("Content-Length", "100")
						_, _ = w.Write([]byte("partial"))
					} else {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
				}))
				httpClient := srv.Client()
				c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 1000)
				c.http.Transport = httpClient.Transport
				start := time.Now()
				_, err := c.GetRaw(t.Context(), "/message")
				require.Error(t, err)
				assert.Equal(t, 8, calls)
				assert.Equal(t, 123*time.Second, time.Since(start))
			})
		})
	}
}

// Dial errors are injected at the network boundary to exercise our retry
// classification without depending on a real DNS resolver or certificate store.
func TestGetConnectionErrorRetries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		attempts int
		delay    time.Duration
	}{
		{"name_not_found", &net.DNSError{Err: "no such host", Name: "mail.example.com", IsNotFound: true}, 1, 0},
		{"certificate", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, 1, 0},
		{"temporary_dns", &net.DNSError{Err: "temporary lookup failure", Name: "mail.example.com", IsTemporary: true}, 8, 123 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				c := NewClient("https://mail.example.com", func(context.Context) (string, error) { return "t", nil }, 1000)
				c.http.Transport = &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { calls++; return nil, tc.err }}
				start := time.Now()
				_, err := c.GetRaw(t.Context(), "/message")
				require.ErrorIs(t, err, tc.err)
				assert.Equal(t, tc.attempts, calls)
				assert.Equal(t, tc.delay, time.Since(start))
			})
		})
	}
}

func TestGetRawWithTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			assert.NoError(http.NewResponseController(w).Flush())
			select {
			case <-time.After(2 * time.Minute):
				_, _ = w.Write([]byte("complete MIME"))
			case <-r.Context().Done():
			}
		}))
		httpClient := srv.Client()
		c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 1000)
		c.http.Transport = httpClient.Transport
		body, err := c.GetRawWithTimeout(t.Context(), "/message", 10*time.Minute)
		require.NoError(err)
		assert.Equal("complete MIME", string(body))

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, err = c.GetRawWithTimeout(ctx, "/message", 10*time.Minute)
		require.ErrorIs(err, context.DeadlineExceeded)

		// An ordinary request must still hit its original 60-second deadline.
		start := time.Now()
		_, err = c.GetRaw(t.Context(), "/metadata")
		require.Error(err)
		assert.Equal(603*time.Second, time.Since(start))
	})
}
