package gmail

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockClock provides deterministic time control for tests.
type mockClock struct {
	mu          sync.Mutex
	current     time.Time
	timers      []mockTimer
	timerNotify chan struct{}
	notifyOnce  sync.Once
}

type mockTimer struct {
	deadline time.Time
	ch       chan time.Time
}

func newMockClock() *mockClock {
	return &mockClock{
		current:     time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		timerNotify: make(chan struct{}, 1),
	}
}

// ensureNotifyChannel lazily initializes timerNotify to prevent blocking on a
// nil channel if mockClock{} is instantiated directly without newMockClock().
func (c *mockClock) ensureNotifyChannel() {
	c.notifyOnce.Do(func() {
		if c.timerNotify == nil {
			c.timerNotify = make(chan struct{}, 1)
		}
	})
}

func (c *mockClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *mockClock) After(d time.Duration) <-chan time.Time {
	c.ensureNotifyChannel()
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	deadline := c.current.Add(d)
	if !c.current.Before(deadline) {
		ch <- c.current
		return ch
	}
	c.timers = append(c.timers, mockTimer{deadline: deadline, ch: ch})
	// Notify waiters that a new timer was registered.
	select {
	case c.timerNotify <- struct{}{}:
	default:
	}
	return ch
}

// TimerCount returns the number of pending timers.
func (c *mockClock) TimerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// waitForTimers blocks until the mock clock has at least n pending timers.
func waitForTimers(t *testing.T, clk *mockClock, n int) {
	t.Helper()
	clk.ensureNotifyChannel()
	timeout := time.After(2 * time.Second)
	for clk.TimerCount() < n {
		select {
		case <-clk.timerNotify:
		case <-timeout:
			require.Failf(t, "timed out waiting for timers", "timed out waiting for %d timer(s); have %d", n, clk.TimerCount())
		}
	}
}

// Advance moves the clock forward and fires any pending timers.
func (c *mockClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.current = c.current.Add(d)
	now := c.current
	var remaining []mockTimer
	for _, t := range c.timers {
		if !now.Before(t.deadline) {
			t.ch <- now
		} else {
			remaining = append(remaining, t)
		}
	}
	c.timers = remaining
	c.mu.Unlock()
}

// newTestLimiterWithClock creates a rate limiter using the given mock clock.
func newTestLimiterWithClock(clk *mockClock) *RateLimiter {
	return newRateLimiter(clk, defaultQPS)
}

// rlFixture encapsulates the mock clock and rate limiter for test setup.
type rlFixture struct {
	clk *mockClock
	rl  *RateLimiter
}

func newRLFixture() *rlFixture {
	clk := newMockClock()
	return &rlFixture{
		clk: clk,
		rl:  newTestLimiterWithClock(clk),
	}
}

// drain sets tokens to zero.
func (f *rlFixture) drain() {
	f.rl.mu.Lock()
	defer f.rl.mu.Unlock()
	f.rl.tokens = 0
}

// state returns a snapshot of the limiter's refill rate and throttle deadline
// under the mutex.
func (f *rlFixture) state() (refillRate float64, throttledUntil time.Time) {
	f.rl.mu.Lock()
	defer f.rl.mu.Unlock()
	return f.rl.refillRate, f.rl.throttledUntil
}

// assertAvailable checks the current available tokens.
func (f *rlFixture) assertAvailable(t *testing.T, expected float64) {
	t.Helper()
	assert.InDelta(t, expected, f.rl.Available(), 1e-9, "Available()")
}

// acquireAsync runs Acquire in a background goroutine and returns a channel
// that receives the result. It waits for the goroutine to either register a
// timer on the mock clock or complete immediately.
func (f *rlFixture) acquireAsync(ctx context.Context, t *testing.T, op Operation) <-chan error {
	t.Helper()
	f.clk.ensureNotifyChannel()
	timersBefore := f.clk.TimerCount()
	ch := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		ch <- f.rl.Acquire(ctx, op)
		close(done)
	}()
	// Wait until either a new timer appears or the goroutine completes.
	timeout := time.After(2 * time.Second)
	for {
		select {
		case <-f.clk.timerNotify:
			if f.clk.TimerCount() > timersBefore {
				return ch
			}
		case <-done:
			return ch
		case <-timeout:
			require.Fail(t, "acquireAsync: timed out waiting for timer or completion")
			return ch
		}
	}
}

func TestOperationCost(t *testing.T) {
	tests := []struct {
		op   Operation
		cost int
	}{
		{OpMessagesGet, 5},
		{OpMessagesGetRaw, 5},
		{OpMessagesList, 5},
		{OpLabelsList, 1},
		{OpHistoryList, 2},
		{OpMessagesTrash, 5},
		{OpMessagesDelete, 10},
		{OpMessagesBatchDelete, 50},
		{OpProfile, 1},
		{Operation(999), 1}, // Unknown operation defaults to 1
	}

	for _, tc := range tests {
		got := tc.op.Cost()
		assert.Equal(t, tc.cost, got, "Operation(%d).Cost()", tc.op)
	}
}

func TestNewRateLimiter(t *testing.T) {
	rl := NewRateLimiter(5.0)

	assert.InDelta(t, float64(DefaultCapacity), rl.capacity, 1e-9, "capacity")
	assert.InDelta(t, float64(DefaultCapacity), rl.tokens, 1e-9, "initial tokens")
	assert.InDelta(t, DefaultRefillRate, rl.refillRate, 1e-9, "refillRate")
}

func TestNewRateLimiter_ScaledQPS(t *testing.T) {
	rl := NewRateLimiter(2.5)
	expectedRate := DefaultRefillRate * 0.5
	assert.InDelta(t, expectedRate, rl.refillRate, 1e-9, "refillRate at 2.5 QPS")

	rl = NewRateLimiter(10.0)
	assert.InDelta(t, DefaultRefillRate, rl.refillRate, 1e-9, "refillRate at 10 QPS (capped)")
}

func TestNewRateLimiter_NilClockPanics(t *testing.T) {
	assert.Panics(t, func() {
		newRateLimiter(nil, 5.0)
	}, "newRateLimiter(nil, ...) should panic")
}

func TestRateLimiter_TryAcquire(t *testing.T) {
	f := newRLFixture()

	assert.True(t, f.rl.TryAcquire(OpProfile), "TryAcquire(OpProfile) should succeed when bucket is full")

	f.drain()

	assert.False(t, f.rl.TryAcquire(OpMessagesBatchDelete), "TryAcquire(OpMessagesBatchDelete) should fail when bucket is empty")
}

func TestRateLimiter_Acquire_Success(t *testing.T) {
	f := newRLFixture()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := f.rl.Acquire(ctx, OpProfile)
	require.NoError(t, err, "Acquire()")
}

func TestRateLimiter_Acquire_ContextCancelled(t *testing.T) {
	f := newRLFixture()
	f.drain()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := f.rl.Acquire(ctx, OpMessagesGet)
	assert.ErrorIs(t, err, context.Canceled, "Acquire() with cancelled context")
}

func TestRateLimiter_Acquire_ContextTimeout(t *testing.T) {
	f := newRLFixture()
	f.drain()
	// Set a very slow refill so tokens won't accumulate
	f.rl.mu.Lock()
	f.rl.refillRate = 0.001
	f.rl.mu.Unlock()

	// Use context.WithCancel and cancel via the mock clock to avoid
	// mixing real-time deadlines with mock clock advancement.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Schedule cancellation when the mock clock advances past 50ms
	go func() {
		<-f.clk.After(50 * time.Millisecond)
		cancel()
	}()
	waitForTimers(t, f.clk, 1)

	done := f.acquireAsync(ctx, t, OpMessagesBatchDelete)

	// Advance mock clock past the cancel point
	f.clk.Advance(100 * time.Millisecond)

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled, "Acquire()")
	case <-time.After(2 * time.Second):
		require.Fail(t, "Acquire() did not return after context cancelled")
	}
}

func TestRateLimiter_Refill(t *testing.T) {
	f := newRLFixture()
	f.drain()

	f.assertAvailable(t, 0)

	// Advance clock by 1 second: should refill 250 tokens
	f.clk.Advance(1 * time.Second)

	f.assertAvailable(t, DefaultCapacity)
}

func TestRateLimiter_Available(t *testing.T) {
	f := newRLFixture()

	f.assertAvailable(t, DefaultCapacity)

	f.rl.TryAcquire(OpMessagesGet) // cost 5

	f.assertAvailable(t, float64(DefaultCapacity-5))
}

func TestRateLimiter_Concurrent(t *testing.T) {
	// Use real clock for concurrency test since goroutine scheduling is inherent
	rl := NewRateLimiter(5.0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errors := make(chan error, 100)

	for range 20 {
		wg.Go(func() {
			if err := rl.Acquire(ctx, OpProfile); err != nil {
				errors <- err
			}
		})
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		assert.NoError(t, err, "concurrent Acquire()")
	}
}

func TestRateLimiter_CapacityLimit(t *testing.T) {
	f := newRLFixture()

	// Advance time significantly — tokens should still be capped
	f.clk.Advance(10 * time.Second)

	avail := f.rl.Available()
	assert.LessOrEqual(t, avail, float64(DefaultCapacity), "Available() should not exceed capacity %v", DefaultCapacity)
}

func TestRateLimiter_Throttle(t *testing.T) {
	t.Run("DrainsTokensAndBlocksRefill", func(t *testing.T) {
		f := newRLFixture()

		f.rl.Throttle(100 * time.Millisecond)

		f.assertAvailable(t, 0)

		// Advance 50ms (still within throttle) — tokens should remain 0
		f.clk.Advance(50 * time.Millisecond)
		f.assertAvailable(t, 0)

		// Advance past throttle expiry
		f.clk.Advance(60 * time.Millisecond)
		assert.Greater(t, f.rl.Available(), float64(0), "Available() after throttle expiry")
	})

	t.Run("RecoverRate", func(t *testing.T) {
		f := newRLFixture()

		f.rl.Throttle(10 * time.Millisecond)

		rate, _ := f.state()
		assert.InDelta(t, DefaultRefillRate*0.5, rate, 1e-9, "refillRate after Throttle")

		f.rl.RecoverRate()

		rate, _ = f.state()
		assert.InDelta(t, DefaultRefillRate, rate, 1e-9, "refillRate after RecoverRate")
	})

	t.Run("DoesNotShortenBackoff", func(t *testing.T) {
		f := newRLFixture()

		f.rl.Throttle(200 * time.Millisecond)
		_, first := f.state()

		f.rl.Throttle(50 * time.Millisecond)
		_, second := f.state()

		assert.False(t, second.Before(first), "Throttle shortened existing backoff: first=%v, second=%v", first, second)
	})

	t.Run("ExtendsBackoff", func(t *testing.T) {
		f := newRLFixture()

		f.rl.Throttle(50 * time.Millisecond)
		_, first := f.state()

		f.clk.Advance(30 * time.Millisecond)
		f.rl.Throttle(50 * time.Millisecond)
		_, second := f.state()

		assert.True(t, second.After(first), "Throttle did not extend backoff: first=%v, second=%v", first, second)
	})

	t.Run("AutoRecoverRate", func(t *testing.T) {
		f := newRLFixture()

		f.rl.Throttle(50 * time.Millisecond)

		rate, _ := f.state()
		assert.InDelta(t, DefaultRefillRate*0.5, rate, 1e-9, "refillRate after Throttle")

		f.clk.Advance(100 * time.Millisecond)
		f.rl.Available() // triggers refill and auto-recovery

		rate, _ = f.state()
		assert.InDelta(t, DefaultRefillRate, rate, 1e-9, "refillRate after throttle expiry")
	})
}

func TestRateLimiter_Acquire_WaitsForThrottle(t *testing.T) {
	f := newRLFixture()

	f.rl.Throttle(100 * time.Millisecond)

	done := f.acquireAsync(context.Background(), t, OpProfile)

	// Advance past throttle — Acquire should complete
	f.clk.Advance(150 * time.Millisecond)

	select {
	case err := <-done:
		require.NoError(t, err, "Acquire()")
	case <-time.After(1 * time.Second):
		require.Fail(t, "Acquire() did not complete after advancing clock past throttle")
	}
}

func TestMockClock_ZeroValueSafe(t *testing.T) {
	// Verify that a zero-value mockClock{} won't block forever due to nil channel.
	clk := &mockClock{}

	// After should work without hanging
	ch := clk.After(10 * time.Millisecond)
	require.NotNil(t, ch, "After() returned nil channel")

	// timerNotify should be lazily initialized
	require.NotNil(t, clk.timerNotify, "timerNotify should be initialized after After() call")
}
