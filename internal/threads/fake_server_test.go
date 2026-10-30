package threads

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testToken = "synthetic-token-do-not-log"
const testID = "900719925474099312345"
const mediaJSON = `{"id":"900719925474099312345","timestamp":"2026-10-28T12:00:00+0000","text":"","has_replies":false}`

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
func newTestClient(t *testing.T, handler http.HandlerFunc, opts func(*ClientOptions)) (*Client, *RequestBudget, *testClock) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	clock := newTestClock()
	budget := clock.budget(100)
	o := ClientOptions{BaseURL: srv.URL + "/v1.0", AccessToken: testToken, HTTPClient: srv.Client(), Budget: budget}
	if opts != nil {
		opts(&o)
	}
	c, err := NewClient(o)
	require.NoError(t, err)
	return c, budget, clock
}
func respond(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, body)
}
