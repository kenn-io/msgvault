package threads

import (
	"context"
	"net/http"
	"sync"
	"time"
)

type testClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func newTestClock() *testClock      { return &testClock{now: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)} }
func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) Wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	return nil
}
func (c *testClock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }
func (c *testClock) budget(limit int) *RequestBudget {
	return NewRequestBudget(BudgetOptions{MaxAttempts: limit, Now: c.Now, Wait: c.Wait})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
