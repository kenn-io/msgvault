package threads

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	ScheduledRequestLimit = 200
	ManualRequestLimit    = 1000
	RunTimeLimit          = 10 * time.Minute
)

type WaitFunc func(context.Context, time.Duration) error
type BudgetOptions struct {
	MaxAttempts int
	MaxDuration time.Duration
	Now         func() time.Time
	Wait        WaitFunc
	Limiter     *RateLimiter
}

// RateLimiter spaces attempts by at least half a second, without an initial
// burst. Keep one per source across operation budgets under the lifecycle lock.
type RateLimiter struct {
	gate chan struct{}
	now  func() time.Time
	wait WaitFunc
	next time.Time
}

type RequestBudget struct {
	mu                sync.Mutex
	attempts, maximum int
	deadline          time.Time
	now               func() time.Time
	wait              WaitFunc
	limiter           *RateLimiter
	pause             *PauseError
}

func clockDefaults(now func() time.Time, wait WaitFunc) (func() time.Time, WaitFunc) {
	if now == nil {
		now = time.Now
	}
	if wait == nil {
		wait = waitContext
	}
	return now, wait
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func NewRateLimiter(now func() time.Time, wait WaitFunc) *RateLimiter {
	now, wait = clockDefaults(now, wait)
	return &RateLimiter{gate: make(chan struct{}, 1), now: now, wait: wait, next: now().Add(time.Second / 2)}
}

// NewRequestBudget starts the elapsed-time cap immediately, including token
// checks and refresh. Zero limits select scheduled defaults; limits cannot
// exceed the manual workload cap or ten minutes.
func NewRequestBudget(o BudgetOptions) *RequestBudget {
	now, wait := clockDefaults(o.Now, o.Wait)
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = ScheduledRequestLimit
	}
	if o.MaxDuration <= 0 {
		o.MaxDuration = RunTimeLimit
	}
	if o.Limiter == nil {
		o.Limiter = NewRateLimiter(now, wait)
	}
	return &RequestBudget{maximum: min(o.MaxAttempts, ManualRequestLimit), deadline: now().Add(min(o.MaxDuration, RunTimeLimit)), now: now, wait: wait, limiter: o.Limiter}
}

func (b *RequestBudget) Attempts() int { b.mu.Lock(); defer b.mu.Unlock(); return b.attempts }
func (b *RequestBudget) Remaining() time.Duration {
	return max(time.Duration(0), b.deadline.Sub(b.now()))
}

// BindLimiter attaches the verified source after OAuth owner discovery without
// resetting the run. Call under the source lifecycle lock, before further I/O.
func (b *RequestBudget) BindLimiter(limiter *RateLimiter) {
	if limiter == nil {
		return
	}
	b.mu.Lock()
	old := b.limiter
	b.mu.Unlock()
	if old == limiter {
		return
	}
	old.gate <- struct{}{}
	next := old.next
	<-old.gate
	limiter.gate <- struct{}{}
	if next.After(limiter.next) {
		limiter.next = next
	}
	<-limiter.gate
	b.mu.Lock()
	b.limiter = limiter
	b.mu.Unlock()
}

func (b *RequestBudget) check() error {
	if b.pause != nil {
		return b.pause
	}
	if b.attempts >= b.maximum {
		return &PauseError{Reason: "attempt_limit"}
	}
	if b.Remaining() <= 0 {
		return &PauseError{Reason: "time_limit"}
	}
	return nil
}

func (b *RequestBudget) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	err := b.check()
	l := b.limiter
	b.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case l.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-l.gate }()
	if delay := l.next.Sub(l.now()); delay > 0 {
		if delay >= b.Remaining() {
			return &PauseError{Reason: "time_limit"}
		}
		if err := l.wait(ctx, delay); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.check(); err != nil {
		return err
	}
	l.next = l.now().Add(time.Second / 2)
	b.attempts++
	return nil
}

// NewBudgetedHTTPClient preserves the provided transport and settings on a
// copy. Standard transports use one HTTP/1 connection per attempt to prevent
// net/http's implicit pooled-connection and HTTP/2 retries from escaping the
// budget. A custom RoundTripper must perform a single dispatch per call.
// OAuth and media calls must share this budget. Rewrapping the same
// budget is idempotent; changing budgets replaces the wrapper, never stacks it.
// Redirects are refused before forwarding credentials to any destination.
func NewBudgetedHTTPClient(c *http.Client, b *RequestBudget) *http.Client {
	if c == nil {
		c = &http.Client{Timeout: time.Minute}
	}
	if b == nil {
		b = NewRequestBudget(BudgetOptions{})
	}
	out := *c
	base := c.Transport
	if wrapped, ok := base.(*budgetTransport); ok {
		base = wrapped.base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	if transport, ok := base.(*http.Transport); ok {
		transport = transport.Clone()
		transport.DisableKeepAlives = true
		transport.Protocols = new(http.Protocols)
		transport.Protocols.SetHTTP1(true)
		base = transport
	}
	out.Transport = &budgetTransport{base: base, budget: b}
	out.CheckRedirect = func(*http.Request, []*http.Request) error { return ErrRedirect }
	return &out
}

type budgetTransport struct {
	base   http.RoundTripper
	budget *RequestBudget
}

func (t *budgetTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(r.Context(), t.budget.Remaining())
	if err := t.budget.acquire(ctx); err != nil {
		cancel()
		if pause := workloadPause(r.Context()); pause != nil {
			return nil, pause
		}
		if r.Context().Err() != nil {
			return nil, r.Context().Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &PauseError{Reason: "time_limit"}
		}
		return nil, err
	}
	response, err := t.base.RoundTrip(r.Clone(ctx))
	if err != nil {
		cancel()
		if pause := workloadPause(r.Context()); pause != nil {
			return nil, pause
		}
		if r.Context().Err() != nil {
			return nil, r.Context().Err()
		}
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.Canceled) {
			return nil, &PauseError{Reason: "time_limit"}
		}
		return nil, errors.New("threads HTTP transport failed")
	}
	// Retain a successful final OAuth result, while forbidding the next
	// attempt in this operation after any recognized high-usage response.
	if highUsage(response.Header) {
		t.budget.mu.Lock()
		t.budget.pause = &PauseError{Reason: "high_usage"}
		t.budget.mu.Unlock()
	}
	response.Body = &budgetBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

type budgetBody struct {
	io.ReadCloser

	cancel context.CancelFunc
}

func (b *budgetBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.cancel()
	}
	return n, err
}
func (b *budgetBody) Close() error { defer b.cancel(); return b.ReadCloser.Close() }

// RunContext bounds local work as well as HTTP, retaining a typed workload
// pause distinct from caller cancellation.
func (b *RequestBudget) RunContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(parent, b.Remaining(), &PauseError{Reason: "time_limit"})
}

func workloadPause(ctx context.Context) *PauseError {
	pause, _ := errors.AsType[*PauseError](context.Cause(ctx))
	return pause
}
func onlyCancellation(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if group, ok := err.(interface{ Unwrap() []error }); ok {
		for _, part := range group.Unwrap() {
			if !onlyCancellation(part) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyCancellation(wrapped.Unwrap())
	}
	return false
}
