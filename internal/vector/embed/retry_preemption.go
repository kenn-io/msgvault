package embed

import (
	"context"
	"io"
	"net/http"
	"sync"

	"go.kenn.io/msgvault/internal/jobctx"
)

// cooperativeRetryState stops a retry wait when a scheduled embedding job is
// asked to yield. It waits for an in-flight HTTP response to finish before
// canceling the retry context so completed provider work can still be used.
type cooperativeRetryState struct {
	mu       sync.Mutex
	inFlight bool
	pending  bool
	cancel   context.CancelCauseFunc
}

func (s *cooperativeRetryState) request() {
	s.mu.Lock()
	if s.inFlight {
		s.pending = true
		s.mu.Unlock()
		return
	}
	s.pending = true
	s.mu.Unlock()
	s.cancel(jobctx.ErrYieldedToWaiter)
}

func (s *cooperativeRetryState) begin(ctx context.Context) bool {
	s.mu.Lock()
	if s.pending || jobctx.PreemptionRequested(ctx) {
		s.pending = true
		s.mu.Unlock()
		s.cancel(jobctx.ErrYieldedToWaiter)
		return false
	}
	s.inFlight = true
	s.mu.Unlock()
	return true
}

func (s *cooperativeRetryState) finish(ctx context.Context) {
	s.mu.Lock()
	s.inFlight = false
	shouldYield := s.pending || jobctx.PreemptionRequested(ctx)
	if shouldYield {
		s.pending = true
	}
	s.mu.Unlock()
	if shouldYield {
		s.cancel(jobctx.ErrYieldedToWaiter)
	}
}

type cooperativeRetryTransport struct {
	base  http.RoundTripper
	state *cooperativeRetryState
}

func (t cooperativeRetryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if !t.state.begin(request.Context()) {
		return nil, jobctx.ErrYieldedToWaiter
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		t.state.finish(request.Context())
		return nil, err
	}
	if response == nil || response.Body == nil {
		t.state.finish(request.Context())
		return response, nil
	}
	response.Body = &cooperativeRetryBody{
		ReadCloser: response.Body,
		finish:     func() { t.state.finish(request.Context()) },
	}
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
	signal := jobctx.PreemptionSignal(ctx)
	if signal == nil {
		return ctx, base, func() {}
	}

	retryCtx, cancel := context.WithCancelCause(ctx)
	state := &cooperativeRetryState{cancel: cancel}
	stop := make(chan struct{})
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		select {
		case <-signal:
			state.request()
		case <-stop:
		}
	}()
	cleanup := func() {
		close(stop)
		<-monitorDone
		cancel(nil)
	}
	return retryCtx, cooperativeRetryTransport{base: base, state: state}, cleanup
}
