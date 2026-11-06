package msgraph

import (
	"math"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/httpretry"
)

// providerQuota is shared by per-request timeout clones and all MIME workers.
// Keeping its mutex behind a pointer also prevents copying a live lock.
type providerQuota struct {
	mu        sync.Mutex
	notBefore time.Time
}

// DeferProviderRetries configures a rapid acquisition before its first request.
// Provider waits become resumable outcomes rather than holding the daemon gate.
func (c *Client) DeferProviderRetries() { c.deferProviderRetry = true }

// RetryNotBefore is independent of a request's return shape or retry budget.
func (c *Client) RetryNotBefore() time.Time {
	c.quota.mu.Lock()
	defer c.quota.mu.Unlock()
	return c.quota.notBefore
}

func (c *Client) recordRetryFloor(header string, attempt int) time.Duration {
	delay := httpretry.RetryAfter(header, attempt, math.MaxInt64)
	candidate := time.Now().Add(delay)
	c.quota.mu.Lock()
	if candidate.After(c.quota.notBefore) {
		c.quota.notBefore = candidate
	}
	c.quota.mu.Unlock()
	return delay
}

func (e *ThrottledError) RetryNotBefore() time.Time { return e.NotBefore }
