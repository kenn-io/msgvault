package gmail

import (
	"context"
	"errors"
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
					return deadlineResponse(http.StatusServiceUnavailable, `{"error":{"code":503}}`), nil
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

func TestReadRequestsRecoverAfterQuotaPause(t *testing.T) {
	for _, op := range []struct {
		name string
		call func(*Client, context.Context) error
	}{
		{"profile", func(c *Client, ctx context.Context) error { _, err := c.GetProfile(ctx); return err }},
		{"labels", func(c *Client, ctx context.Context) error { _, err := c.ListLabels(ctx); return err }},
		{"messages", func(c *Client, ctx context.Context) error { _, err := c.ListMessages(ctx, "", "page-2"); return err }},
		{"snapshot", func(c *Client, ctx context.Context) error {
			_, err := c.ListCompleteMessageSnapshot(ctx, "page-2")
			return err
		}},
		{"history", func(c *Client, ctx context.Context) error { _, err := c.ListHistory(ctx, 42, "page-2"); return err }},
	} {
		for _, quota := range []struct {
			status int
			pause  time.Duration
		}{
			{http.StatusForbidden, time.Minute},
			{http.StatusTooManyRequests, 30 * time.Second},
		} {
			t.Run(op.name+"/"+http.StatusText(quota.status), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					start := time.Now()
					requests := 0
					var firstURL string
					c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
						requests++
						if requests == 1 {
							firstURL = r.URL.String()
							return deadlineResponse(quota.status, `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`), nil
						}
						assert.Equal(t, firstURL, r.URL.String(), "retry the same page and query")
						assert.GreaterOrEqual(t, time.Since(start), quota.pause, "do not send requests during the quota pause")
						deadline, ok := r.Context().Deadline()
						require.True(t, ok)
						assert.Equal(t, 30*time.Second, time.Until(deadline), "start a fresh budget after the pause")
						select {
						case <-r.Context().Done():
							return nil, r.Context().Err()
						case <-time.After(20 * time.Second):
						}
						return deadlineResponse(http.StatusOK, `{}`), nil
					})
					require.NoError(t, op.call(c, context.Background()))
					assert.Equal(t, 2, requests)
				})
			})
		}
	}
}

func TestGetProfileHonorsRetryAfter(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		for _, tc := range []struct {
			name, header string
			httpDate     bool
			pause        time.Duration
		}{
			{name: "seconds", header: "1800", pause: 30 * time.Minute},
			{name: "HTTP date", httpDate: true, pause: 30 * time.Minute},
			{name: "short", header: "1"},
			{name: "invalid", header: "invalid"},
		} {
			t.Run(http.StatusText(status)+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					start := time.Now()
					pause := 30 * time.Second
					if status == http.StatusForbidden {
						pause = time.Minute
					}
					pause = max(pause, tc.pause)
					requests := 0
					c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
						requests++
						if requests == 1 {
							resp := deadlineResponse(status, `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`)
							resp.Header.Set("Retry-After", tc.header)
							if tc.httpDate {
								resp.Header.Set("Retry-After", start.Add(tc.pause).UTC().Format(http.TimeFormat))
							}
							return resp, nil
						}
						assert.GreaterOrEqual(t, time.Since(start), pause, "wait for both Retry-After and the minimum quota pause")
						return deadlineResponse(http.StatusOK, `{}`), nil
					})
					_, err := c.GetProfile(context.Background())
					require.NoError(t, err)
					assert.Equal(t, 2, requests)
				})
			})
		}
	}
}

func TestGetProfileCallerDeadlineInterruptsRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		requests := 0
		c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
			requests++
			resp := deadlineResponse(http.StatusTooManyRequests, `{}`)
			resp.Header.Set("Retry-After", "600")
			return resp, nil
		})
		start := time.Now()
		_, err := c.GetProfile(ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, time.Minute, time.Since(start))
		assert.Equal(t, 1, requests, "the caller deadline must stop the wait before another request")
	})
}

func TestGetProfileQuotaRetriesAreBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		requests := 0
		var lastRequest time.Time
		c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
			requests++
			if !lastRequest.IsZero() {
				assert.GreaterOrEqual(time.Since(lastRequest), 30*time.Second)
			}
			lastRequest = time.Now()
			return deadlineResponse(http.StatusTooManyRequests, `{"error":{"message":"Too many concurrent requests"}}`), nil
		})
		_, err := c.GetProfile(context.Background())
		require.Error(err)
		throttled, ok := errors.AsType[*ThrottledError](err)
		require.True(ok)
		assert.Equal("Too many concurrent requests", throttled.Detail)
		assert.Equal(6, requests, "one attempt and five quota retries")
		assert.NotErrorIs(err, context.DeadlineExceeded, "quota exhaustion is not a context deadline")
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

func TestGetDraftAllowsSlowRawTransfer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
			assert.Equal(t, "/gmail/v1/users/me/drafts/draft-1", r.URL.Path)
			assert.Equal(t, "raw", r.URL.Query().Get("format"))
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: &delayedMessageBody{
					ctx: r.Context(), delay: 2 * time.Minute,
					Reader: strings.NewReader(`{"id":"draft-1","message":{"id":"msg-1","threadId":"thread-1","raw":"dGVzdA"}}`),
				},
			}, nil
		})
		draft, err := client.GetDraft(t.Context(), "draft-1")
		require.NoError(t, err)
		assert.Equal(t, []byte("test"), draft.Message.Raw)
	})
}

func TestListMessagesDeadlineReportsLastResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		requests := 0
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
			requests++
			// The caller's deadline expires during the following quota pause.
			select {
			case <-r.Context().Done():
				return nil, r.Context().Err()
			case <-time.After(3 * time.Second):
			}
			resp := deadlineResponse(http.StatusTooManyRequests,
				`{"error":{"code":429,"message":"User-rate limit exceeded. Retry after 2026-09-26T22:23:39Z",`+
					`"errors":[{"reason":"rateLimitExceeded","domain":"usageLimits"}],"status":"RESOURCE_EXHAUSTED"}}`)
			resp.Header.Set("Retry-After", "7")
			return resp, nil
		})
		list, err := c.ListMessages(ctx, "", "")
		require.ErrorIs(err, context.DeadlineExceeded, "the deadline must stay matchable")
		assert.Nil(list)
		assert.Equal(1, requests, "caller deadline interrupts the quota pause before another request")
		_, throttled := errors.AsType[*ThrottledError](err)
		assert.True(throttled, "callers must be able to recognise the throttle")
		assert.Contains(err.Error(), "context deadline exceeded after ")
		assert.Contains(err.Error(), "last response: rate limited (429): rateLimitExceeded; "+
			"User-rate limit exceeded. Retry after 2026-09-26T22:23:39Z; Retry-After 7")
	})
}

func TestGetProfileBodyDeadlinePreservesLastResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests := 0
		c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
			requests++
			if requests == 1 {
				return deadlineResponse(http.StatusServiceUnavailable, ""), nil
			}
			return &http.Response{StatusCode: http.StatusOK, Body: &deadlineBody{ctx: r.Context()}}, nil
		})
		_, err := c.GetProfile(context.Background())
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Contains(t, err.Error(), "last response: server error (503)")
		assert.Equal(t, 2, requests)
	})
}

func TestGetProfileRetryHonorsSharedThrottle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		requests := 0
		var c *Client
		c = newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
			requests++
			// Another request can pause the shared limiter during this retry budget.
			c.rateLimiter.Throttle(time.Minute)
			return deadlineResponse(http.StatusServiceUnavailable, ""), nil
		})
		start := time.Now()
		_, err := c.GetProfile(context.Background())
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Contains(err.Error(), "last response: server error (503)")
		assert.Equal(1, requests)
		assert.Equal(30*time.Second, time.Since(start))
	})
}

func TestGetProfileDeadlineAfterQuotaPausePreservesLastResponse(t *testing.T) {
	for _, bodyRead := range []bool{false, true} {
		name := "transport"
		if bodyRead {
			name = "body read"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				requests := 0
				var retryStart time.Time
				c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
					requests++
					if requests == 1 {
						return deadlineResponse(http.StatusTooManyRequests, `{"error":{"message":"Too many concurrent requests"}}`), nil
					}
					retryStart = time.Now()
					if bodyRead {
						return &http.Response{StatusCode: http.StatusOK, Body: &deadlineBody{ctx: r.Context()}}, nil
					}
					<-r.Context().Done()
					return nil, r.Context().Err()
				})
				_, err := c.GetProfile(context.Background())
				require.ErrorIs(t, err, context.DeadlineExceeded)
				assert.Contains(err.Error(), "last response: rate limited (429): Too many concurrent requests")
				assert.Equal(2, requests, "an I/O deadline must not start another quota retry")
				assert.Equal(30*time.Second, time.Since(retryStart))
			})
		})
	}
}

func TestGetProfileDeadlineWithoutResponseStaysBare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newDeadlineClient(t, func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		profile, err := c.GetProfile(context.Background())
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Nil(t, profile)
		assert.NotContains(t, err.Error(), "last response",
			"a stalled request has no upstream response to report")
	})
}

func TestGmailErrorDetail(t *testing.T) {
	withRetryAfter := http.Header{"Retry-After": []string{"30"}}
	for _, tc := range []struct {
		name   string
		header http.Header
		body   string
		want   string
	}{
		{"reason and message", nil,
			`{"error":{"message":"Quota exceeded for quota metric","errors":[{"reason":"userRateLimitExceeded"}]}}`,
			"userRateLimitExceeded; Quota exceeded for quota metric"},
		{"message only", nil, `{"error":{"message":"Too many concurrent requests"}}`, "Too many concurrent requests"},
		{"unfamiliar body", withRetryAfter, "upstream proxy failure", "Retry-After 30"},
		{"empty body with header", withRetryAfter, "", "Retry-After 30"},
		{"empty", nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := tc.header
			if header == nil {
				header = http.Header{}
			}
			assert.Equal(t, tc.want, gmailErrorDetail(header, []byte(tc.body)))
		})
	}
}
