package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/visual"
)

// VectorStatus describes the daemon's vector-search subsystem state. The
// serve daemon starts with `initializing` and flips to `ready` or `error`
// when the background init finishes; non-daemon servers derive `ready` or
// `disabled` from whether a backend was supplied at construction.
type VectorStatus string

const (
	VectorStatusDisabled     VectorStatus = "disabled"
	VectorStatusInitializing VectorStatus = "initializing"
	VectorStatusReady        VectorStatus = "ready"
	VectorStatusError        VectorStatus = "error"
	// VectorStatusStale means the backend initialized fine, but the active
	// index's fingerprint does not match the configured embedding
	// model/dimension/preprocess policy, so vector search returns
	// index_stale 503s until the index is rebuilt. It is evaluated once at
	// init completion using the same staleness check the query path runs.
	VectorStatusStale VectorStatus = "stale"
)

// SetVectorFeatures atomically installs every vector search component before
// publishing ready. The daemon uses this path so a request can never observe
// ready vector status before semantic person search exists.
func (s *Server) SetVectorFeatures(
	engine *hybrid.Engine,
	personEngine PersonSearchEngine,
	backend vector.Backend,
	cfg vector.Config,
) {
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.hybridEngine = engine
	s.personSearchEngine = personEngine
	s.backend = backend
	s.vectorCfg = cfg
	s.vectorStatus = VectorStatusReady
	s.vectorErr = ""
	s.vectorStaleLatch = false
	s.vectorFreshNextCheck = time.Time{}
}

func (s *Server) SetVisualSearch(service *visual.SearchService) {
	s.vectorMu.Lock()
	s.visualSearch = service
	if !s.vectorCfg.Enabled && service != nil {
		s.vectorStatus = VectorStatusReady
		s.vectorErr = ""
	}
	s.vectorMu.Unlock()
}

func (s *Server) SetVisualOperations(
	build func(context.Context, operations.PassScope) error,
	run func(context.Context, operations.PassScope) error,
	retry func(context.Context, operations.PassScope, int64, string) error,
	status func(context.Context, bool) (visual.Status, error),
	retire func(context.Context) error,
) {
	s.vectorMu.Lock()
	s.visualBuild = build
	s.visualRun = run
	s.visualRetry = retry
	s.visualStatus = status
	s.visualRetire = retire
	s.vectorMu.Unlock()
}

// SetVectorInitError marks the vector subsystem as failed. The daemon keeps
// serving; vector endpoints return 503 carrying the message. Calling with a
// nil error is a programmer error — there is nothing to report — and is a
// no-op: it does not flip the status to error or touch any existing state.
func (s *Server) SetVectorInitError(err error) {
	if err == nil {
		return
	}
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.vectorStatus = VectorStatusError
	s.vectorErr = err.Error()
}

// SetVectorStale marks the vector subsystem as stale: the backend
// initialized and its components are installed, but the active index does
// not match the configured embedding settings, so vector searches return
// index_stale until config is aligned or the index is rebuilt. detail should
// name the stored vs configured fingerprint and the recovery options. Calling with an
// empty detail is a no-op — there is nothing actionable to report.
func (s *Server) SetVectorStale(detail string) {
	if detail == "" {
		return
	}
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.vectorStatus = VectorStatusStale
	s.vectorErr = detail
}

// SetVectorScopeDrift marks the vector subsystem stale because the durable
// [vector.embed.scope] accounts now resolve to a different source-ID set
// than the installed components were initialized with. Unlike SetVectorStale,
// the resulting status is LATCHED: the active generation still matches the
// startup fingerprint, so refreshVectorStatusIfStale would otherwise clear
// the status on the next request while searches keep serving the
// wrongly-scoped index. Only a successful reinit (SetVectorFeatures — in
// practice a daemon restart) clears it. An empty detail is a no-op.
func (s *Server) SetVectorScopeDrift(detail string) {
	if detail == "" {
		return
	}
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.vectorStatus = VectorStatusStale
	s.vectorErr = detail
	s.vectorStaleLatch = true
}

// refreshVectorStatusIfStale re-runs the generation freshness check when the
// cached status is stale. The stale status is latched once at init completion,
// so without this a `--full-rebuild` (or the daemon embed job) that reactivates
// a matching generation would keep /health, /api/v1/stats, and serve status
// reporting stale until a daemon restart. Cheap no-op unless the status is
// currently stale; leaves the status untouched on a still-stale, building, or
// transient-error result.
func (s *Server) refreshVectorStatusIfStale(ctx context.Context) {
	s.vectorMu.RLock()
	stale := s.vectorStatus == VectorStatusStale
	latched := s.vectorStaleLatch
	backend := s.backend
	cfg := s.vectorCfg
	s.vectorMu.RUnlock()
	// A latched stale (embedding scope drift) matches the startup
	// fingerprint by construction, so a clean resolve proves nothing;
	// only SetVectorFeatures (reinit) may clear it.
	if !stale || latched || backend == nil {
		return
	}
	if _, err := vector.ResolveActiveForFingerprint(ctx, backend, cfg.GenerationFingerprint()); err != nil {
		// Still stale, still building, or a transient backend error: leave the
		// stale status in place. Only a clean resolve clears it.
		return
	}
	s.vectorMu.Lock()
	// Guard against clobbering a status a concurrent writer changed (e.g. a
	// reinit that flipped to error, or a scope-drift latch) while we held
	// no lock.
	if s.vectorStatus == VectorStatusStale && !s.vectorStaleLatch {
		s.vectorStatus = VectorStatusReady
		s.vectorErr = ""
	}
	s.vectorMu.Unlock()
}

// vectorScopeCheckInterval throttles the preflight scope-drift check: the
// resolution queries the main DB per configured account, so it runs at most
// once per interval across all search requests.
const vectorScopeCheckInterval = time.Minute

// SetVectorScopeCheck installs a callback that re-resolves the durable
// [vector.embed.scope] accounts and returns a non-empty detail when they no
// longer resolve to the source set vector search was initialized with. The
// preflight on every vector-search entry point runs it (throttled), so
// drift latches the stale status even on daemons whose embed job never
// fires (empty cron, run_after_sync=false). A nil check disables the
// preflight detection; the embed job's own detection is unaffected.
func (s *Server) SetVectorScopeCheck(check func(ctx context.Context) (string, error)) {
	s.vectorMu.Lock()
	defer s.vectorMu.Unlock()
	s.vectorScopeCheck = check
	s.vectorScopeNextCheck = time.Time{}
}

// maybeCheckVectorScopeDrift runs the installed scope-drift check when one
// is due. A resolution failure (possibly transient) is logged and does not
// change the status; only a positive drift result latches stale.
func (s *Server) maybeCheckVectorScopeDrift(ctx context.Context) {
	s.vectorMu.Lock()
	check := s.vectorScopeCheck
	if check == nil || s.vectorStaleLatch || time.Now().Before(s.vectorScopeNextCheck) {
		s.vectorMu.Unlock()
		return
	}
	s.vectorScopeNextCheck = time.Now().Add(vectorScopeCheckInterval)
	s.vectorMu.Unlock()
	detail, err := check(ctx)
	if err != nil {
		s.logger.Warn("embedding scope drift check failed", "error", err)
		return
	}
	if detail != "" {
		// The public /health view hides the detail (it can name configured
		// account identifiers), so log it for operators here.
		s.logger.Warn("embedding scope drift detected; vector search marked stale", "detail", detail)
		s.SetVectorScopeDrift(detail)
	}
}

// vectorFreshnessCheckInterval throttles maybeRefreshVectorFreshness: the
// check costs one backend generation lookup and runs on health, stats, and
// search preflight paths, so it fires at most once per interval.
const vectorFreshnessCheckInterval = time.Minute

// maybeRefreshVectorFreshness is the ready→stale counterpart of
// refreshVectorStatusIfStale. A daemon-proxied one-off scoped build (or any
// CLI rebuild under a different configuration) can activate a generation
// whose fingerprint differs from the installed one WITHOUT changing the
// configured scope, so the drift latch never fires: searches then 503 with
// index_stale from the query-time fingerprint check while /health and
// /api/v1/stats keep reporting ready. Re-run the freshness check (throttled)
// while the status is ready and flip it to stale on a fingerprint mismatch.
// Only ErrIndexStale flips the status — building or transient backend errors
// are not the "index does not match configured settings" condition this
// exists to expose. The plain (unlatched) stale it sets clears through
// refreshVectorStatusIfStale once a matching generation activates.
func (s *Server) maybeRefreshVectorFreshness(ctx context.Context) {
	s.vectorMu.Lock()
	backend := s.backend
	cfg := s.vectorCfg
	due := s.vectorStatus == VectorStatusReady && backend != nil && cfg.Enabled &&
		!time.Now().Before(s.vectorFreshNextCheck)
	if due {
		s.vectorFreshNextCheck = time.Now().Add(vectorFreshnessCheckInterval)
	}
	s.vectorMu.Unlock()
	if !due {
		return
	}
	_, err := vector.ResolveActiveForFingerprint(ctx, backend, cfg.GenerationFingerprint())
	if !errors.Is(err, vector.ErrIndexStale) {
		return
	}
	s.SetVectorStale(err.Error() + "; if this is a one-off account-scoped generation, set matching [vector.embed.scope] accounts and restart the daemon; otherwise run `msgvault embeddings build --full-rebuild` to rebuild")
}

// refreshVectorStatus runs every throttled revalidation that keeps the
// reported vector status honest, in dependency order: the account-scope
// drift check (config vs initialized scope), the fingerprint freshness
// check (ready→stale after a foreign activation), and the clearable stale
// refresh (stale→ready once a matching generation activates). Every
// status-reporting or status-gated path — health, stats, coverage, and the
// search preflight — must call this rather than a subset, so that on a
// daemon whose embed job never runs, any of those endpoints can be the
// event that detects a changed scope or a swapped generation.
func (s *Server) refreshVectorStatus(ctx context.Context) {
	s.maybeCheckVectorScopeDrift(ctx)
	s.maybeRefreshVectorFreshness(ctx)
	s.refreshVectorStatusIfStale(ctx)
}

// vectorSearchPreflight gates a vector-search entry point on the stale
// status. The installed components validate the STARTUP fingerprint at
// query time, which still matches after embedding-scope drift, so without
// this gate searches would keep serving a wrongly-scoped index that health
// already reports as stale. Runs the throttled revalidations first (so a
// search can be the event that detects drift) and writes the index_stale
// 503 when the status is (still) stale. Returns false when the response
// has been written.
func (s *Server) vectorSearchPreflight(ctx context.Context, w http.ResponseWriter) bool {
	s.refreshVectorStatus(ctx)
	if status, _ := s.VectorStatus(); status == VectorStatusStale {
		s.writeVectorUnavailable(w)
		return false
	}
	return true
}

// VectorStatus returns the vector subsystem status and, when the status is
// VectorStatusError or VectorStatusStale, the associated detail message.
func (s *Server) VectorStatus() (VectorStatus, string) {
	s.vectorMu.RLock()
	defer s.vectorMu.RUnlock()
	return s.vectorStatus, s.vectorErr
}

// vectorHealth returns the health-response view of the vector subsystem,
// or nil when vector search is disabled. The detail message can carry
// operator-sensitive material — account-resolution errors name configured
// mailbox identifiers — so this detailed form is served only behind the
// API-key boundary (/api/v1/health).
func (s *Server) vectorHealth() *VectorHealth {
	status, errMsg := s.VectorStatus()
	if status == VectorStatusDisabled {
		return nil
	}
	return &VectorHealth{Status: string(status), Error: errMsg}
}

// vectorHealthPublic is the unauthenticated /health view: it reports the
// status (enough for monitoring) but replaces the detail with a generic
// pointer, because init and scope-drift details embed configured account
// identifiers from resolver errors.
func (s *Server) vectorHealthPublic() *VectorHealth {
	status, _ := s.VectorStatus()
	if status == VectorStatusDisabled {
		return nil
	}
	health := &VectorHealth{Status: string(status)}
	if status == VectorStatusError || status == VectorStatusStale {
		health.Error = "vector search is unavailable; see the authenticated health endpoint or daemon logs for details"
	}
	return health
}

func (s *Server) vectorComponents() (*hybrid.Engine, vector.Backend, vector.Config) {
	s.vectorMu.RLock()
	defer s.vectorMu.RUnlock()
	return s.hybridEngine, s.backend, s.vectorCfg
}

func (s *Server) personSearchComponent() PersonSearchEngine {
	s.vectorMu.RLock()
	defer s.vectorMu.RUnlock()
	return s.personSearchEngine
}

// writeVectorUnavailable reports why vector search cannot serve a request
// right now: still initializing (daemon background migration), failed to
// initialize, or simply not enabled.
func (s *Server) writeVectorUnavailable(w http.ResponseWriter) {
	status, errMsg := s.VectorStatus()
	switch status {
	case VectorStatusInitializing:
		writeError(w, http.StatusServiceUnavailable, "vector_initializing",
			"vector search is initializing (schema migration or backfill in progress); retry shortly")
	case VectorStatusError:
		writeError(w, http.StatusServiceUnavailable, "vector_init_failed",
			"vector search failed to initialize: "+errMsg)
	case VectorStatusStale:
		writeError(w, http.StatusServiceUnavailable, "index_stale", errMsg)
	default:
		writeError(w, http.StatusServiceUnavailable, "vector_not_enabled",
			"vector search is not configured on this server")
	}
}
