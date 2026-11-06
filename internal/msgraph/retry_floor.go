package msgraph

import (
	"math"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
)

// DeferProviderRetries configures a rapid acquisition before its first request.
// Provider waits become resumable outcomes rather than holding the daemon gate.
func (c *Client) DeferProviderRetries() { c.deferProviderRetry = true }

// RetryNotBefore is independent of a request's return shape or retry budget.
func (c *Client) RetryNotBefore() time.Time {
	c.quotaMu.Lock()
	defer c.quotaMu.Unlock()
	return c.notBefore
}

func (c *Client) recordRetryFloor(header string, attempt int) time.Duration {
	delay := httpretry.RetryAfter(header, attempt, math.MaxInt64)
	candidate := time.Now().Add(delay)
	c.quotaMu.Lock()
	if candidate.After(c.notBefore) {
		c.notBefore = candidate
	}
	c.quotaMu.Unlock()
	return delay
}

func (e *ThrottledError) RetryNotBefore() time.Time { return e.NotBefore }
