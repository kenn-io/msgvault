package loops

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/scheduler"
)

// RunnerOptions keeps daemon lifetime and gate ownership outside HTTP request
// contexts. Scheduled jobs already hold the scheduler's operation gate; only
// the unscheduled fallback calls BeginWork itself.
type RunnerOptions struct {
	Context        context.Context
	Run            func(context.Context) error
	Rebuild        func(context.Context) error
	BeginWork      func(context.Context, string) (func(), bool)
	StartScheduled func() (scheduler.JobDisposition, error)
	OnError        func(error)
	// AfterRun runs only for manual passes, under the gate with daemon lifetime.
	AfterRun func(context.Context)
}

// Runner coalesces manual work and rebuild intent. The scheduler supplies its
// own single flight when configured; an empty schedule uses the same daemon
// service through a bounded, gated goroutine.
type Runner struct {
	opts                                 RunnerOptions
	mu                                   sync.Mutex
	running, executing, pending, rebuild bool
}

func NewRunner(opts RunnerOptions) (*Runner, error) {
	if opts.Context == nil || opts.Run == nil || opts.Rebuild == nil {
		return nil, errors.New("open loops runner: daemon context, run and rebuild are required")
	}
	if opts.StartScheduled == nil && opts.BeginWork == nil {
		return nil, errors.New("open loops runner: manual work requires an operation gate")
	}
	return &Runner{opts: opts}, nil
}

// Start returns before acquiring the operation gate. A handler can therefore
// request work while holding that gate without deadlocking the daemon.
func (r *Runner) Start(rebuild bool) (scheduler.JobDisposition, error) {
	if err := r.opts.Context.Err(); err != nil {
		return "", err
	}
	r.mu.Lock()
	r.rebuild = r.rebuild || rebuild
	if r.opts.StartScheduled != nil {
		r.mu.Unlock()
		return r.opts.StartScheduled()
	}
	if r.running {
		if !r.executing || r.pending {
			r.mu.Unlock()
			return scheduler.JobCoalesced, nil
		}
		r.pending = true
		r.mu.Unlock()
		return scheduler.JobPending, nil
	}
	r.running = true
	r.mu.Unlock()
	go r.runManual()
	return scheduler.JobStarted, nil
}

// RunScheduled is the registered job body, invoked only after the scheduler
// has acquired its gate. Rebuild requests arriving during a pass remain set
// for the scheduler's follow-up pass.
func (r *Runner) RunScheduled(ctx context.Context) error {
	r.mu.Lock()
	rebuild := r.rebuild
	r.rebuild = false
	r.mu.Unlock()
	return r.run(ctx, rebuild)
}

func (r *Runner) run(ctx context.Context, rebuild bool) error {
	if rebuild {
		if err := r.opts.Rebuild(ctx); err != nil {
			return err
		}
	}
	return r.opts.Run(ctx)
}

func (r *Runner) runManual() {
	for {
		done, ok := r.opts.BeginWork(r.opts.Context, "open-loops")
		if !ok {
			r.mu.Lock()
			r.running, r.executing, r.pending = false, false, false
			r.mu.Unlock()
			return
		}
		r.mu.Lock()
		r.executing = true
		rebuild := r.rebuild
		r.rebuild = false
		r.mu.Unlock()
		ctx, cancel := context.WithTimeout(r.opts.Context, 2*time.Minute)
		err := r.run(ctx, rebuild)
		cancel()
		if r.opts.AfterRun != nil {
			r.opts.AfterRun(r.opts.Context)
		}
		done()
		if err != nil && !errors.Is(err, context.Canceled) {
			if r.opts.OnError != nil {
				r.opts.OnError(err)
			} else {
				slog.Error("open loops pass failed", "error", err)
			}
		}
		r.mu.Lock()
		r.executing = false
		if !r.pending || r.opts.Context.Err() != nil {
			r.running, r.pending = false, false
			r.mu.Unlock()
			return
		}
		r.pending = false
		r.mu.Unlock()
	}
}
