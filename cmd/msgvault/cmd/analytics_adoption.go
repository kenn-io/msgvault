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
// server off live-SQL fallback without a restart, and demote it back when
// the cache disappears. runServe registers the server; the state stays empty
// in plain CLI processes, which makes maybeAdoptAnalyticsCache a no-op there.
// Adopted DuckDB engines are kept until shutdown — in-flight requests may
// still hold a replaced engine, so none is closed while the server runs.
var analyticsAdoption struct {
	mu      sync.Mutex
	server  *api.Server
	cfg     *config.Config
	store   *store.Store
	adopted []*query.DuckDBEngine
}

func registerAnalyticsCacheAdoption(server *api.Server, c *config.Config, s *store.Store) {
	analyticsAdoption.mu.Lock()
	defer analyticsAdoption.mu.Unlock()
	analyticsAdoption.server = server
	analyticsAdoption.cfg = c
	analyticsAdoption.store = s
}

// shutdownAnalyticsCacheAdoption unregisters the daemon and closes any
// adopted DuckDB engines. Call it after the API server has shut down so no
// in-flight request still holds one.
func shutdownAnalyticsCacheAdoption() {
	analyticsAdoption.mu.Lock()
	defer analyticsAdoption.mu.Unlock()
	analyticsAdoption.server = nil
	analyticsAdoption.cfg = nil
	analyticsAdoption.store = nil
	for _, engine := range analyticsAdoption.adopted {
		_ = engine.Close()
	}
	analyticsAdoption.adopted = nil
}

// maybeAdoptAnalyticsCache reconciles the daemon's analytics engine with the
// on-disk cache after cache-affecting work. A daemon on live-SQL fallback
// adopts a fresh, complete cache after a successful build, so aggregate
// views speed up without a restart; a stale-again cache (e.g. a sync landed
// mid-build) is left for the next build, since live SQL is at least as
// current. A daemon on DuckDB whose cache files were removed (remove-account
// clears the analytics directory) demotes back to live SQL so aggregate
// views keep working, and re-adopts after the next successful build. Only
// [analytics] engine="auto" reconciles: "sql" is a deliberate live-SQL
// choice and "duckdb" is documented to never fall back.
func maybeAdoptAnalyticsCache() {
	analyticsAdoption.mu.Lock()
	defer analyticsAdoption.mu.Unlock()
	server, c, s := analyticsAdoption.server, analyticsAdoption.cfg, analyticsAdoption.store
	if server == nil || c == nil || s == nil {
		return
	}
	if engineMode := c.Analytics.Engine; engineMode != "" && engineMode != config.AnalyticsEngineAuto {
		return
	}
	mode := server.AnalyticsMode()
	if mode != api.AnalyticsModeSQLFallback && mode != api.AnalyticsModeDuckDB {
		return
	}
	analyticsDir := c.AnalyticsDir()
	// Hold the inter-process build lock across the freshness check and the
	// engine swap so a build running in another process (e.g. a daemon-owned
	// CLI child's rebuildCacheAfterWrite) cannot hand us a half-written
	// cache or trigger a spurious demotion mid-rebuild. A held lock means
	// that build's completion path retries the reconciliation.
	buildLock, err := cacheBuildFileLock(analyticsDir)
	if err != nil {
		logger.Warn("analytics engine reconciliation skipped", "error", err)
		return
	}
	if locked, err := buildLock.TryLock(); err != nil || !locked {
		logger.Debug("analytics engine reconciliation deferred; a cache build is in progress elsewhere")
		return
	}
	defer func() { _ = buildLock.Unlock() }()

	// A complete-looking Parquet layout is only trustworthy together with a
	// valid sync state: builds invalidate the state before their first cache
	// mutation, so a build killed mid-export can leave every directory
	// populated while some tables lack the new rows.
	consistent := query.HasCompleteParquetData(analyticsDir) &&
		hasValidCacheSyncState(analyticsDir)
	if mode == api.AnalyticsModeDuckDB {
		if consistent {
			return
		}
		server.SetAnalyticsEngine(query.NewEngine(s.DB(), false), api.AnalyticsModeSQLFallback)
		logger.Warn("analytics cache files missing or sync state invalid; aggregate views demoted to live SQL until the next cache build")
		return
	}

	staleness := cacheNeedsBuild(c.DatabaseDSN(), analyticsDir)
	if staleness.NeedsBuild || !consistent {
		reason := staleness.Reason
		if reason == "" {
			reason = "cache files missing or incomplete"
		}
		logger.Debug("analytics cache not adopted; staying on live SQL",
			"reason", reason)
		return
	}
	duckEngine, err := openDaemonDuckDBEngine(c, s)
	if err != nil {
		logger.Warn("analytics cache adoption failed to open DuckDB engine; staying on live SQL",
			"error", err)
		return
	}
	analyticsAdoption.adopted = append(analyticsAdoption.adopted, duckEngine)
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
			} else {
				logger.Info("background analytics cache build completed")
			}
		}
		// Reconcile regardless of build outcome; on failure this is a no-op
		// for a fallback daemon but keeps the engine honest either way.
		maybeAdoptAnalyticsCache()
	}()
}
