package embed

import (
	"context"
	"io"
	"net/http"
	"sync"

	"go.kenn.io/msgvault/internal/jobctx"
)

// waitForPreemption blocks until the job under ctx is asked to yield or ctx
// ends, and reports whether a yield was requested.
func waitForPreemption(ctx context.Context) bool {
	select {
	case <-jobctx.PreemptionSignal(ctx):
		return true
	case <-ctx.Done():
		return false
	}
}

// cooperativeRetryTransport cancels the retry context once a scheduled
// embedding job is asked to yield, but never while an HTTP response is in
// flight, so completed provider work can still be used.
type cooperativeRetryTransport struct {
	base   http.RoundTripper
	ctx    context.Context
	cancel context.CancelCauseFunc

	mu       sync.Mutex
	inFlight bool
}

// yieldIfIdle cancels the retry context when preemption was requested and no
// request is in flight. Preemption requests are sticky, so a request that was
// in flight yields when its response body closes.
func (t *cooperativeRetryTransport) yieldIfIdle() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.inFlight && jobctx.PreemptionRequested(t.ctx) {
		t.cancel(jobctx.ErrYieldedToWaiter)
	}
}

func (t *cooperativeRetryTransport) finish() {
	t.mu.Lock()
	t.inFlight = false
	t.mu.Unlock()
	t.yieldIfIdle()
}

func (t *cooperativeRetryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if jobctx.PreemptionRequested(t.ctx) {
		t.mu.Unlock()
		t.cancel(jobctx.ErrYieldedToWaiter)
		return nil, jobctx.ErrYieldedToWaiter
	}
	t.inFlight = true
	t.mu.Unlock()
	response, err := t.base.RoundTrip(request)
	if err != nil || response == nil || response.Body == nil {
		t.finish()
		return response, err
	}
	response.Body = &cooperativeRetryBody{ReadCloser: response.Body, finish: t.finish}
	return response, nil
}

type cooperativeRetryBody struct {
	io.ReadCloser

	finish func()
	once   sync.Once
}

func (b *cooperativeRetryBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.finish)
	return err
}

// cooperativeRetryContext makes retry backoff listen to job preemption while
// leaving any active provider request alone until its response body is closed.
func cooperativeRetryContext(
	ctx context.Context, base http.RoundTripper,
) (context.Context, http.RoundTripper, func()) {
	if jobctx.PreemptionSignal(ctx) == nil {
		return ctx, base, func() {}
	}
	if base == nil {
		base = http.DefaultTransport
	}
	retryCtx, cancel := context.WithCancelCause(ctx)
	transport := &cooperativeRetryTransport{base: base, ctx: ctx, cancel: cancel}
	pollCtx, stopPolling := context.WithCancel(ctx)
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		if waitForPreemption(pollCtx) {
			transport.yieldIfIdle()
		}
	}()
	return retryCtx, transport, func() {
		stopPolling()
		<-polled
		cancel(nil)
	}
}
