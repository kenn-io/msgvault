package gmail

import (
	"context"
	"sync"
	"time"
)

// Clock abstracts time operations for testability.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Operation represents a Gmail API operation with its quota cost.
type Operation int

const (
	OpMessagesGet         Operation = iota // 5 units
	OpMessagesGetRaw                       // 5 units
	OpMessagesList                         // 5 units
	OpLabelsList                           // 1 unit
	OpHistoryList                          // 2 units
	OpMessagesTrash                        // 5 units
	OpMessagesDelete                       // 10 units
	OpMessagesBatchDelete                  // 50 units
	OpProfile                              // 1 unit
)

// Cost returns the quota cost for an operation.
func (o Operation) Cost() int {
	switch o {
	case OpMessagesGet, OpMessagesGetRaw, OpMessagesList, OpMessagesTrash:
		return 5
	case OpMessagesDelete:
		return 10
	case OpMessagesBatchDelete:
		return 50
	case OpHistoryList:
		return 2
	default:
		return 1 // OpLabelsList, OpProfile, unknown
	}
}

// DefaultCapacity is the default token bucket capacity (Gmail's per-user quota).
const DefaultCapacity = 250

// DefaultRefillRate is tokens per second at the default rate.
const DefaultRefillRate = 250.0

const (
	// defaultQPS is the baseline QPS used to calculate the scale factor.
	defaultQPS = 5.0

	// throttleRecoveryFactor is the multiplier applied to the refill rate during throttle recovery.
	throttleRecoveryFactor = 0.5

	// minWait is the minimum wait duration when tokens are insufficient.
	minWait = 10 * time.Millisecond
)

// realClock implements Clock using the standard time package.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RateLimiter implements a token bucket rate limiter for Gmail API calls.
// It is safe for concurrent use and supports adaptive throttling.
type RateLimiter struct {
	mu             sync.Mutex
	clock          Clock
	tokens         float64
	capacity       float64
	refillRate     float64 // tokens per second
	baseRefillRate float64 // original refill rate for recovery
	lastRefill     time.Time
	throttledUntil time.Time // when throttled, don't refill until this time
}

// MinQPS is the minimum allowed QPS to prevent division by zero.
const MinQPS = 0.1

// NewRateLimiter creates a rate limiter with the specified QPS.
// A qps of 5 is the default safe rate for Gmail API.
// QPS is clamped to a minimum of MinQPS (0.1) to prevent division by zero.
func NewRateLimiter(qps float64) *RateLimiter {
	return newRateLimiter(realClock{}, qps)
}

// newRateLimiter creates a rate limiter with the given clock and QPS.
// Panics if clk is nil.
func newRateLimiter(clk Clock, qps float64) *RateLimiter {
	if clk == nil {
		panic("gmail: RateLimiter requires a non-nil Clock")
	}
	if qps < MinQPS {
		qps = MinQPS
	}

	scaleFactor := qps / defaultQPS
	if scaleFactor > 1.0 {
		scaleFactor = 1.0
	}

	refillRate := DefaultRefillRate * scaleFactor
	return &RateLimiter{
		clock:          clk,
		tokens:         DefaultCapacity,
		capacity:       DefaultCapacity,
		refillRate:     refillRate,
		baseRefillRate: refillRate,
		lastRefill:     clk.Now(),
	}
}

// reserve attempts to acquire tokens for the operation. Returns 0 if tokens
// were acquired immediately, or the duration to wait before retrying.
func (r *RateLimiter) reserve(op Operation) time.Duration {
	cost := float64(op.Cost())

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.clock.Now()

	// If we're in a throttle period, wait until it expires
	if now.Before(r.throttledUntil) {
		return r.throttledUntil.Sub(now)
	}

	r.refill()

	if r.tokens >= cost {
		r.tokens -= cost
		return 0
	}

	// Calculate wait time based on token deficit
	deficit := cost - r.tokens
	waitTime := max(time.Duration(deficit/r.refillRate*1000)*time.Millisecond, minWait)
	return waitTime
}

// Acquire blocks until the required tokens are available.
// Returns an error if the context is cancelled.
func (r *RateLimiter) Acquire(ctx context.Context, op Operation) error {
	for {
		waitTime := r.reserve(op)
		if waitTime == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.clock.After(waitTime):
			continue
		}
	}
}

// TryAcquire attempts to acquire tokens without blocking.
// Returns true if successful, false if insufficient tokens.
func (r *RateLimiter) TryAcquire(op Operation) bool {
	cost := float64(op.Cost())

	r.mu.Lock()
	defer r.mu.Unlock()

	r.refill()

	if r.tokens >= cost {
		r.tokens -= cost
		return true
	}
	return false
}

// refill adds tokens based on elapsed time. Must be called with lock held.
func (r *RateLimiter) refill() {
	now := r.clock.Now()

	// If we're in a throttle period, don't refill yet
	if now.Before(r.throttledUntil) {
		r.lastRefill = now
		return
	}

	// If throttle just expired, restore the base refill rate
	if r.refillRate < r.baseRefillRate && !r.throttledUntil.IsZero() {
		r.refillRate = r.baseRefillRate
	}

	elapsed := now.Sub(r.lastRefill).Seconds()
	r.lastRefill = now

	r.tokens += elapsed * r.refillRate
	if r.tokens > r.capacity {
		r.tokens = r.capacity
	}
}

// Available returns the current number of available tokens.
func (r *RateLimiter) Available() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refill()
	return r.tokens
}

// Throttle temporarily reduces the rate when we hit API rate limits.
// This provides adaptive back-pressure when Gmail returns 429/403 quota errors.
func (r *RateLimiter) Throttle(duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.clock.Now()
	newThrottleEnd := now.Add(duration)

	// Don't shorten an existing throttle window (e.g., 429 shouldn't shorten a 403 backoff)
	if newThrottleEnd.After(r.throttledUntil) {
		r.throttledUntil = newThrottleEnd
	}

	// Reset lastRefill to throttle end to prevent crediting elapsed time after throttle expires
	r.lastRefill = r.throttledUntil

	// Drain existing tokens to force waiting
	r.tokens = 0
	// Reduce refill rate for gradual recovery
	r.refillRate = r.baseRefillRate * throttleRecoveryFactor
}

// RecoverRate restores the original refill rate after throttling.
func (r *RateLimiter) RecoverRate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refillRate = r.baseRefillRate
}
