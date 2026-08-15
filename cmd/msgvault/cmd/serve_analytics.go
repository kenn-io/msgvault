package cmd

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

// daemonAnalyticsInitHandle tracks the background cache maintenance started
// after the HTTP listener is accepting requests. Its fatal channel is only
// used for required DuckDB initialization failures; automatic mode keeps its
// initial SQL engine when maintenance fails.
type daemonAnalyticsInitHandle struct {
	started chan struct{}
	done    chan struct{}
	fatal   chan error

	mu      sync.Mutex
	err     error
	swapped bool
}

func newDaemonAnalyticsInitHandle() *daemonAnalyticsInitHandle {
	return &daemonAnalyticsInitHandle{
		started: make(chan struct{}),
		done:    make(chan struct{}),
		fatal:   make(chan error, 1),
	}
}

func completedDaemonAnalyticsInitHandle() *daemonAnalyticsInitHandle {
	h := newDaemonAnalyticsInitHandle()
	close(h.started)
	close(h.done)
	return h
}

func (h *daemonAnalyticsInitHandle) WaitStarted(ctx context.Context) bool {
	if h == nil {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-h.started:
		return true
	case <-ctx.Done():
		return false
	}
}

// WaitContext reports whether the initializer completed before ctx ended.
// The final done check makes a simultaneous completion and timeout deterministic
// in favor of completion, so the daemon never closes an engine while the
// initializer is still able to install it.
func (h *daemonAnalyticsInitHandle) WaitContext(ctx context.Context) bool {
	if h == nil {
		return true
	}
	select {
	case <-h.done:
		return true
	default:
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-h.done:
		return true
	case <-ctx.Done():
		select {
		case <-h.done:
			return true
		default:
			return false
		}
	}
}

func (h *daemonAnalyticsInitHandle) Fatal() <-chan error {
	if h == nil {
		return nil
	}
	return h.fatal
}

func (h *daemonAnalyticsInitHandle) setError(err error) {
	if h == nil || err == nil {
		return
	}
	h.mu.Lock()
	h.err = err
	h.mu.Unlock()
	select {
	case h.fatal <- err:
	default:
	}
}

func (h *daemonAnalyticsInitHandle) setSwapped() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.swapped = true
	h.mu.Unlock()
}

func (h *daemonAnalyticsInitHandle) Err() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func (h *daemonAnalyticsInitHandle) Swapped() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.swapped
}

// prepareDaemonAnalyticsEngine creates the state that can be installed before
// the scheduler and HTTP server start. SQL and PostgreSQL need no cache
// maintenance and retain the old synchronous selection. Auto and DuckDB defer
// cache inspection/build/open until the listener is already serving.
func prepareDaemonAnalyticsEngine(
	ctx context.Context,
	c *config.Config,
	s *store.Store,
	intent startupCacheBuildIntent,
) (query.Engine, string, startupCacheBuildOutcome, bool, error) {
	if c == nil || s == nil {
		return nil, "", startupCacheBuildOutcomeNone, false,
			errors.New("daemon analytics engine unavailable")
	}

	engineMode := c.Analytics.Engine
	if engineMode == "" {
		engineMode = config.AnalyticsEngineAuto
	}
	if s.IsPostgreSQL() || engineMode == config.AnalyticsEngineSQL {
		engine, mode, outcome, err := openDaemonAnalyticsEngine(ctx, c, s, intent)
		return engine, mode, outcome, false, err
	}

	switch engineMode {
	case config.AnalyticsEngineAuto:
		logger.Info("using live SQL analytics engine while cache initializes",
			"engine", engineMode)
		return query.NewEngine(s.DB(), false), api.AnalyticsModeSQLFallback,
			startupCacheBuildOutcomeNone, true, nil
	case config.AnalyticsEngineDuckDB:
		logger.Info("DuckDB analytics unavailable while required cache initializes",
			"engine", engineMode)
		// Keep SQLite-backed detail, search, attachment, and text routes live.
		// The API mode gates DuckDB-dependent analytics until the initializer
		// atomically replaces this engine.
		return query.NewEngine(s.DB(), false), api.AnalyticsModeInitializing,
			startupCacheBuildOutcomeNone, true, nil
	default:
		// Config validation normally rejects this before runServe. Keep the old
		// final-selection behavior as a defensive fallback for embedded callers.
		engine, mode, outcome, err := openDaemonAnalyticsEngine(ctx, c, s, intent)
		return engine, mode, outcome, false, err
	}
}

// startDaemonAnalyticsInitializer performs final cache selection after the
// HTTP listener starts. The operation tracker prevents scheduled/API writes
// from racing cache export and keeps an idle-shutdown daemon alive until the
// worker exits.
func startDaemonAnalyticsInitializer(
	ctx context.Context,
	c *config.Config,
	s *store.Store,
	intent startupCacheBuildIntent,
	apiServer *api.Server,
	ownership *serveOwnership,
	tracker scheduler.WorkTracker,
) *daemonAnalyticsInitHandle {
	h := newDaemonAnalyticsInitHandle()
	apiServer.SetAnalyticsInitializationActive(true)
	go func() {
		defer close(h.done)
		defer apiServer.SetAnalyticsInitializationActive(false)
		if tracker != nil {
			release, ok := tracker.BeginWorkContext(ctx)
			close(h.started)
			if !ok {
				logger.Info("analytics cache initialization aborted", "reason", "daemon shutting down")
				return
			}
			defer release()
		} else {
			close(h.started)
		}

		logger.Info("daemon startup step", "step", "init_analytics_engine")
		engine, mode, outcome, err := openDaemonAnalyticsEngine(ctx, c, s, intent)
		if ctx.Err() != nil {
			if engine != nil {
				_ = engine.Close()
			}
			return
		}

		if err != nil {
			if engine != nil {
				_ = engine.Close()
			}
			if intent != startupCacheBuildIntentNone {
				if outcomeErr := ownership.SetStartupCacheBuildOutcome(outcome); outcomeErr != nil {
					h.setError(outcomeErr)
					return
				}
			}
			logger.Error("daemon startup analytics initialization failed", "error", err)
			h.setError(err)
			return
		}

		if mode == api.AnalyticsModeDuckDB {
			if engine == nil {
				err := errors.New("DuckDB analytics initialization returned no engine")
				if intent != startupCacheBuildIntentNone {
					_ = ownership.SetStartupCacheBuildOutcome(startupCacheBuildOutcomeFatal)
				}
				h.setError(err)
				return
			}
			apiServer.SetAnalyticsEngine(engine, mode)
			h.setSwapped()
		} else if engine != nil {
			// Auto mode returns a newly-created SQL fallback when maintenance
			// fails or is disabled. Keep the original live engine installed so
			// requests do not observe a needless engine replacement.
			_ = engine.Close()
		}

		if intent != startupCacheBuildIntentNone {
			if outcomeErr := ownership.SetStartupCacheBuildOutcome(outcome); outcomeErr != nil {
				h.setError(outcomeErr)
				return
			}
		}
		logger.Info("daemon startup step complete", "step", "init_analytics_engine")
	}()
	return h
}

// closeDaemonAnalyticsEngines closes the current API engine only after HTTP
// shutdown and then closes the original fallback when a DuckDB swap occurred.
// SetAnalyticsEngine intentionally leaves the previous engine open, so both
// daemon-owned engines must be closed here.
func closeDaemonAnalyticsEngines(
	apiServer *api.Server,
	initialEngine query.Engine,
	initializer *daemonAnalyticsInitHandle,
) error {
	if initializer != nil {
		select {
		case <-initializer.done:
		default:
			return errors.New("analytics initialization is still running")
		}
	}
	var errs []error
	if apiServer != nil {
		if err := apiServer.CloseAnalyticsEngine(); err != nil {
			errs = append(errs, fmt.Errorf("close analytics engine: %w", err))
		}
	}
	if initializer != nil && initializer.Swapped() && initialEngine != nil {
		if err := initialEngine.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close initial analytics engine: %w", err))
		}
	}
	return errors.Join(errs...)
}

// closeDaemonStoreAfterInitializers avoids closing the archive database while
// either background initializer can still be using it. A shutdown timeout
// skips deferred cleanup; the process is exiting, and keeping the handle open
// is safer than racing a worker that has not joined.
func closeDaemonStoreAfterInitializers(
	s *store.Store,
	analyticsInitializer *daemonAnalyticsInitHandle,
	vectorInitializer *vectorInitHandle,
) error {
	if s == nil {
		return nil
	}
	if analyticsInitializer != nil {
		select {
		case <-analyticsInitializer.done:
		default:
			return errors.New("analytics initialization is still running")
		}
	}
	if vectorInitializer != nil {
		select {
		case <-vectorInitializer.done:
		default:
			return errors.New("vector initialization is still running")
		}
	}
	return s.Close()
}
