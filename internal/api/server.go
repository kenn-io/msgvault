// Package api provides the HTTP API server for msgvault.
package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	daemonVersion  string
	router         http.Handler
	server         *http.Server
	rateLimiter    *RateLimiter
	idleTracker    *IdleTracker
	operationGate  OperationGate
	cfgMu          sync.RWMutex // protects cfg.Accounts
	// vectorMu guards the vector subsystem state: the daemon installs
	// hybridEngine/backend/vectorCfg from a background init goroutine
	// after the server is already handling requests.
	vectorMu     sync.RWMutex
	hybridEngine *hybrid.Engine
	vectorCfg    vector.Config
	backend      vector.Backend
	vectorStatus VectorStatus
	vectorErr    string
}

type SQLQueryRunner func(ctx context.Context, sql string) (*query.QueryResult, error)

const (
	DaemonLongRequestTimeout = 30 * time.Minute
	DaemonShutdownPath       = "/api/daemon/shutdown"
	defaultBindAddr          = "127.0.0.1"
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
	// RequestTimeout caps each request by adding a deadline to the request
	// context. Zero defaults to 60s. The underlying http.Server's WriteTimeout
	// is set to RequestTimeout + 5s so handlers that honor cancellation can
	// return structured error responses before the connection deadline.
	RequestTimeout time.Duration
	// DaemonVersion is returned by the unauthenticated kit-compatible
	// /api/ping endpoint used for local daemon discovery. Empty is allowed.
	DaemonVersion string
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
		cfg:            opts.Config,
		store:          opts.Store,
		engine:         opts.Engine,
		sqlQueryRunner: opts.SQLQueryRunner,
		shutdownToken:  opts.ShutdownToken,
		shutdownFunc:   opts.ShutdownFunc,
		hybridEngine:   opts.HybridEngine,
		vectorCfg:      opts.VectorCfg,
		backend:        opts.Backend,
		scheduler:      opts.Scheduler,
		logger:         opts.Logger,
		requestTimeout: timeout,
		daemonVersion:  opts.DaemonVersion,
		idleTracker:    opts.IdleTracker,
		operationGate:  opts.OperationGate,
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

	var h http.Handler = mux
	h = RateLimitMiddleware(s.rateLimiter)(h)
	h = CORSMiddleware(corsConfig)(h)
	h = operationGateMiddleware(s.operationGate)(h)
	h = s.timeoutMiddleware(h)
	if s.idleTracker != nil {
		h = s.idleTracker.Wrap(h)
	}
	h = s.recoverMiddleware(h)
	h = s.loggerMiddleware(h)
	h = requestIDMiddleware(h)
	return h
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
		if isLongDaemonRequest(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
		"/api/v1/cli/verify",
		"/api/v1/query":
		return true
	default:
		return false
	}
}

// loggerMiddleware logs HTTP requests.
func (s *Server) loggerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := newTrackingResponseWriter(w)

		defer func() {
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
	writeJSON(w, http.StatusOK, HealthResponse{Status: "ok", Vector: s.vectorHealth()})
}
