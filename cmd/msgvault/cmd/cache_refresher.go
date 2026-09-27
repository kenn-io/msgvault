package cmd

import (
	"context"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/scheduler"
)

// daemonCacheRefresher runs due analytics cache rebuilds for the daemon. It is
// nil outside the daemon, where post-sync rebuilds run synchronously.
var daemonCacheRefresher *backgroundCacheRefresher

// backgroundCacheRefresher runs analytics cache rebuilds off the scheduled job
// that requested them, so a sync releases the daemon operation gate as soon as
// its own work is done. It runs one rebuild at a time and collapses requests
// that arrive meanwhile into a single follow-up, so no request is lost. It
// holds no operation gate: builds read a SQLite snapshot and tolerate
// concurrent writes.
type backgroundCacheRefresher struct {
	ctx    context.Context
	cancel context.CancelFunc
	run    func(context.Context, string) error
	work   scheduler.WorkTracker

	// afterFunc schedules delayed requests; time.AfterFunc outside tests.
	afterFunc func(time.Duration, func()) *time.Timer

	mu        sync.Mutex
	running   bool
	pending   bool
	pendingID string
	closed    bool
	// delayed is the single outstanding RequestAfter timer, due at delayedAt.
	delayed   *time.Timer
	delayedAt time.Time
	wg        sync.WaitGroup
}

func newBackgroundCacheRefresher(
	ctx context.Context,
	run func(context.Context, string) error,
	work scheduler.WorkTracker,
) *backgroundCacheRefresher {
	ctx, cancel := context.WithCancel(ctx)
	r := &backgroundCacheRefresher{
		ctx: ctx, cancel: cancel, run: run, work: work, afterFunc: time.AfterFunc,
	}
	if r.run == nil {
		r.run = func(ctx context.Context, identifier string) error {
			return rebuildCacheNow(ctx, identifier, r.RequestAfter)
		}
	}
	return r
}

// Request asks for a rebuild check on behalf of identifier. It returns false
// once shutdown has begun.
func (r *backgroundCacheRefresher) Request(identifier string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	if r.running {
		r.pending = true
		r.pendingID = identifier
		return true
	}
	r.running = true
	r.wg.Add(1)
	go r.loop(identifier)
	return true
}

// RequestAfter issues Request(identifier) once delay has elapsed, unless the
// refresher shuts down first. Only the earliest outstanding delayed request
// is kept: every sync inside a throttle window asks for the same refresh, and
// the rebuild it triggers covers all of them.
func (r *backgroundCacheRefresher) RequestAfter(delay time.Duration, identifier string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	due := time.Now().Add(delay)
	if r.delayed != nil {
		if !due.Before(r.delayedAt) {
			return
		}
		r.delayed.Stop()
	}
	r.delayedAt = due
	var timer *time.Timer
	timer = r.afterFunc(delay, func() {
		r.mu.Lock()
		if r.delayed == timer {
			r.delayed = nil
		}
		r.mu.Unlock()
		r.Request(identifier)
	})
	r.delayed = timer
}

// Shutdown refuses new requests, cancels the running rebuild, and waits for
// it to stop or for ctx to end.
func (r *backgroundCacheRefresher) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	r.pending = false
	if r.delayed != nil {
		r.delayed.Stop()
		r.delayed = nil
	}
	r.mu.Unlock()
	r.cancel()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// shutdownBackgroundCacheRefresher covers startup exits that return before the
// daemon reaches its normal shutdown sequence. Shutdown is safe to call again
// there after the normal path has already drained the worker.
func shutdownBackgroundCacheRefresher(r *backgroundCacheRefresher) error {
	if r == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), serveOperationDrainTimeout)
	defer cancel()
	if err := r.Shutdown(ctx); err != nil {
		logger.Warn("analytics cache refresh did not stop during cleanup", "error", err)
		return err
	}
	return nil
}

func (r *backgroundCacheRefresher) loop(identifier string) {
	defer r.wg.Done()
	for {
		r.runOnce(identifier)
		r.mu.Lock()
		if !r.pending || r.closed {
			r.pending = false
			r.running = false
			r.mu.Unlock()
			return
		}
		identifier = r.pendingID
		r.pending = false
		r.mu.Unlock()
	}
}

func (r *backgroundCacheRefresher) runOnce(identifier string) {
	if r.work != nil {
		done, ok := r.work.BeginWorkContext(r.ctx)
		if !ok {
			return
		}
		defer done()
	}
	if err := r.run(r.ctx, identifier); err != nil && r.ctx.Err() == nil {
		logger.Warn("background analytics cache refresh failed",
			"identifier", identifier, "error", err)
	}
}
