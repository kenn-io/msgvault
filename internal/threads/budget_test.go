package threads

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThreadsBudgetCountsRetries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/oauth/access_token" {
			respond(w, 200, `{}`)
		} else {
			respond(w, 503, `{"error":{"code":2}}`)
		}
	}))
	defer srv.Close()
	clock := newTestClock()
	b := clock.budget(3)
	httpClient := NewBudgetedHTTPClient(srv.Client(), b)
	resp, err := httpClient.Post(srv.URL+"/oauth/access_token", "application/x-www-form-urlencoded", nil)
	require.NoError(err)
	require.NoError(resp.Body.Close())
	c, err := NewClient(ClientOptions{HTTPClient: httpClient, BaseURL: srv.URL, AccessToken: testToken, Budget: b})
	require.NoError(err)
	_, err = c.Me(context.Background())
	var pause *PauseError
	require.ErrorAs(err, &pause)
	assert.Equal(3, b.Attempts())
	assert.Equal(int32(3), calls.Load(), "OAuth and retries share exactly one transport budget")
	assert.Equal(int32(3), calls.Load())
}

func TestThreadsBudgetRateAndTime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	clock := newTestClock()
	limiter := NewRateLimiter(clock.Now, clock.Wait)
	var starts []time.Time
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		starts = append(starts, clock.Now())
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(http.NoBody), Request: r}, nil
	})
	start := clock.Now()
	for range 3 {
		budget := NewRequestBudget(BudgetOptions{MaxAttempts: 1, Now: clock.Now, Wait: clock.Wait, Limiter: limiter})
		client := NewBudgetedHTTPClient(&http.Client{Transport: transport}, budget)
		resp, err := client.Get("https://example.com")
		require.NoError(err)
		require.NoError(resp.Body.Close())
		assert.Equal(1, budget.Attempts())
		resp, err = client.Get("https://example.com")
		if resp != nil {
			require.NoError(resp.Body.Close())
		}
		var pause *PauseError
		require.ErrorAs(err, &pause)
	}
	assert.Equal([]time.Time{start.Add(500 * time.Millisecond), start.Add(time.Second), start.Add(1500 * time.Millisecond)}, starts)
	clock.advance(time.Hour)
	b := clock.budget(10)
	clock.advance(RunTimeLimit)
	resp, err := NewBudgetedHTTPClient(&http.Client{Transport: transport}, b).Get("https://example.com")
	if resp != nil {
		require.NoError(resp.Body.Close())
	}
	var pause *PauseError
	require.ErrorAs(err, &pause)
	assert.Zero(b.Attempts())
	assert.Zero(b.Remaining())
}

func TestThreadsBudgetRealTimerCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := NewRequestBudget(BudgetOptions{})
		client := NewBudgetedHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			assert.Fail(t, "cancelled timer reached transport")
			return nil, context.Canceled
		})}, budget)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
			if err != nil {
				done <- err
				return
			}
			resp, err := client.Do(req)
			if resp != nil {
				if closeErr := resp.Body.Close(); closeErr != nil {
					done <- closeErr
					return
				}
			}
			done <- err
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		require.ErrorIs(t, <-done, context.Canceled)
		assert.Zero(t, budget.Attempts())
	})
}

func TestThreadsBudgetBindVerifiedSource(t *testing.T) {
	clock := newTestClock()
	sourceLimiter := NewRateLimiter(clock.Now, clock.Wait)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
	})
	sourceBudget := NewRequestBudget(BudgetOptions{MaxAttempts: 10, Now: clock.Now, Wait: clock.Wait, Limiter: sourceLimiter})
	// Finish one source operation, leaving its next permitted attempt in force.
	send := func(b *RequestBudget) {
		resp, err := NewBudgetedHTTPClient(&http.Client{Transport: transport}, b).Get("https://example.com")
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	send(sourceBudget)
	unbound := clock.budget(10)
	// OAuth spent time and an attempt before the source was verified.
	send(unbound)
	unbound.BindLimiter(sourceLimiter)
	before := unbound.Remaining()
	send(unbound)
	send(sourceBudget)
	assert.Equal(t, 2, unbound.Attempts())
	assert.Equal(t, before-time.Second, unbound.Remaining(), "binding retains elapsed time and later source operations see the same limiter")
}

func TestThreadsBudgetConcurrentSourceOperations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		limiter := NewRateLimiter(nil, nil)
		start := time.Now()
		starts := make(chan time.Time, 3)
		done := make(chan error, 3)
		for range 3 {
			b := NewRequestBudget(BudgetOptions{Limiter: limiter})
			c := NewBudgetedHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				starts <- time.Now()
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
			})}, b)
			go func() {
				res, err := c.Get("https://example.com")
				if err == nil {
					err = res.Body.Close()
				}
				done <- err
			}()
		}
		for range 3 {
			require.NoError(t, <-done)
		}
		assert.Equal(start.Add(500*time.Millisecond), <-starts)
		assert.Equal(start.Add(time.Second), <-starts)
		assert.Equal(start.Add(1500*time.Millisecond), <-starts)
	})
}

func TestThreadsBudgetDoesNotHideTransportReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls atomic.Int32
	c, b, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 {
			connection, _, err := http.NewResponseController(w).Hijack()
			if assert.NoError(err) {
				assert.NoError(connection.Close())
			}
			return
		}
		respond(w, 200, `{"id":"123"}`)
	}, nil)
	_, err := c.Me(context.Background())
	require.NoError(err)
	_, err = c.Me(context.Background())
	require.Error(err, "transport failures have no invisible replay")
	assert.Equal(int32(2), calls.Load())
	assert.Equal(2, b.Attempts())
}

func TestThreadsBudgetDeadlineIncludesLimiterWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		start := time.Now()
		limiter := NewRateLimiter(nil, func(ctx context.Context, _ time.Duration) error {
			// Model a delayed limiter; the enclosing run still has a deadline.
			return waitContext(ctx, time.Hour)
		})
		budget := NewRequestBudget(BudgetOptions{MaxDuration: time.Second, Limiter: limiter})
		client := NewBudgetedHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			assert.Fail("expired wait dispatched a request")
			return nil, context.DeadlineExceeded
		})}, budget)
		resp, err := client.Get("https://example.com")
		if resp != nil {
			require.NoError(resp.Body.Close())
		}
		var pause *PauseError
		require.ErrorAs(err, &pause)
		assert.Equal(start.Add(time.Second), time.Now())
		assert.Zero(budget.Attempts())
	})
}

func TestThreadsBudgetRunContextTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := NewRequestBudget(BudgetOptions{MaxDuration: time.Minute})
		ctx, cancel := budget.RunContext(t.Context())
		defer cancel()
		time.Sleep(time.Minute)
		synctest.Wait()
		select {
		case <-ctx.Done():
			var pause *PauseError
			require.ErrorAs(t, context.Cause(ctx), &pause)
			assert.Equal(t, "time_limit", pause.Reason)
		default:
			require.Fail(t, "workload context remained active at its virtual deadline")
		}
	})
}
