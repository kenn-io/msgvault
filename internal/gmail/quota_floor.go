package gmail

import (
	"context"
	"time"
)

// DeferProviderRetries lets a rapid account release its work gate instead of
// waiting through a provider prohibition. Configure before using the client.
func (c *Client) DeferProviderRetries() { c.deferProviderRetry = true }

// RetryNotBefore retains quota evidence even when a batch converts errors into
// durable per-message debt and returns a summary rather than the original error.
func (c *Client) RetryNotBefore() time.Time {
	c.rateLimiter.mu.Lock()
	defer c.rateLimiter.mu.Unlock()
	return c.rateLimiter.throttledUntil
}

func (c *Client) acquireQuota(ctx context.Context, op Operation) error {
	if !c.deferProviderRetry {
		return c.rateLimiter.Acquire(ctx, op)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		wait := c.rateLimiter.reserve(op)
		floor := c.RetryNotBefore()
		if c.rateLimiter.clock.Now().Before(floor) {
			return &ThrottledError{Summary: "provider retry deferred", NotBefore: floor}
		}
		if wait == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.rateLimiter.clock.After(wait):
		}
	}
}

func (e *ThrottledError) RetryNotBefore() time.Time { return e.NotBefore }
