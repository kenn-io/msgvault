package gmail

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// deadlineTransport replaces the external Gmail service, leaving the client's
// rate limiter, retry loop and context handling intact.
type deadlineTransport func(*http.Request) (*http.Response, error)

func (f deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newDeadlineClient(t *testing.T, transport deadlineTransport) *Client {
	t.Helper()
	c := NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"}))
	// Install the fake as the base of the production oauth2.Transport instead
	// of replacing the transport, so the deadline budget is measured across
	// the real OAuth round trip. The static token never expires, so token
	// acquisition stays out of the request path; bounding a stalled token
	// refresh is a separate OAuth concern (TokenSource.Token() is contextless).
	oauthTransport, ok := c.httpClient.Transport.(*oauth2.Transport)
	require.True(t, ok, "NewClient must install an oauth2.Transport")
	oauthTransport.Base = transport
	return c
}

func deadlineResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestGetProfileRetryDeadline(t *testing.T) {
	for _, tc := range []struct {
		name                string
		callerTimeout, want time.Duration
	}{
		{"no caller deadline", 0, 30 * time.Second},
		{"later caller deadline", time.Minute, 30 * time.Second},
		{"earlier caller deadline", 5 * time.Second, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				ctx := context.Background()
				if tc.callerTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.callerTimeout)
					defer cancel()
				}
				start := time.Now()
				requests := 0
				c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
					requests++
					deadline, ok := r.Context().Deadline()
					require.True(ok, "HTTP request needs an overall deadline")
					assert.Equal(start.Add(tc.want), deadline, "retries must not restart the budget")
					assert.Equal("Bearer test-token", r.Header.Get("Authorization"),
						"requests must carry the OAuth token through the production transport")
					// Even zero jitter cannot exhaust all retries before the deadline.
					select {
					case <-r.Context().Done():
						return nil, r.Context().Err()
					case <-time.After(3 * time.Second):
					}
					return deadlineResponse(http.StatusTooManyRequests, `{"error":{"code":429}}`), nil
				})
				profile, err := c.GetProfile(ctx)
				require.ErrorIs(err, context.DeadlineExceeded)
				assert.Nil(profile)
				assert.Equal(tc.want, time.Since(start))
				assert.Greater(requests, 1, "exercise retries before the deadline")
			})
		})
	}
}

func TestGetProfileCallerDeadlineBoundsRateLimitWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		requests := 0
		c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
			requests++
			return deadlineResponse(http.StatusOK, `{}`), nil
		})
		c.rateLimiter.Throttle(2 * time.Minute)
		start := time.Now()
		_, err := c.GetProfile(ctx)
		require.ErrorIs(err, context.DeadlineExceeded)
		assert.Equal(time.Minute, time.Since(start))
		assert.Zero(requests)
	})
}

func TestGetProfileDeadlineStartsAfterRateLimitWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		var httpStart time.Time
		c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
			httpStart = time.Now()
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		c.rateLimiter.Throttle(time.Minute)
		start := time.Now()
		_, err := c.GetProfile(context.Background())
		require.ErrorIs(err, context.DeadlineExceeded)
		require.False(httpStart.IsZero(), "request must get through the quota wait")
		assert.GreaterOrEqual(httpStart.Sub(start), time.Minute)
		assert.Equal(30*time.Second, time.Since(httpStart))
	})
}

func TestListMessagesAfterRawFetchQuotaRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		throttle time.Duration
	}{
		{"403 quota", http.StatusForbidden, time.Minute},
		{"429 rate limit", http.StatusTooManyRequests, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				rawRequests := 0
				c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case "/gmail/v1/users/me/messages/msg-1":
						rawRequests++
						if rawRequests == 1 {
							return deadlineResponse(tc.status,
								string(NewGmailError(tc.status).WithReason(reasonRateLimitExceeded).Build())), nil
						}
						return deadlineResponse(http.StatusOK, `{"id":"msg-1","raw":"dGVzdA"}`), nil
					case "/gmail/v1/users/me/messages":
						// A quota wait must leave time for the next page to arrive.
						select {
						case <-r.Context().Done():
							return nil, r.Context().Err()
						case <-time.After(5 * time.Second):
						}
						return deadlineResponse(http.StatusOK, `{"messages":[{"id":"msg-2"}]}`), nil
					default:
						return deadlineResponse(http.StatusNotFound, ""), nil
					}
				})
				start := time.Now()
				message, err := c.GetMessageRaw(context.Background(), "msg-1")
				require.NoError(err)
				assert.Equal([]byte("test"), message.Raw)

				page, err := c.ListMessages(context.Background(), "", "")
				require.NoError(err)
				require.Len(page.Messages, 1)
				assert.Equal("msg-2", page.Messages[0].ID)
				assert.GreaterOrEqual(time.Since(start), tc.throttle+5*time.Second,
					"preserve the quota pause before fetching the next page")
			})
		})
	}
}

type deadlineBody struct {
	ctx    context.Context
	closed bool
}

func (b *deadlineBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b *deadlineBody) Close() error             { b.closed = true; return nil }

func TestGetProfileDeadlineInterruptsHTTP(t *testing.T) {
	for _, bodyRead := range []bool{false, true} {
		name := "transport"
		if bodyRead {
			name = "body read"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				var body *deadlineBody
				c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
					if bodyRead {
						body = &deadlineBody{ctx: r.Context()}
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
					}
					<-r.Context().Done()
					return nil, r.Context().Err()
				})
				start := time.Now()
				_, err := c.GetProfile(ctx)
				require.ErrorIs(err, context.DeadlineExceeded)
				assert.Equal(30*time.Second, time.Since(start))
				if bodyRead {
					require.NotNil(body)
					assert.True(body.closed)
				}
			})
		})
	}
}

type retrySignalBody struct {
	io.Reader

	closed chan struct{}
}

func (b *retrySignalBody) Close() error { b.closed <- struct{}{}; return nil }

func TestGetProfileCancellationInterruptsRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		closed := make(chan struct{}, maxRetries+1)
		c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: &retrySignalBody{Reader: strings.NewReader(""), closed: closed}}, nil
		})
		result := make(chan error, 1)
		go func() { _, err := c.GetProfile(ctx); result <- err }()
		<-closed
		synctest.Wait()
		start := time.Now()
		cancel()
		require.ErrorIs(<-result, context.Canceled)
		assert.Zero(time.Since(start))
	})
}

func TestGetProfileRecoversWithinDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		requests := 0
		c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
			requests++
			if requests == 1 {
				return deadlineResponse(http.StatusTooManyRequests, ""), nil
			}
			return deadlineResponse(http.StatusOK, `{"emailAddress":"user@example.com","historyId":"42"}`), nil
		})
		start := time.Now()
		profile, err := c.GetProfile(context.Background())
		require.NoError(err)
		require.NotNil(profile)
		assert.Equal("user@example.com", profile.EmailAddress)
		assert.Equal(uint64(42), profile.HistoryID)
		assert.Equal(2, requests)
		assert.Less(time.Since(start), 30*time.Second)
	})
}

// delayedMessageBody models a raw MIME download whose transfer outlasts a
// metadata request, while still observing request cancellation.
type delayedMessageBody struct {
	io.Reader

	ctx   context.Context
	delay time.Duration
}

func (b *delayedMessageBody) Read(p []byte) (int, error) {
	if b.delay > 0 {
		select {
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		case <-time.After(b.delay):
			b.delay = 0
		}
	}
	return b.Reader.Read(p)
}

func (b *delayedMessageBody) Close() error { return nil }

func TestGetMessageRawTransferDeadline(t *testing.T) {
	for _, tc := range []struct {
		name          string
		transfer      time.Duration
		callerTimeout time.Duration
		wantElapsed   time.Duration
		wantError     bool
	}{
		{"slow download completes", 2 * time.Minute, 0, 2 * time.Minute, false},
		{"stalled download is bounded", 6 * time.Minute, 0, 5 * time.Minute, true},
		{"caller deadline wins", 2 * time.Minute, 5 * time.Second, 5 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				ctx := context.Background()
				if tc.callerTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.callerTimeout)
					defer cancel()
				}
				c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body: &delayedMessageBody{
							ctx: r.Context(), delay: tc.transfer,
							Reader: strings.NewReader(`{"id":"msg-1","raw":"dGVzdA"}`),
						},
					}, nil
				})
				start := time.Now()
				message, err := c.GetMessageRaw(ctx, "msg-1")
				if tc.wantError {
					require.ErrorIs(err, context.DeadlineExceeded)
					assert.Nil(message)
				} else {
					require.NoError(err)
					require.NotNil(message)
					assert.Equal([]byte("test"), message.Raw)
				}
				assert.Equal(tc.wantElapsed, time.Since(start))
			})
		})
	}
}
