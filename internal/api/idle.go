package api

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// IdleTracker tracks external HTTP activity and daemon-owned background work.
// When no request or tracked background job has run for timeout, it starts
// draining and invokes onIdle exactly once.
type IdleTracker struct {
	timeout time.Duration
	onIdle  func()

	mu             sync.Mutex
	lastExternal   time.Time
	activeExternal int
	activeWork     int
	draining       bool
	notify         chan struct{}
	once           sync.Once
}

func NewIdleTracker(timeout time.Duration, onIdle func()) *IdleTracker {
	return &IdleTracker{
		timeout:      timeout,
		onIdle:       onIdle,
		lastExternal: time.Now(),
		notify:       make(chan struct{}, 1),
	}
}

func (t *IdleTracker) Wrap(next http.Handler) http.Handler {
	if t == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		done, ok := t.beginExternal()
		if !ok {
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		defer done()
		next.ServeHTTP(w, r)
	})
}

func (t *IdleTracker) BeginWork() (func(), bool) {
	if t == nil {
		return func() {}, true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining {
		return func() {}, false
	}
	t.activeWork++
	t.signalLocked()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			if t.activeWork > 0 {
				t.activeWork--
			}
			t.lastExternal = time.Now()
			t.signalLocked()
			t.mu.Unlock()
		})
	}, true
}

func (t *IdleTracker) BeginWorkContext(ctx context.Context) (func(), bool) {
	if ctx != nil && ctx.Err() != nil {
		return func() {}, false
	}
	return t.BeginWork()
}

func (t *IdleTracker) Do(fn func()) bool {
	done, ok := t.BeginWork()
	if !ok {
		return false
	}
	defer done()
	fn()
	return true
}

func (t *IdleTracker) Touch() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if !t.draining {
		t.lastExternal = time.Now()
		t.signalLocked()
	}
	t.mu.Unlock()
}

func (t *IdleTracker) Run(ctx context.Context) {
	if t == nil || t.timeout <= 0 {
		return
	}
	timer := time.NewTimer(t.timeout)
	defer timer.Stop()
	for {
		wait, fire := t.nextWait(time.Now())
		if fire {
			t.once.Do(func() {
				if t.onIdle != nil {
					t.onIdle()
				}
			})
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-t.notify:
		case <-timer.C:
		}
	}
}

func (t *IdleTracker) beginExternal() (func(), bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining {
		return func() {}, false
	}
	t.activeExternal++
	t.signalLocked()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			if t.activeExternal > 0 {
				t.activeExternal--
			}
			t.lastExternal = time.Now()
			t.signalLocked()
			t.mu.Unlock()
		})
	}, true
}

func (t *IdleTracker) nextWait(now time.Time) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining {
		return 0, true
	}
	if t.activeExternal > 0 || t.activeWork > 0 {
		return t.timeout, false
	}
	elapsed := now.Sub(t.lastExternal)
	if elapsed >= t.timeout {
		t.draining = true
		return 0, true
	}
	return t.timeout - elapsed, false
}

func (t *IdleTracker) signalLocked() {
	select {
	case t.notify <- struct{}{}:
	default:
	}
}
