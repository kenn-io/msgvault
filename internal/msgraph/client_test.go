package msgraph

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
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

// GetRawMetered charges every response it reads, failed and broken ones
// included, and an error from charge ends the request.
func TestGetRawMeteredChargesEveryAttempt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	errBody := `{"error":{"code":"serviceUnavailable"}}`
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(errBody))
		case 2:
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte("partial"))
		default:
			_, _ = w.Write([]byte("full body"))
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 1000)

	var charged []int64
	body, err := c.GetRawMetered(t.Context(), "/x", 1<<20, func(n int64) error {
		charged = append(charged, n)
		return nil
	})
	require.NoError(err)
	assert.Equal("full body", string(body))
	assert.Equal([]int64{int64(len(errBody)), int64(len("partial")), int64(len("full body"))}, charged)

	calls.Store(0)
	limit := errors.New("limit")
	charges := 0
	_, err = c.GetRawMetered(t.Context(), "/x", 1<<20, func(int64) error {
		charges++
		return limit
	})
	require.ErrorIs(err, limit)
	assert.Equal(1, charges)
	assert.EqualValues(1, calls.Load())
}

// A request cancelled while it waits out a 429 stops. A write was never
// applied, so it is marked not sent.
func TestClientContextCancelDuringRetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(context.Context, *Client) error
		want error
	}{
		{
			name: "GetRaw",
			call: func(ctx context.Context, c *Client) error { _, err := c.GetRaw(ctx, "/x"); return err },
			want: context.Canceled,
		},
		{
			name: "SendOnce",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.SendOnce(ctx, http.MethodPatch, "/me/contacts/a", map[string]string{}, `W/"1"`)
				return err
			},
			want: ErrNotSent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
				require.ErrorIs(t, tc.call(ctx, c), tc.want)
			})
		})
	}
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
		{"unauthorized", http.StatusUnauthorized, `{"error":{"code":"InvalidAuthenticationToken"}}`, ErrUnauthorized},
		{"stale_etag", http.StatusPreconditionFailed, `{"error":{"code":"ErrorIrresolvableConflict"}}`, ErrPreconditionFailed},
		{"bad_change_key", http.StatusBadRequest, `{"error":{"code":"ErrorInvalidChangeKey"}}`, ErrPreconditionFailed},
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

// A POST retries a 429 after Retry-After, resends the same JSON body, and
// accepts any 2xx.
func TestPostRetriesAndAcceptsAny2xx(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var methods, bodies, types []string
				srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					b, _ := io.ReadAll(r.Body)
					methods = append(methods, r.Method)
					bodies = append(bodies, string(b))
					types = append(types, r.Header.Get("Content-Type"))
					if len(methods) == 1 {
						w.Header().Set("Retry-After", "2")
						w.WriteHeader(http.StatusTooManyRequests)
						return
					}
					w.WriteHeader(status)
				}))
				httpClient := srv.Client()
				c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 1000)
				c.http.Transport = httpClient.Transport
				start := time.Now()
				err := c.Post(t.Context(), "/me/messages/m1/move", map[string]string{"destinationId": "deleteditems"})
				require.NoError(t, err)
				assert.Equal(t, 2*time.Second, time.Since(start))
				assert.Equal(t, []string{"POST", "POST"}, methods)
				want := `{"destinationId":"deleteditems"}`
				assert.Equal(t, []string{want, want}, bodies)
				assert.Equal(t, []string{"application/json", "application/json"}, types)
			})
		})
	}
}

func TestPostErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   error
	}{
		{"missing", http.StatusNotFound, ErrNotFound},
		{"forbidden", http.StatusForbidden, ErrForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 1000)
			err := c.Post(t.Context(), "/me/messages/m1/permanentDelete", nil)
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

// A 429 that outlasts every retry, or whose wait outlasts the caller's
// deadline, returns at once with Graph's last Retry-After, so a caller can
// pause instead of repeating the request or seeing a cancellation.
func TestGetReportsThrottling(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryAfter string
		deadline   time.Duration
		want       time.Duration
	}{
		{name: "retries exhausted", retryAfter: "30", want: 30 * time.Second},
		{name: "wait outlasts deadline", retryAfter: "600", deadline: 5 * time.Minute, want: 600 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Retry-After", tc.retryAfter)
					w.WriteHeader(http.StatusTooManyRequests)
				}))
				httpClient := srv.Client()
				c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 1000)
				c.http.Transport = httpClient.Transport
				ctx := t.Context()
				if tc.deadline > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.deadline)
					defer cancel()
				}
				_, err := c.GetRaw(ctx, "/message")
				throttled, ok := errors.AsType[*ThrottledError](err)
				require.True(t, ok, "error: %v", err)
				assert.Equal(t, tc.want, throttled.RetryAfter)
				assert.NoError(t, ctx.Err())
			})
		})
	}
}

// SendOnce does not repeat a write after a 5xx, which can follow an applied
// write, but it retries a 429, which Graph applied nothing for.
func TestSendOnceRetriesOnlyThrottling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		var statuses []int
		calls := 0
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			status := statuses[calls]
			calls++
			w.WriteHeader(status)
		}))
		httpClient := srv.Client()
		c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 1000)
		c.http.Transport = httpClient.Transport

		statuses, calls = []int{http.StatusServiceUnavailable, http.StatusCreated}, 0
		_, err := c.SendOnce(t.Context(), http.MethodPost, "/contacts", map[string]string{}, "")
		require.Error(err)
		assert.Equal(1, calls)

		statuses, calls = []int{http.StatusTooManyRequests, http.StatusCreated}, 0
		_, err = c.SendOnce(t.Context(), http.MethodPost, "/contacts", map[string]string{}, "")
		require.NoError(err)
		assert.Equal(2, calls)
	})
}

func TestSendHeldByRateLimitIsMarkedNotSent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests := 0
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			w.WriteHeader(http.StatusNoContent)
		}))
		transport := srv.Client().Transport
		c := NewClient(srv.URL, func(context.Context) (string, error) { return "t", nil }, 0.001)
		c.http.Transport = transport
		_, err := c.SendOnce(t.Context(), http.MethodDelete, "/me/contacts/a", nil, "")
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		_, err = c.SendOnce(ctx, http.MethodDelete, "/me/contacts/b", nil, "")
		require.ErrorIs(t, err, ErrNotSent)
		assert.Equal(t, 1, requests)
	})
}
