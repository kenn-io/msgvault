package cmd

import (
	"context"
	"sync"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

// analyticsAdoption lets daemon cache-build completions upgrade the API
// server off live-SQL fallback without a restart. runServe registers the
// server; the state stays empty in plain CLI processes, which makes
// maybeAdoptAnalyticsCache a no-op there. At most one adoption ever happens:
// the mode leaves sql-fallback permanently once the DuckDB engine installs.
var analyticsAdoption struct {
	mu      sync.Mutex
	server  *api.Server
	cfg     *config.Config
	store   *store.Store
	adopted *query.DuckDBEngine
}

func registerAnalyticsCacheAdoption(server *api.Server, c *config.Config, s *store.Store) {
	analyticsAdoption.mu.Lock()
	defer analyticsAdoption.mu.Unlock()
	analyticsAdoption.server = server
	analyticsAdoption.cfg = c
	analyticsAdoption.store = s
}

// shutdownAnalyticsCacheAdoption unregisters the daemon and closes any
// adopted DuckDB engine. Call it after the API server has shut down so no
// in-flight request still holds the engine.
func shutdownAnalyticsCacheAdoption() {
	analyticsAdoption.mu.Lock()
	defer analyticsAdoption.mu.Unlock()
	analyticsAdoption.server = nil
	analyticsAdoption.cfg = nil
	analyticsAdoption.store = nil
	if analyticsAdoption.adopted != nil {
		_ = analyticsAdoption.adopted.Close()
		analyticsAdoption.adopted = nil
	}
}

// maybeAdoptAnalyticsCache upgrades a daemon running on live-SQL fallback
// onto the Parquet cache after a successful cache build, so aggregate views
// speed up without a restart. Only [analytics] engine="auto" adopts: "sql"
// is a deliberate live-SQL choice and "duckdb" never falls back. The cache
// must be fresh and complete at adoption time — a cache that went stale
// again (e.g. a sync landed mid-build) is left for the next build to adopt,
// since live SQL is at least as current.
func maybeAdoptAnalyticsCache() {
	analyticsAdoption.mu.Lock()
	defer analyticsAdoption.mu.Unlock()
	server, c, s := analyticsAdoption.server, analyticsAdoption.cfg, analyticsAdoption.store
	if server == nil || c == nil || s == nil || analyticsAdoption.adopted != nil {
		return
	}
	if server.AnalyticsMode() != api.AnalyticsModeSQLFallback {
		return
	}
	if engineMode := c.Analytics.Engine; engineMode != "" && engineMode != config.AnalyticsEngineAuto {
		return
	}
	analyticsDir := c.AnalyticsDir()
	staleness := cacheNeedsBuild(c.DatabaseDSN(), analyticsDir)
	if staleness.NeedsBuild || !query.HasCompleteParquetData(analyticsDir) {
		reason := staleness.Reason
		if reason == "" {
			reason = "cache files missing or incomplete"
		}
		logger.Info("analytics cache not adopted after build; staying on live SQL",
			"reason", reason)
		return
	}
	duckEngine, err := openDaemonDuckDBEngine(c, s)
	if err != nil {
		logger.Warn("analytics cache adoption failed to open DuckDB engine; staying on live SQL",
			"error", err)
		return
	}
	analyticsAdoption.adopted = duckEngine
	server.SetAnalyticsEngine(duckEngine, api.AnalyticsModeDuckDB)
	logger.Info("analytics engine upgraded to DuckDB over the Parquet cache")
}

// startAnalyticsCacheAutoBuild runs the deferred startup cache build in the
// background when the daemon fell back to live SQL over a buildable cache
// ([analytics] engine="auto" with auto_build_cache=true). The tracker keeps
// a background daemon from idle-stopping mid-build. The build intentionally
// does not hold the operation gate: it only reads the archive and writes the
// analytics directory, and holding the gate would block the very command
// that auto-started the daemon; build subprocesses already serialize among
// themselves.
func startAnalyticsCacheAutoBuild(
	ctx context.Context,
	c *config.Config,
	apiServer *api.Server,
	tracker scheduler.WorkTracker,
) {
	go func() {
		defer apiServer.SetAnalyticsCacheBuilding(false)
		if tracker != nil {
			done, ok := tracker.BeginWorkContext(ctx)
			if !ok {
				return
			}
			defer done()
		}
		staleness := cacheNeedsBuild(c.DatabaseDSN(), c.AnalyticsDir())
		if staleness.NeedsBuild {
			logger.Info("building analytics cache in background",
				"reason", staleness.Reason,
				"full_rebuild", staleness.FullRebuild)
			if err := buildCacheSubprocessForRun(ctx, staleness.FullRebuild); err != nil {
				logger.Warn("background analytics cache build failed; aggregate views stay on live SQL",
					"error", err)
				return
			}
			logger.Info("background analytics cache build completed")
		}
		maybeAdoptAnalyticsCache()
	}()
}
