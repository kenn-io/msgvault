package cmd

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
)

type beeperEventScheduler interface {
	StartJob(name string, manual bool) (scheduler.JobDisposition, error)
}

func markBeeperDirty(dirty chan<- struct{}) {
	select {
	case dirty <- struct{}{}:
	default:
	}
}

// runBeeperWakeups uses a fixed window, not a trailing debounce: a busy source
// cannot postpone admission indefinitely. Failed admission retains dirty work.
func runBeeperWakeups(ctx context.Context, dirty <-chan struct{}, start func() error) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-dirty:
		}
		timer := time.NewTimer(time.Second)
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if ctx.Err() != nil {
				return
			}
			// Everything already signaled is covered by this admission. A hint arriving
			// during StartJob remains queued for the next fixed window.
			select {
			case <-dirty:
			default:
			}
			if start() == nil {
				break
			}
			timer.Reset(time.Second)
		}
	}
}

// startBeeperEvents returns a cancel-and-join function even when disabled. The
// caller stops it before scheduler drain and before closing the archive Store.
func startBeeperEvents(ctx context.Context, cfg *config.Config, sched beeperEventScheduler, registered bool, logger *slog.Logger) func() {
	if !cfg.Beeper.Enabled || !cfg.Beeper.Events || cfg.Beeper.Schedule == "" || !registered {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	dirty := make(chan struct{}, 1)
	client := beeper.NewClient(cfg.Beeper.URL, func(context.Context) (string, error) { return beeper.LoadToken(cfg.TokensDir()) }, cfg.Beeper.RateLimitQPS)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		runBeeperWakeups(ctx, dirty, func() error { _, err := sched.StartJob(api.BeeperJobName, false); return err })
	}()
	go func() {
		defer workers.Done()
		lastState := ""
		_ = client.WatchEvents(ctx, func() { markBeeperDirty(dirty) }, func(err error) {
			state := "subscribed"
			if err != nil {
				state = "degraded"
			}
			if state == lastState {
				return
			}
			lastState = state
			if err == nil {
				logger.Info("Beeper events subscribed; HTTP catch-up requested")
			} else {
				logger.Warn("Beeper events unavailable; polling continues", "error", err)
			}
		})
	}()
	return func() { cancel(); workers.Wait() }
}
