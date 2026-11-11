package threads

import (
	"context"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func threadsBudgetRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", nil)
	require.NoError(t, err)
	return req
}

func TestThreadsPauseErrorMessage(t *testing.T) {
	for _, tc := range []struct{ reason, message string }{
		{"attempt_limit", "Threads connection stopped after too many requests; wait and rerun add-threads"},
		{"time_limit", "Threads connection timed out; rerun add-threads"},
		{"other", "Threads connection timed out; rerun add-threads"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			err := &PauseError{Reason: tc.reason}
			assert.Equal(t, tc.message, err.Error())
			assert.Equal(t, tc.reason, err.Reason)
		})
	}
}

func TestThreadsBudgetRateAndTime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	clock := newTestClock()
	limiter := NewRateLimiter(clock.Now, clock.Wait)
	var starts []time.Time
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		starts = append(starts, clock.Now())
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(http.NoBody), Request: r}, nil
	})
	start := clock.Now()
	for range 3 {
		budget := NewRequestBudget(BudgetOptions{MaxAttempts: 1, Now: clock.Now, Wait: clock.Wait, Limiter: limiter})
		client := NewBudgetedHTTPClient(&http.Client{Transport: transport}, budget)
		resp, err := client.Do(threadsBudgetRequest(t))
		require.NoError(err)
		require.NoError(resp.Body.Close())
		assert.Equal(1, budget.Attempts())
		resp, err = client.Do(threadsBudgetRequest(t))
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
	resp, err := NewBudgetedHTTPClient(&http.Client{Transport: transport}, b).Do(threadsBudgetRequest(t))
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
		ctx, cancel := context.WithCancel(t.Context())
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
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	})
	sourceBudget := NewRequestBudget(BudgetOptions{MaxAttempts: 10, Now: clock.Now, Wait: clock.Wait, Limiter: sourceLimiter})
	// Finish one source operation, leaving its next permitted attempt in force.
	send := func(b *RequestBudget) {
		resp, err := NewBudgetedHTTPClient(&http.Client{Transport: transport}, b).Do(threadsBudgetRequest(t))
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
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
			})}, b)
			req := threadsBudgetRequest(t)
			go func() {
				res, err := c.Do(req)
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
		resp, err := client.Do(threadsBudgetRequest(t))
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
