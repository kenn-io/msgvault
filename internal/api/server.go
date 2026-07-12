// Package api provides the HTTP API server for msgvault.
package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

// MessageStore defines the store operations the API needs.
type MessageStore interface {
	GetStats() (*StoreStats, error)
	ListMessages(offset, limit int) ([]APIMessage, int64, error)
	GetMessage(id int64) (*APIMessage, error)
	GetMessagesSummariesByIDs(ids []int64) ([]APIMessage, error)
	SearchMessages(query string, offset, limit int) ([]APIMessage, int64, error)
	SearchMessagesQuery(q *search.Query, offset, limit int) ([]APIMessage, int64, error)
}

// ctxMessageSearcher is an optional extension of MessageStore for stores that
// accept a context on the search path. handleSearch prefers it so an
// abandoned or timed-out request cancels the underlying query instead of
// running it to completion. Stores that predate it still satisfy MessageStore
// and fall back to the non-context methods.
type ctxMessageSearcher interface {
	SearchMessagesContext(ctx context.Context, query string, offset, limit int) ([]APIMessage, int64, error)
	SearchMessagesQueryContext(ctx context.Context, q *search.Query, offset, limit int) ([]APIMessage, int64, error)
}

// CtxMessageStore is an optional extension of MessageStore for stores that
// accept a context on the non-search read paths. Request handlers prefer it so
// the request_id carried on r.Context() (via store.WithRequestID) reaches every
// request-owned SQL query for slow/error logging, and so an abandoned request
// cancels the underlying queries. Stores that predate it fall back to the
// non-context methods.
type CtxMessageStore interface {
	GetStatsContext(ctx context.Context) (*StoreStats, error)
	ListMessagesContext(ctx context.Context, offset, limit int) ([]APIMessage, int64, error)
	GetMessageContext(ctx context.Context, id int64) (*APIMessage, error)
	GetMessagesSummariesByIDsContext(ctx context.Context, ids []int64) ([]APIMessage, error)
}

// getStats calls the context-aware store variant when available, so
// request-owned stats queries carry the request context.
func (s *Server) getStats(ctx context.Context) (*StoreStats, error) {
	if cs, ok := s.store.(CtxMessageStore); ok {
		return cs.GetStatsContext(ctx)
	}
	return s.store.GetStats()
}

// listMessages calls the context-aware store variant when available.
func (s *Server) listMessages(ctx context.Context, offset, limit int) ([]APIMessage, int64, error) {
	if cs, ok := s.store.(CtxMessageStore); ok {
		return cs.ListMessagesContext(ctx, offset, limit)
	}
	return s.store.ListMessages(offset, limit)
}

// getMessage calls the context-aware store variant when available.
func (s *Server) getMessage(ctx context.Context, id int64) (*APIMessage, error) {
	if cs, ok := s.store.(CtxMessageStore); ok {
		return cs.GetMessageContext(ctx, id)
	}
	return s.store.GetMessage(id)
}

// getMessagesSummariesByIDs calls the context-aware store variant when available.
func (s *Server) getMessagesSummariesByIDs(ctx context.Context, ids []int64) ([]APIMessage, error) {
	if cs, ok := s.store.(CtxMessageStore); ok {
		return cs.GetMessagesSummariesByIDsContext(ctx, ids)
	}
	return s.store.GetMessagesSummariesByIDs(ids)
}

// SourceStatusStore defines the source/sync read operations used by the
// source status endpoint.
type SourceStatusStore interface {
	ListSources(sourceType string) ([]*store.Source, error)
	GetActiveSync(sourceID int64) (*store.SyncRun, error)
	GetLatestSync(sourceID int64) (*store.SyncRun, error)
	GetLastSuccessfulSync(sourceID int64) (*store.SyncRun, error)
	CountSyncRunItems(syncRunID int64, status string) (int64, error)
	ListSyncRunItems(syncRunID int64, status string, limit int) ([]store.SyncRunItem, error)
}

// StoreStats is an alias for store.Stats — single source of truth.
type StoreStats = store.Stats

// SyncScheduler defines the scheduler operations the API needs.
type SyncScheduler interface {
	IsScheduled(email string) bool
	TriggerSync(email string) error
	AddAccount(email, schedule string) error
	Status() []AccountStatus
	IsRunning() bool
}

// AccountStatus is an alias for scheduler.AccountStatus — single source of truth.
type AccountStatus = scheduler.AccountStatus

// AttachmentBlobStore serves attachment bytes by content hash from packed or
// loose storage. Implemented by the daemon attachment store. Not-found errors satisfy
// errors.Is(err, fs.ErrNotExist).
type AttachmentBlobStore interface {
	OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error)
}

// Server represents the HTTP API server.
type Server struct {
	cfg            *config.Config
	store          MessageStore
	engine         query.Engine // Query engine for aggregates and TUI support
	sqlQueryRunner SQLQueryRunner
	shutdownToken  string
	shutdownFunc   func()
	scheduler      SyncScheduler
	logger         *slog.Logger
	requestTimeout time.Duration
	// queryTimeout caps POST /api/v1/query. Defaults to QueryEndpointTimeout;
	// tests override it to exercise the timeout path without a real slow query.
	queryTimeout time.Duration
	// inProgressThreshold/Interval control the in-flight request WARN. Default
	// to the package constants; tests shrink them to exercise the path.
	inProgressThreshold time.Duration
	inProgressInterval  time.Duration
	daemonVersion       string
	analyticsMode       string
	router              http.Handler
	server              *http.Server
	rateLimiter         *RateLimiter
	idleTracker         *IdleTracker
	operationGate       OperationGate
	// ftsIndexComplete memoizes that the FTS index is fully populated so
	// handleCLISearch stops probing on every request. NeedsFTSBackfill runs an
	// anti-join that scans every message when the index is complete (the
	// healthy steady state) — tens of seconds on a large archive — which
	// dominated CLI search latency (the fast /api/v1/search path never probes).
	// Set once the index is confirmed complete; not reset, so a hole created
	// mid-session by a rare inline UpsertFTS failure is only auto-repaired after
	// a restart or `rebuild-fts` (the same limitation the /api/v1/search path
	// already has, since it never backfills).
	ftsIndexComplete atomic.Bool
	// ftsEnsureRunning guards the single background probe/backfill worker
	// spawned by CLI searches; ftsIndexState is what that worker is doing
	// ("checking", "building", or "" when idle/complete) so search responses
	// can report it. See ensureCLISearchIndexAsync.
	ftsEnsureRunning atomic.Bool
	ftsIndexState    atomic.Value
	// ftsRebuildGen is a seqlock-style generation for index rebuilds:
	// handleCLIRebuildFTS bumps it to odd on entry and back to even on
	// return. The ensure worker's completeness probe runs outside the
	// operation gate, so its result can predate a concurrent rebuild's
	// index clear; the worker snapshots this generation before probing and
	// memoizes a "complete" observation only on an even, unchanged value.
	ftsRebuildGen atomic.Uint64
	// activity reports request-scoped work that health should surface even
	// though it runs outside (or with more detail than) the operation gate,
	// e.g. the first-search FTS completeness probe and backfill progress.
	// See beginActivity / setActivityLabel / currentActivity.
	activityMu    sync.Mutex
	activityCount int
	activityLabel string
	activitySince time.Time
	cfgMu         sync.RWMutex // protects cfg.Accounts
	// vectorMu guards the vector subsystem state: the daemon installs
	// hybridEngine/backend/vectorCfg from a background init goroutine
	// after the server is already handling requests.
	vectorMu     sync.RWMutex
	hybridEngine *hybrid.Engine
	vectorCfg    vector.Config
	backend      vector.Backend
	vectorStatus VectorStatus
	vectorErr    string
	// backupFreeze tracks the single active backup freeze window opened via
	// POST /api/v1/backup/freeze/begin. See backup_freeze.go.
	backupFreeze backupFreezeState
	// blobStore serves attachment bytes for handleCLIAttachment. Nil in tests
	// and embedded callers that construct a Server without options, which
	// fall back to the legacy loose-file open.
	blobStore AttachmentBlobStore
}

type SQLQueryRunner func(ctx context.Context, sql string) (*query.QueryResult, error)

const (
	DaemonLongRequestTimeout = 30 * time.Minute
	// QueryEndpointTimeout is the hard ceiling for POST /api/v1/query. The raw
	// SQL endpoint is the F2 runaway culprit: a single bad SELECT over the full
	// archive pegged every core for minutes. 120s is generous for legitimate
	// analytics while still bounding a pathological query.
	QueryEndpointTimeout = 120 * time.Second
	queryEndpointPath    = "/api/v1/query"
	DaemonShutdownPath   = "/api/daemon/shutdown"
	defaultBindAddr      = "127.0.0.1"
	// inProgressLogThreshold is how long a request may run before the logger
	// emits a WARN "http request in progress" line, and inProgressLogInterval
	// how often it repeats thereafter. Requests are otherwise logged only on
	// completion, so a runaway in-flight request was invisible in serve.log.
	inProgressLogThreshold = 10 * time.Second
	inProgressLogInterval  = 30 * time.Second
	// DaemonShutdownTokenHeader is an HTTP header name, not a credential.
	// #nosec G101
	DaemonShutdownTokenHeader = "X-Msgvault-Daemon-Token"
)

// ServerOptions configures the API server.
type ServerOptions struct {
	Config         *config.Config
	Store          MessageStore
	Engine         query.Engine // Optional: query engine for aggregates and TUI support
	SQLQueryRunner SQLQueryRunner
	ShutdownToken  string
	ShutdownFunc   func()
	HybridEngine   *hybrid.Engine
	VectorCfg      vector.Config
	Backend        vector.Backend
	// VectorStatus is the initial vector subsystem status. Zero value
	// derives it: ready when Backend is non-nil, disabled otherwise. The
	// serve daemon passes VectorStatusInitializing and installs the
	// components later via SetVectorFeatures.
	VectorStatus  VectorStatus
	Scheduler     SyncScheduler
	Logger        *slog.Logger
	IdleTracker   *IdleTracker
	OperationGate OperationGate
	// BlobStore serves attachment bytes for /api/v1/cli/attachment through
	// packed CAS storage with a loose-file fallback. Nil keeps the legacy
	// loose-file-only read path.
	BlobStore AttachmentBlobStore
	// RequestTimeout caps each request by adding a deadline to the request
	// context. Zero defaults to 60s. The underlying http.Server's WriteTimeout
	// is set to RequestTimeout + 5s so handlers that honor cancellation can
	// return structured error responses before the connection deadline.
	RequestTimeout time.Duration
	// DaemonVersion is returned by the unauthenticated kit-compatible
	// /api/ping endpoint used for local daemon discovery. Empty is allowed.
	DaemonVersion string
	// AnalyticsMode is the analytics engine the daemon selected at startup
	// (an AnalyticsMode constant), reported by /health so clients can tell
	// whether aggregate views run on the cache or live SQL. Empty omits the
	// field.
	AnalyticsMode string
}

// NewServer creates a new API server.
func NewServer(cfg *config.Config, store MessageStore, sched SyncScheduler, logger *slog.Logger) *Server {
	return NewServerWithOptions(ServerOptions{
		Config:    cfg,
		Store:     store,
		Scheduler: sched,
		Logger:    logger,
	})
}

// NewServerWithOptions creates a new API server with full options including query engine.
func NewServerWithOptions(opts ServerOptions) *Server {
	timeout := opts.RequestTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	s := &Server{
		cfg:                 opts.Config,
		store:               opts.Store,
		engine:              opts.Engine,
		sqlQueryRunner:      opts.SQLQueryRunner,
		shutdownToken:       opts.ShutdownToken,
		shutdownFunc:        opts.ShutdownFunc,
		hybridEngine:        opts.HybridEngine,
		vectorCfg:           opts.VectorCfg,
		backend:             opts.Backend,
		scheduler:           opts.Scheduler,
		logger:              opts.Logger,
		requestTimeout:      timeout,
		queryTimeout:        QueryEndpointTimeout,
		inProgressThreshold: inProgressLogThreshold,
		inProgressInterval:  inProgressLogInterval,
		daemonVersion:       opts.DaemonVersion,
		analyticsMode:       opts.AnalyticsMode,
		idleTracker:         opts.IdleTracker,
		operationGate:       opts.OperationGate,
		blobStore:           opts.BlobStore,
	}
	s.vectorStatus = opts.VectorStatus
	if s.vectorStatus == "" {
		if opts.Backend != nil {
			s.vectorStatus = VectorStatusReady
		} else {
			s.vectorStatus = VectorStatusDisabled
		}
	}
	s.router = s.setupRouter()
	return s
}

// setupRouter configures the Huma API router and standard HTTP middleware.
func (s *Server) setupRouter() http.Handler {
	mux := http.NewServeMux()
	api := s.setupHumaAPI(mux)
	apiV1 := s.setupAPIV1Group(api)
	s.registerHumaRoutes(api, apiV1)
	s.registerPprofHandlers(mux)

	// Catch-all so unknown paths and trailing-slash misses return the JSON
	// ErrorResponse envelope the contract declares, instead of Go's default
	// text/plain "404 page not found". More specific patterns (API routes,
	// /debug/pprof/, /openapi.*, /docs) still take precedence over "/".
	mux.HandleFunc("/", s.handleNotFound)

	// CORS middleware (config-driven; disabled when no origins configured)
	corsConfig := CORSConfig{
		AllowedOrigins:   s.cfg.Server.CORSOrigins,
		AllowedMethods:   defaultCORSAllowedMethods(),
		AllowedHeaders:   defaultCORSAllowedHeaders(),
		AllowCredentials: s.cfg.Server.CORSCredentials,
		MaxAge:           s.cfg.Server.CORSMaxAge,
	}
	if corsConfig.MaxAge == 0 && len(corsConfig.AllowedOrigins) > 0 {
		corsConfig.MaxAge = 86400
	}

	// Rate limiting (10 req/sec with burst of 20)
	s.rateLimiter = NewRateLimiter(10, 20)

	// The operation gate sits inside rate limiting and checks API auth
	// itself, so unauthenticated or rate-limited requests are rejected
	// before they can register as gate waiters or observe operation state.
	var h http.Handler = mux
	h = operationGateMiddleware(s.operationGate, s.apiRequestAuthorized)(h)
	h = RateLimitMiddleware(s.rateLimiter, s.loopbackRateLimitExempt)(h)
	h = CORSMiddleware(corsConfig)(h)
	h = s.timeoutMiddleware(h)
	if s.idleTracker != nil {
		h = s.idleTracker.Wrap(h)
	}
	h = s.recoverMiddleware(h)
	h = s.loggerMiddleware(h)
	h = requestIDMiddleware(h)
	return h
}

// registerPprofHandlers wires the standard net/http/pprof handlers under
// /debug/pprof/, each gated to trusted loopback callers. The guard requires
// both that the request is loopback (via r.RemoteAddr, not spoofable headers)
// AND that it passes apiRequestAuthorized — so when an API key is configured,
// unauthenticated traffic that reaches loopback through a same-host reverse
// proxy, TLS terminator, or SSH tunnel cannot read profiles. In keyless local
// mode apiRequestAuthorized returns true, preserving on-box goroutine/CPU/heap
// introspection for the TUI/CLI autostart case. There is no config knob.
func (s *Server) registerPprofHandlers(mux *http.ServeMux) {
	trustedLoopbackOnly := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !isLoopbackRequest(r) || !s.apiRequestAuthorized(r) {
				http.NotFound(w, r)
				return
			}
			h(w, r)
		}
	}
	// pprof.Index also serves the named profiles (heap, goroutine, allocs, …).
	mux.HandleFunc("/debug/pprof/", trustedLoopbackOnly(pprof.Index))
	mux.HandleFunc("/debug/pprof/cmdline", trustedLoopbackOnly(pprof.Cmdline))
	mux.HandleFunc("/debug/pprof/profile", trustedLoopbackOnly(pprof.Profile))
	mux.HandleFunc("/debug/pprof/symbol", trustedLoopbackOnly(pprof.Symbol))
	mux.HandleFunc("/debug/pprof/trace", trustedLoopbackOnly(pprof.Trace))
}

// Start begins listening for HTTP requests.
// Returns an error if the security posture is invalid.
func (s *Server) Start() error {
	bindAddr := s.cfg.Server.BindAddr
	if bindAddr == "" {
		bindAddr = defaultBindAddr
	}
	addr := net.JoinHostPort(bindAddr, strconv.Itoa(s.cfg.Server.APIPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	return s.StartOnListener(ln)
}

// StartOnListener serves HTTP requests on an already-bound listener. The serve
// daemon uses this to reserve its configured API port before expensive archive
// startup work begins.
func (s *Server) StartOnListener(ln net.Listener) error {
	if ln == nil {
		return errors.New("nil listener")
	}
	if err := s.cfg.Server.ValidateSecure(); err != nil {
		_ = ln.Close()
		return err
	}

	if s.cfg.Server.APIKey == "" {
		s.logger.Warn("API server running without authentication — set [server] api_key in config.toml")
	}

	// WriteTimeout must comfortably exceed the request-context timeout;
	// otherwise a request whose context deadline equals the server
	// WriteTimeout could lose the race and have its TCP connection torn down
	// before the structured error response reaches the client.
	writeBudget := max(s.requestTimeout, DaemonLongRequestTimeout)
	writeTimeout := writeBudget + 5*time.Second
	s.server = &http.Server{
		Addr:         ln.Addr().String(),
		Handler:      s.router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: writeTimeout,
		IdleTimeout:  120 * time.Second,
	}

	s.logger.Info("starting API server", "addr", ln.Addr().String())
	return s.server.Serve(ln)
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.rateLimiter != nil {
		s.rateLimiter.Close()
	}
	if s.server == nil {
		return nil
	}
	s.logger.Info("shutting down API server")
	return s.server.Shutdown(ctx)
}

// Router returns the HTTP router for testing.
func (s *Server) Router() http.Handler {
	return s.router
}

func (s *Server) timeoutMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timeout, bounded := s.requestTimeoutForPath(r.URL.Path)
		if !bounded {
			// Long-running request (multi-hour sync, import, embeddings
			// build): the server's absolute WriteTimeout would sever the
			// response at the 30-minute mark regardless of activity, so
			// clear the connection's write deadline for this request. A
			// disconnected client still ends the work via r.Context()
			// cancellation. Best-effort: test recorders lack deadlines.
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requestTimeoutForPath returns the context deadline to impose on a request
// and whether one applies at all. POST /api/v1/query gets its own generous
// ceiling; the streaming/long-running CLI operations stay unbounded (they
// report progress and are gated by the operation gate); everything else gets
// the standard per-request timeout.
func (s *Server) requestTimeoutForPath(path string) (time.Duration, bool) {
	if path == queryEndpointPath {
		return s.queryTimeout, true
	}
	if isLongDaemonRequest(path) {
		return 0, false
	}
	return s.requestTimeout, true
}

func isLongDaemonRequest(path string) bool {
	switch path {
	case "/api/v1/cli/build-cache",
		"/api/v1/cli/deduplicate/plan",
		"/api/v1/cli/rebuild-fts",
		"/api/v1/cli/repair-encoding",
		"/api/v1/cli/run",
		"/api/v1/cli/search",
		"/api/v1/cli/sync",
		"/api/v1/cli/sync-full",
		"/api/v1/cli/verify":
		return true
	default:
		return false
	}
}

// loggerMiddleware logs HTTP requests on completion and, for requests that
// overrun inProgressThreshold, emits a repeating WARN so a runaway in-flight
// request is visible in serve.log instead of only appearing (if ever) once it
// finishes.
func (s *Server) loggerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := newTrackingResponseWriter(w)

		stopWatch := s.watchInProgressRequest(r, start)

		defer func() {
			stopWatch()
			s.logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration", time.Since(start),
				"request_id", requestIDFromContext(r.Context()),
			)
		}()

		next.ServeHTTP(ww, r)
	})
}

// watchInProgressRequest starts a goroutine that logs a WARN if the request is
// still running after inProgressThreshold, repeating every inProgressInterval.
// The returned stop function ends the goroutine (called on request completion)
// and must be invoked exactly once, so the watcher never leaks.
func (s *Server) watchInProgressRequest(r *http.Request, start time.Time) func() {
	threshold := s.inProgressThreshold
	if threshold <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	method, path := r.Method, r.URL.Path
	requestID := requestIDFromContext(r.Context())
	go func() {
		timer := time.NewTimer(threshold)
		defer timer.Stop()
		for {
			select {
			case <-done:
				return
			case <-timer.C:
				s.logger.Warn("http request in progress",
					"method", method,
					"path", path,
					"request_id", requestID,
					"elapsed", time.Since(start),
				)
				if s.inProgressInterval <= 0 {
					return
				}
				timer.Reset(s.inProgressInterval)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := newTrackingResponseWriter(w)
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("panic serving request",
					"panic", recovered,
					"path", r.URL.Path,
					"request_id", requestIDFromContext(r.Context()),
				)
				if !ww.WroteHeader() {
					writeError(ww, http.StatusInternalServerError, "internal_error", "Internal server error")
				}
			}
		}()
		next.ServeHTTP(ww, r)
	})
}

type requestIDKey struct{}

var nextRequestID atomic.Uint64

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = fmt.Sprintf("msgvault-%d", nextRequestID.Add(1))
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		// Also stash it where the SQL logger reads it, so a "sql slow"
		// line can be correlated with this request's "http request" line.
		ctx = store.WithRequestID(ctx, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

type trackingResponseWriter struct {
	http.ResponseWriter

	status int
	bytes  int
}

func newTrackingResponseWriter(w http.ResponseWriter) *trackingResponseWriter {
	return &trackingResponseWriter{ResponseWriter: w}
}

func (w *trackingResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

func (w *trackingResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *trackingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *trackingResponseWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *trackingResponseWriter) BytesWritten() int {
	return w.bytes
}

func (w *trackingResponseWriter) WroteHeader() bool {
	return w.status != 0
}

// loopbackRateLimitExempt reports whether a request should bypass the rate
// limiter. Loopback origin alone is not sufficient: a same-host reverse proxy,
// SSH tunnel, or TLS terminator forwarding to loopback makes remote traffic
// arrive as 127.0.0.1, which would otherwise brute-force the API key
// unthrottled. A loopback request is exempt only when it is trusted — either
// no API key is configured (pure local mode, the TUI/CLI autostart case) or it
// carries a valid API key (an authenticated local client). apiRequestAuthorized
// returns true in exactly those two cases, so it is reused here to avoid the
// auth logic drifting.
func (s *Server) loopbackRateLimitExempt(r *http.Request) bool {
	return isLoopbackRequest(r) && s.apiRequestAuthorized(r)
}

func (s *Server) apiRequestAuthorized(r *http.Request) bool {
	// Skip auth if no API key configured.
	if s.cfg.Server.APIKey == "" {
		return true
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		authHeader = r.Header.Get("X-Api-Key")
	}
	if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
		authHeader = authHeader[7:]
	}

	return subtle.ConstantTimeCompare([]byte(authHeader), []byte(s.cfg.Server.APIKey)) == 1
}

func (s *Server) logUnauthorizedAPIRequest(r *http.Request) {
	s.logger.Warn("unauthorized API request",
		"path", r.URL.Path,
		"remote_addr", r.RemoteAddr,
	)
}

func (s *Server) handleDaemonShutdown(w http.ResponseWriter, r *http.Request) {
	if s.shutdownToken == "" || s.shutdownFunc == nil {
		writeError(w, http.StatusNotFound, "shutdown_unavailable", "Daemon shutdown is not available")
		return
	}

	got := r.Header.Get(DaemonShutdownTokenHeader)
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.shutdownToken)) != 1 {
		s.logger.Warn("unauthorized daemon shutdown request", "remote_addr", r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid or missing daemon shutdown token")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"shutting_down"}`))
	go s.shutdownFunc()
}

// handleHealth returns a simple health check response.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.refreshVectorStatusIfStale(r.Context())
	writeJSON(w, http.StatusOK, HealthResponse{
		Status:          "ok",
		Vector:          s.vectorHealth(),
		Operation:       s.operationBusyHealth(),
		AnalyticsEngine: s.analyticsMode,
	})
}

// handleAuthenticatedHealth returns health details that are safe behind the
// API-key boundary.
func (s *Server) handleAuthenticatedHealth(w http.ResponseWriter, r *http.Request) {
	s.refreshVectorStatusIfStale(r.Context())
	writeJSON(w, http.StatusOK, HealthResponse{
		Status:          "ok",
		Vector:          s.vectorHealth(),
		Operation:       s.operationHealth(),
		AnalyticsEngine: s.analyticsMode,
	})
}

// operationBusyHealth reports only whether the operation gate is currently
// held, avoiding protected route labels and start times on public /health.
func (s *Server) operationBusyHealth() *OperationHealth {
	_, _, held := s.operationGateHolder()
	if !held {
		return nil
	}
	return &OperationHealth{Busy: true}
}

// operationHealth reports what the daemon is currently working on. A
// request-scoped activity (which carries live progress detail) wins over the
// operation gate holder's static label; the gate holder covers everything
// else. Unlabeled holders still get a generic label so clients can tell
// "busy" from "idle".
func (s *Server) operationHealth() *OperationHealth {
	if label, since, active := s.currentActivity(); active {
		return &OperationHealth{Busy: true, Label: label, StartedAt: &since}
	}
	label, since, held := s.operationGateHolder()
	if !held {
		return nil
	}
	if label == "" {
		label = "an archive operation"
	}
	return &OperationHealth{Busy: true, Label: label, StartedAt: &since}
}

// beginActivity records that a request is doing labeled work the operation
// gate cannot describe — either ungated work (the FTS completeness probe) or
// gated work whose label should carry live progress (the FTS backfill). The
// returned func ends the activity. Overlapping activities share one label:
// the first begin sets it, the last end clears it.
func (s *Server) beginActivity(label string) func() {
	s.activityMu.Lock()
	s.activityCount++
	if s.activityCount == 1 {
		s.activityLabel = label
		s.activitySince = time.Now()
	}
	s.activityMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.activityMu.Lock()
			s.activityCount--
			if s.activityCount == 0 {
				s.activityLabel = ""
				s.activitySince = time.Time{}
			}
			s.activityMu.Unlock()
		})
	}
}

// setActivityLabel updates the current activity's label in place so progress
// callbacks can keep health output live (e.g. "building the search index
// (12000/48000 messages)"). No-op when no activity is active.
func (s *Server) setActivityLabel(label string) {
	s.activityMu.Lock()
	if s.activityCount > 0 {
		s.activityLabel = label
	}
	s.activityMu.Unlock()
}

func (s *Server) currentActivity() (string, time.Time, bool) {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.activityCount == 0 {
		return "", time.Time{}, false
	}
	return s.activityLabel, s.activitySince, true
}

func (s *Server) operationGateHolder() (string, time.Time, bool) {
	lg, ok := s.operationGate.(LabeledOperationGate)
	if !ok {
		return "", time.Time{}, false
	}
	return lg.Holder()
}

// handleNotFound is the mux catch-all for unmatched paths. It returns the
// standard JSON ErrorResponse envelope so clients that parse the documented
// error shape do not choke on Go's default text/plain 404.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "No route matches "+r.Method+" "+r.URL.Path)
}
