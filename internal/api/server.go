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
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonauth"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/tasklinks"
	"go.kenn.io/msgvault/internal/vector"
	vectordocument "go.kenn.io/msgvault/internal/vector/document"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/visual"
	webapp "go.kenn.io/msgvault/internal/web"
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

// MessageIdentityStore is the optional source-identity extension used by
// analytical filters and response hydration. Production stores implement it;
// keeping it separate preserves lightweight MessageStore test doubles.
type MessageIdentityStore interface {
	ResolveAccountIdentityContext(ctx context.Context, sourceID int64, identifier string) (store.ResolvedAccountIdentity, error)
	MatchMessageIdentitiesContext(ctx context.Context, messageIDs []int64) (map[int64]store.MessageIdentityMatch, error)
}

type TaskLinkOperations interface {
	Create(ctx context.Context, key string, create taskclient.TaskCreate, identity tasklinks.MessageIdentity, addedAt time.Time) (taskclient.Task, error)
	Link(ctx context.Context, taskID string, identity tasklinks.MessageIdentity, addedAt time.Time) (taskclient.Task, error)
	Unlink(ctx context.Context, taskID string, identity tasklinks.MessageIdentity) (taskclient.Task, error)
	Lookup(ctx context.Context, identity tasklinks.MessageIdentity) tasklinks.LookupResult
	Search(ctx context.Context, query string) ([]tasklinks.TaskSummary, error)
}

type TaskIdentityResolver func(context.Context, *APIMessage) (tasklinks.MessageIdentity, error)

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

// ChangedMessageLister is an optional extension of MessageStore for stores that
// can serve the content-change feed. Optional rather than part of MessageStore
// because the feed needs the content_changed_at triggers and a commit-bound
// reading, which only a store sitting on the migrated schema has: anything else
// implementing MessageStore — the package's own test doubles today — leaves the
// method off and the route answers 503 feature_unavailable rather than inventing
// a watermark.
type ChangedMessageLister interface {
	ListChangedMessages(
		ctx context.Context, since store.ChangedMessagesCursor, limit int,
	) (store.ChangedMessagePage, error)
}

// The store implementation must satisfy the optional interface. This guards the
// store side of the contract only: the daemon passes cmd.storeAPIAdapter, not
// *store.Store, so the assertion that actually protects the production route is
// the one beside that adapter in cmd/msgvault/cmd/serve.go, which is a non-test
// file and so fails the build rather than a test run. An end-to-end test drives
// the route through the adapter in cmd/msgvault/cmd/changes_api_e2e_test.go.
var _ ChangedMessageLister = (*store.Store)(nil)

// ArchiveIdentifier is an optional extension of MessageStore for stores that
// can report the durable identity of the archive behind them.
//
// Separate from ChangedMessageLister, and asserted separately, because the two
// are different capabilities: listing changed rows needs the migrated schema,
// while identifying the archive needs archive_metadata. The change feed happens
// to need both — its cursor carries an archive-local message id, so a cursor is
// only meaningful in the archive that issued it and has to name it — but
// widening ChangedMessageLister to carry the lookup would make every future
// implementer of the feed also implement identity, and would put the answer to
// "which archive is this" behind the feed.
//
// The same 503 feature_unavailable as ChangedMessageLister: a store that cannot
// say which archive it is cannot issue a cursor anyone can safely resume from,
// and an unbound cursor is the failure the binding exists to prevent.
type ArchiveIdentifier interface {
	ArchiveUIDContext(ctx context.Context) (string, error)
}

// The store implementation must satisfy the optional interface. As with
// ChangedMessageLister, the assertion that protects the production route is the
// one beside cmd.storeAPIAdapter in cmd/msgvault/cmd/serve.go.
var _ ArchiveIdentifier = (*store.Store)(nil)

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
	// JobStatus reports every generic (non-account) scheduled job — the
	// synctech-sms, gcal, granola, circleback, and beeper sources — so
	// sourceStatus can surface their scheduled/running/error state via
	// SchedulerJobNameForSource. See scheduler_jobs.go.
	JobStatus() []JobStatus
	// IsJobScheduled and StartJob manually run a generic (non-account)
	// scheduler job by name, the trigger counterpart to JobStatus used by
	// handleTriggerSync for generic sources. StartJob runs asynchronously
	// (like TriggerSync) so the HTTP handler can return before the job
	// acquires the daemon's operation gate, avoiding a self-deadlock when
	// the request itself is holding that gate.
	IsJobScheduled(name string) bool
	StartJob(name string) error
	TriggerJob(name string) error
}

// AccountStatus is an alias for scheduler.AccountStatus — single source of truth.
type AccountStatus = scheduler.AccountStatus

// JobStatus is an alias for scheduler.JobStatus — single source of truth.
type JobStatus = scheduler.JobStatus

// AttachmentBlobStore serves attachment bytes by content hash from packed or
// loose storage. Implemented by the daemon attachment store. Not-found errors satisfy
// errors.Is(err, fs.ErrNotExist).
type AttachmentBlobStore interface {
	OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error)
}

type fastmailIdentityInventory = provideridentity.Inventory

type analyticsEngineState struct {
	engine                        query.Engine
	mode                          string
	analyticsInitializationActive bool
}

type analyticsEngineContextKey struct{}

// Server represents the HTTP API server.
type Server struct {
	cfg            *config.Config
	store          MessageStore
	analyticsState atomic.Pointer[analyticsEngineState]
	savedViewStore SavedViewStore
	sqlQueryRunner SQLQueryRunner
	shutdownToken  string
	shutdownFunc   func()
	scheduler      SyncScheduler
	cardDAV        *CardDAVController
	logger         *slog.Logger
	requestTimeout time.Duration
	// readTimeout is the ordinary connection read ceiling used by http.Server.
	// Tests shrink it to exercise protective slow-body handling without waiting
	// for the production timeout.
	readTimeout time.Duration
	// queryTimeout caps POST /api/v1/query. Defaults to QueryEndpointTimeout;
	// tests override it to exercise the timeout path without a real slow query.
	queryTimeout time.Duration
	// inProgressThreshold/Interval control the in-flight request WARN. Default
	// to the package constants; tests shrink them to exercise the path.
	inProgressThreshold time.Duration
	inProgressInterval  time.Duration
	daemonVersion       string
	// lexicalCandidateCap overrides query.MaxExploreCandidateMessageIDs in
	// resolveExploreSearch. Tests shrink it to exercise cap behavior without
	// building 10k-row fixtures; zero means the production cap.
	lexicalCandidateCap int
	router              http.Handler
	// serverMu protects the HTTP server pointer and listener-start state. The
	// daemon starts Serve in a goroutine while shutdown can begin from a
	// cancelled root context, so Shutdown must not race the assignment below.
	serverMu                  sync.RWMutex
	server                    *http.Server
	started                   chan struct{}
	startErr                  error
	startOnce                 sync.Once
	rateLimiter               *RateLimiter
	changesRateLimiter        *RateLimiter
	documentSearchRateLimiter *RateLimiter
	visualCoverageRateLimiter *RateLimiter
	idleTracker               *IdleTracker
	operationGate             OperationGate
	operationHistoryReader    operations.HistoryReader
	importContext             context.Context
	cancelImports             context.CancelFunc
	importMu                  sync.Mutex
	importsClosed             bool
	importWG                  sync.WaitGroup
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
	// changesStallLoggedAt throttles the WARN handleMessageChanges emits when
	// the content-change feed is held back by a long-lived write transaction.
	// Unix nanoseconds of the last such line, so a consumer polling once a
	// second cannot turn one stuck connection into a log flood.
	changesStallLoggedAt atomic.Int64
	// ftsRebuildGen is a seqlock-style generation for index rebuilds:
	// handleCLIRebuildFTS bumps it to odd on entry and back to even on
	// return. The ensure worker's completeness probe runs outside the
	// operation gate, so its result can predate a concurrent rebuild's
	// index clear; the worker snapshots this generation before probing and
	// memoizes a "complete" observation only on an even, unchanged value.
	ftsRebuildGen atomic.Uint64
	// settingsPendingRestart remains set after the first successful browser
	// config edit for the lifetime of this daemon process.
	settingsPendingRestart atomic.Bool
	// settingsConfigEditor is the persisted config transaction boundary. Tests
	// replace it to deterministically exercise post-publication error handling.
	settingsConfigEditor func(string, string, []config.Edit) (config.ConfigFile, error)
	// settingsCredentialDeleter is the independent credential-store transaction
	// boundary used when a settings edit changes a provider origin.
	settingsCredentialDeleter func(
		string, providercredentials.Snapshot, []string,
	) (providercredentials.Snapshot, error)
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
	vectorMu           sync.RWMutex
	hybridEngine       *hybrid.Engine
	vectorCfg          vector.Config
	backend            vector.Backend
	personSearchEngine PersonSearchEngine
	visualSearch       *visual.SearchService
	visualBuild        func(context.Context) error
	visualRun          func(context.Context) error
	visualRetry        func(context.Context, int64, string) error
	visualStatus       func(context.Context, bool) (visual.Status, error)
	// visualCoverageScan serializes the archive-wide coverage scan behind
	// GET /multimodal/status?coverage=1.
	visualCoverageScan sync.Mutex
	visualRetire       func(context.Context) error
	vectorStatus       VectorStatus
	vectorErr          string
	// vectorStaleLatch pins a stale status that refreshVectorStatusIfStale
	// must not clear: set when the durable embedding scope drifts from the
	// scope the installed components were initialized with. The active
	// generation still matches the STARTUP fingerprint in that state, so
	// the ordinary refresh would flip straight back to ready. Only a
	// successful reinit (SetVectorFeatures) clears the latch.
	vectorStaleLatch bool
	documentSearchMu sync.RWMutex
	documentSearch   *vectordocument.SearchService
	// vectorScopeCheck re-resolves the durable embedding scope on the
	// vector-search preflight path (throttled by vectorScopeNextCheck) so
	// drift is detected even when no embed job ever runs. Wired by the
	// daemon; see SetVectorScopeCheck.
	vectorScopeCheck     func(ctx context.Context) (string, error)
	vectorScopeNextCheck time.Time
	// vectorFreshNextCheck throttles maybeRefreshVectorFreshness, the
	// ready→stale counterpart of refreshVectorStatusIfStale: a
	// daemon-proxied one-off scoped build can activate a generation whose
	// fingerprint no longer matches the installed configuration without
	// any config change, and nothing else re-validates a ready status.
	vectorFreshNextCheck time.Time
	// backupFreeze tracks the single active backup freeze window opened via
	// POST /api/v1/backup/freeze/begin. See backup_freeze.go.
	backupFreeze backupFreezeState
	// blobStore serves attachment bytes for handleCLIAttachment. Nil in tests
	// and embedded callers that construct a Server without options, which
	// fall back to the legacy loose-file open.
	blobStore AttachmentBlobStore
	// remoteImages is the SSRF-hardened fetcher behind
	// POST /api/v1/content/remote-image. Tests replace it to inject a fake
	// resolver and dialer.
	remoteImages *remoteImageFetcher
	// inlineCache parses each message's raw MIME once and serves every cid: from
	// that result, collapsing the per-cid fan-out (see inline_cache.go).
	inlineCache *inlineParseCache
	spaHandler  http.Handler
	sessions    *sessionStore
	// trustedProxies contains only explicitly configured direct proxy peers.
	// Forwarded scheme/host data is ignored for every other RemoteAddr.
	trustedProxies   []netip.Prefix
	exploreState     *exploreServerState
	exploreCursorKey [32]byte
	// clock overrides the wall clock for handlers that pin a date into a
	// pagination cursor (see handleRelationships); nil means time.Now. Tests
	// inject a fixed clock to exercise pagination across UTC midnight.
	clock func() time.Time
	// taskIntegrationProbe performs server-side discovery and capability
	// validation. It is never exposed to the browser with its credentials.
	taskIntegrationProbe     TaskIntegrationProbe
	taskLinkOperations       TaskLinkOperations
	taskIdentityResolver     TaskIdentityResolver
	fastmailInventoryFactory provideridentity.Factory
	// listenerBound is set true once StartOnListener binds a real listener
	// (the sole production serve path). It stays false for direct-handler unit
	// tests that drive s.Router() without starting a listener, leaving the
	// keyless-loopback Host guard inert for them.
	listenerBound bool
	// listenPort is the actual TCP port StartOnListener bound. The keyless-
	// loopback Host guard requires the request authority's port to match it.
	listenPort int
}

// clockNow returns the current wall time, honoring the test-injected clock.
func (s *Server) clockNow() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

type SQLQueryRunner func(ctx context.Context, sql string) (*query.QueryResult, error)

const (
	DaemonLongRequestTimeout = 30 * time.Minute
	daemonReadTimeout        = 15 * time.Second
	// QueryEndpointTimeout is the hard ceiling for POST /api/v1/query. The raw
	// SQL endpoint is the F2 runaway culprit: a single bad SELECT over the full
	// archive pegged every core for minutes. 120s is generous for legitimate
	// analytics while still bounding a pathological query.
	QueryEndpointTimeout = 120 * time.Second
	queryEndpointPath    = "/api/v1/query"
	DaemonIdentityPath   = "/api/daemon/identity"
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
	DaemonShutdownTokenHeader     = apiprotocol.DaemonRuntimeTokenHeader
	DaemonIdentityChallengeHeader = "X-Msgvault-Daemon-Challenge"
	DaemonIdentityProofHeader     = "X-Msgvault-Daemon-Proof"
)

// ServerOptions configures the API server.
type ServerOptions struct {
	Config *config.Config
	Store  MessageStore
	// SavedViewStore owns durable analytical view definitions. It is separate
	// from the minimal MessageStore so API consumers do not need to implement
	// unrelated persistence methods.
	SavedViewStore SavedViewStore
	Engine         query.Engine // Optional: query engine for aggregates and TUI support
	SQLQueryRunner SQLQueryRunner
	ShutdownToken  string
	ShutdownFunc   func()
	HybridEngine   *hybrid.Engine
	VectorCfg      vector.Config
	Backend        vector.Backend
	// PersonSearchEngine is the optional semantic people service.
	PersonSearchEngine PersonSearchEngine
	// VectorStatus is the initial vector subsystem status. Zero value
	// derives it: ready when Backend is non-nil, disabled otherwise. The
	// serve daemon passes VectorStatusInitializing and installs the
	// components later via SetVectorFeatures.
	VectorStatus VectorStatus
	Scheduler    SyncScheduler
	// CardDAV owns the single configured CardDAV service used by HTTP, CLI,
	// and scheduled synchronization.
	CardDAV       *CardDAVController
	Logger        *slog.Logger
	IdleTracker   *IdleTracker
	OperationGate OperationGate
	// OperationHistoryReader owns the normalized, privacy-bounded operation
	// ledgers. It stays separate from MessageStore so unsupported stores can
	// expose an explicit unavailable contract instead of implementing unrelated
	// history methods.
	OperationHistoryReader operations.HistoryReader
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
	// AnalyticsMode is the analytics engine the daemon selected initially (an
	// AnalyticsMode constant), reported by /health so clients can tell whether
	// aggregate views run on the cache or live SQL. The daemon may replace the
	// engine and mode at runtime. Empty omits the field.
	AnalyticsMode string
	// AnalyticsInitializationActive reports that cache selection, build, or
	// open work is already scheduled. Set it before the listener is exposed so
	// the first request cannot mistake active initialization for a terminal
	// SQL fallback.
	AnalyticsInitializationActive bool
	// SPAHandler overrides the embedded browser application handler. Nil uses
	// internal/web.Handler and is the production default. Tests may inject a
	// handler built over an in-memory filesystem.
	SPAHandler http.Handler
	// TaskIntegrationProbe overrides provider-neutral task discovery for tests.
	// Nil uses taskclient.Evaluate.
	TaskIntegrationProbe TaskIntegrationProbe
	TaskLinkOperations   TaskLinkOperations
	TaskIdentityResolver TaskIdentityResolver
	// FastmailInventoryFactory is the provider-read seam used by identity
	// discovery. Nil constructs the production JMAP client.
	FastmailInventoryFactory provideridentity.Factory
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
	taskProbe := opts.TaskIntegrationProbe
	if taskProbe == nil {
		taskProbe = taskclient.Evaluate
	}
	fastmailInventoryFactory := opts.FastmailInventoryFactory
	if fastmailInventoryFactory == nil {
		fastmailInventoryFactory = provideridentity.NewFastmailInventory
	}
	importContext, cancelImports := context.WithCancel(context.Background())
	s := &Server{
		cfg:                      opts.Config,
		store:                    opts.Store,
		savedViewStore:           opts.SavedViewStore,
		sqlQueryRunner:           opts.SQLQueryRunner,
		shutdownToken:            opts.ShutdownToken,
		shutdownFunc:             opts.ShutdownFunc,
		hybridEngine:             opts.HybridEngine,
		vectorCfg:                opts.VectorCfg,
		backend:                  opts.Backend,
		personSearchEngine:       opts.PersonSearchEngine,
		scheduler:                opts.Scheduler,
		cardDAV:                  opts.CardDAV,
		logger:                   opts.Logger,
		requestTimeout:           timeout,
		readTimeout:              daemonReadTimeout,
		queryTimeout:             QueryEndpointTimeout,
		inProgressThreshold:      inProgressLogThreshold,
		inProgressInterval:       inProgressLogInterval,
		daemonVersion:            opts.DaemonVersion,
		idleTracker:              opts.IdleTracker,
		operationGate:            opts.OperationGate,
		operationHistoryReader:   opts.OperationHistoryReader,
		importContext:            importContext,
		cancelImports:            cancelImports,
		blobStore:                opts.BlobStore,
		remoteImages:             newRemoteImageFetcher(),
		inlineCache:              newInlineParseCache(inlineCacheMaxEntries, inlineCacheMaxBytes),
		spaHandler:               opts.SPAHandler,
		sessions:                 newSessionStore(defaultSessionTTL),
		exploreState:             newExploreServerState(time.Now),
		exploreCursorKey:         newExploreCursorKey(),
		trustedProxies:           trustedProxyPrefixes(opts.Config.Server.TrustedProxies),
		settingsConfigEditor:     config.EditConfigFilePrivate,
		taskIntegrationProbe:     taskProbe,
		taskLinkOperations:       opts.TaskLinkOperations,
		taskIdentityResolver:     opts.TaskIdentityResolver,
		fastmailInventoryFactory: fastmailInventoryFactory,
		started:                  make(chan struct{}),
	}
	s.analyticsState.Store(&analyticsEngineState{
		engine: opts.Engine, mode: opts.AnalyticsMode,
		analyticsInitializationActive: opts.AnalyticsInitializationActive,
	})
	if s.taskIdentityResolver == nil {
		s.taskIdentityResolver = s.resolveTaskMessageIdentity
	}
	if s.taskLinkOperations == nil {
		s.taskLinkOperations = newTaskLinkBackend(opts.Config)
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
	// Most trusted local traffic bypasses the general limiter, but the change
	// feed takes SQLite's writer lock and therefore has its own non-bypassable
	// budget.
	s.rateLimiter = NewRateLimiter(10, 20)
	s.changesRateLimiter = NewRateLimiter(
		changeFeedRequestsPerSecond, changeFeedRequestBurst)
	s.documentSearchRateLimiter = NewRateLimiter(
		documentSearchRequestsPerSecond, documentSearchRequestBurst)
	s.visualCoverageRateLimiter = NewRateLimiter(
		visualCoverageScansPerSecond, visualCoverageScanBurst)

	mux := http.NewServeMux()
	api := s.setupHumaAPI(mux)
	apiV1 := s.setupAPIV1Group(api)
	s.registerHumaRoutes(api, apiV1)
	s.registerPprofHandlers(mux)

	// Registered API, debug, OpenAPI, and docs routes retain priority over the
	// root handler. The root serves exact embedded assets and safe navigation;
	// everything else delegates to the JSON ErrorResponse fallback.
	spaHandler := s.spaHandler
	if spaHandler == nil {
		spaHandler = webapp.Handler(http.HandlerFunc(s.handleNotFound))
	}
	mux.Handle("/", spaHandler)

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
	if corsConfig.AllowCredentials && slices.Contains(corsConfig.AllowedOrigins, "*") {
		s.logger.Warn("cors_origins contains \"*\": wildcard matches never receive " +
			"Access-Control-Allow-Credentials; list exact origins in cors_origins to allow credentialed CORS")
	}

	// Request security classification and CSRF checks sit inside rate limiting
	// but outside the operation gate, so rejected browser mutations never wait
	// on or observe archive work. The gate still checks API auth itself so
	// unauthenticated requests do not register as waiters.
	var h http.Handler = mux
	h = s.analyticsEngineMiddleware(h)
	h = operationGateMiddleware(s.operationGate, s.apiRequestAuthorized)(h)
	h = s.csrfMiddleware(h)
	h = s.requestSecurityMiddleware(h)
	h = RateLimitMiddleware(s.rateLimiter, s.loopbackRateLimitExempt)(h)
	h = CORSMiddleware(corsConfig)(h)
	h = s.timeoutMiddleware(h)
	if s.idleTracker != nil {
		h = s.idleTracker.Wrap(h)
	}
	h = s.recoverMiddleware(h)
	h = s.loggerMiddleware(h)
	h = requestIDMiddleware(h)
	h = apiCacheControlMiddleware(h)
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
		listenErr := fmt.Errorf("listen %s: %w", addr, err)
		s.signalStarted(listenErr)
		return listenErr
	}
	return s.StartOnListener(ln)
}

// WaitStarted waits until StartOnListener has installed the HTTP server and
// listener metadata, or until startup reports an error. A cancelled context
// only cancels the wait; callers that close the reserved listener must wait
// again without cancellation before releasing resources used by startup.
func (s *Server) WaitStarted(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.serverMu.Lock()
	started := s.started
	if started == nil {
		started = make(chan struct{})
		s.started = started
	}
	s.serverMu.Unlock()

	select {
	case <-started:
		s.serverMu.RLock()
		err := s.startErr
		s.serverMu.RUnlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) signalStarted(err error) {
	s.serverMu.Lock()
	started := s.started
	if started == nil {
		started = make(chan struct{})
		s.started = started
	}
	s.serverMu.Unlock()

	s.startOnce.Do(func() {
		s.serverMu.Lock()
		s.startErr = err
		s.serverMu.Unlock()
		close(started)
	})
}

// StartOnListener serves HTTP requests on an already-bound listener. The serve
// daemon uses this to reserve its configured API port before expensive archive
// startup work begins.
func (s *Server) StartOnListener(ln net.Listener) error {
	if ln == nil {
		err := errors.New("nil listener")
		s.signalStarted(err)
		return err
	}
	if err := s.cfg.Server.ValidateSecure(); err != nil {
		_ = ln.Close()
		s.signalStarted(err)
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
	server := &http.Server{
		Addr:         ln.Addr().String(),
		Handler:      s.router,
		ReadTimeout:  s.readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  120 * time.Second,
	}
	s.serverMu.Lock()
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
		s.listenPort = tcpAddr.Port
	}
	s.listenerBound = true
	s.server = server
	s.serverMu.Unlock()
	s.signalStarted(nil)

	s.logger.Info("starting API server", "addr", ln.Addr().String())
	return server.Serve(ln)
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.importMu.Lock()
	s.importsClosed = true
	if s.cancelImports != nil {
		s.cancelImports()
	}
	s.importMu.Unlock()
	importsDone := make(chan struct{})
	go func() {
		s.importWG.Wait()
		close(importsDone)
	}()
	var importJobsErr error
	select {
	case <-importsDone:
	case <-ctx.Done():
		importJobsErr = ctx.Err()
	}
	if s.rateLimiter != nil {
		s.rateLimiter.Close()
	}
	if s.changesRateLimiter != nil {
		s.changesRateLimiter.Close()
	}
	if s.documentSearchRateLimiter != nil {
		s.documentSearchRateLimiter.Close()
	}
	if s.visualCoverageRateLimiter != nil {
		s.visualCoverageRateLimiter.Close()
	}
	if s.sessions != nil {
		s.sessions.Close()
	}
	s.serverMu.RLock()
	server := s.server
	s.serverMu.RUnlock()
	if server == nil {
		return importJobsErr
	}
	s.logger.Info("shutting down API server")
	return errors.Join(importJobsErr, server.Shutdown(ctx))
}

// Router returns the HTTP router for testing.
func (s *Server) Router() http.Handler {
	return s.router
}

// analyticsEngineMiddleware snapshots the immutable engine/mode pair into the
// request context. A runtime swap is one atomic pointer store, so it never
// waits for a request and each request keeps one consistent view.
func (s *Server) analyticsEngineMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := s.analyticsState.Load()
		ctx := context.WithValue(r.Context(), analyticsEngineContextKey{}, state)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) analyticsStateForContext(ctx context.Context) *analyticsEngineState {
	if ctx != nil {
		if state, ok := ctx.Value(analyticsEngineContextKey{}).(*analyticsEngineState); ok && state != nil {
			return state
		}
	}
	return s.currentAnalyticsState()
}

func (s *Server) currentAnalyticsState() *analyticsEngineState {
	return s.analyticsState.Load()
}

func (s *Server) queryEngineForContext(ctx context.Context) query.Engine {
	state := s.analyticsStateForContext(ctx)
	if state == nil {
		return nil
	}
	return state.engine
}

func (s *Server) analyticsModeForContext(ctx context.Context) string {
	state := s.analyticsStateForContext(ctx)
	if state == nil {
		return ""
	}
	return state.mode
}

func (s *Server) analyticsInitializingForContext(ctx context.Context) bool {
	return s.analyticsModeForContext(ctx) == AnalyticsModeInitializing
}

func (s *Server) analyticsCacheInitializingForContext(ctx context.Context) bool {
	state := s.analyticsStateForContext(ctx)
	return state != nil && (state.mode == AnalyticsModeInitializing || state.analyticsInitializationActive)
}

// QueryEngine returns the analytics engine currently serving API queries.
// The daemon uses it when a request must follow a runtime engine swap.
func (s *Server) QueryEngine() query.Engine {
	state := s.currentAnalyticsState()
	if state == nil {
		return nil
	}
	return state.engine
}

// QueryEngineForRequest returns the engine snapshot carried by a routed
// request context. Callbacks invoked by handlers use it to stay on the same
// engine even if background initialization installs a new state.
func (s *Server) QueryEngineForRequest(ctx context.Context) query.Engine {
	return s.queryEngineForContext(ctx)
}

// AnalyticsMode returns the analytics mode currently reported by health
// endpoints. The engine and mode come from one immutable state snapshot.
func (s *Server) AnalyticsMode() string {
	state := s.currentAnalyticsState()
	if state == nil {
		return ""
	}
	return state.mode
}

// SetAnalyticsEngine installs an engine and its matching health mode as one
// state transition. It deliberately does not close the previous engine:
// daemon-owned engines remain live until HTTP shutdown and background workers
// have stopped.
func (s *Server) SetAnalyticsEngine(engine query.Engine, mode string) {
	for {
		current := s.currentAnalyticsState()
		next := &analyticsEngineState{engine: engine, mode: mode}
		if current != nil {
			next.analyticsInitializationActive = current.analyticsInitializationActive
		}
		if s.analyticsState.CompareAndSwap(current, next) {
			return
		}
	}
}

// SetAnalyticsInitializationActive reports whether the daemon is still
// selecting, building, or opening the analytical cache. It is separate from
// AnalyticsMode because auto mode keeps serving SQL-backed routes while that
// work runs.
func (s *Server) SetAnalyticsInitializationActive(active bool) {
	for {
		current := s.currentAnalyticsState()
		next := &analyticsEngineState{analyticsInitializationActive: active}
		if current != nil {
			next.engine = current.engine
			next.mode = current.mode
		}
		if s.analyticsState.CompareAndSwap(current, next) {
			return
		}
	}
}

// AnalyticsInitializationActive reports whether background cache
// initialization is still in progress.
func (s *Server) AnalyticsInitializationActive() bool {
	state := s.currentAnalyticsState()
	return state != nil && state.analyticsInitializationActive
}

// CloseAnalyticsEngine closes and clears the current engine after the daemon
// has shut down HTTP handling. If HTTP shutdown fails, the daemon skips this
// call so a late request cannot use an engine while it is being closed.
func (s *Server) CloseAnalyticsEngine() error {
	state := s.analyticsState.Swap(&analyticsEngineState{})
	var engine query.Engine
	if state != nil {
		engine = state.engine
	}
	if engine == nil {
		return nil
	}
	return engine.Close()
}

func (s *Server) requestUsesCLITimeoutPolicy(r *http.Request) bool {
	if r.Header.Get(apiprotocol.ClientClassHeader) != apiprotocol.ClientClassCLI {
		return false
	}
	return s.requestAuthentication(r).trustedForCLIDuration
}

func serveWithoutRequestDeadlines(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
) {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Time{})
	_ = controller.SetWriteDeadline(time.Time{})
	next.ServeHTTP(w, r)
}

func serveWithoutWriteDeadline(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
) {
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})
	next.ServeHTTP(w, r)
}

func serveMeetingImportWithReadDeadline(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
) {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(DaemonLongRequestTimeout))
	_ = controller.SetWriteDeadline(time.Time{})
	next.ServeHTTP(w, r)
}

func serveWithProtectiveRequestDeadline(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
) {
	ctx, cancel := context.WithTimeout(r.Context(), DaemonLongRequestTimeout)
	defer cancel()

	if deadline, ok := ctx.Deadline(); ok {
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(deadline)
	}
	next.ServeHTTP(w, r.WithContext(ctx))
}

func (s *Server) timeoutMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost &&
			r.URL.Path == meetingImportEndpointPath &&
			s.apiRequestAuthorized(r) {
			serveMeetingImportWithReadDeadline(w, r, next)
			return
		}
		if cardDAVRequestNeedsProtectiveCeiling(r) {
			serveWithProtectiveRequestDeadline(w, r, next)
			return
		}
		if s.requestUsesCLITimeoutPolicy(r) {
			if cliRequestNeedsProtectiveCeiling(r) {
				serveWithProtectiveRequestDeadline(w, r, next)
				return
			}
			serveWithoutRequestDeadlines(w, r, next)
			return
		}

		timeout, bounded := s.requestTimeoutForPath(r.URL.Path)
		if !bounded {
			serveWithoutWriteDeadline(w, r, next)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// cardDAVRequestNeedsProtectiveCeiling identifies routes that may perform
// upstream CardDAV requests. Their client has its own five-minute operation
// budget, so the ordinary one-minute API timeout must not preempt it.
func cardDAVRequestNeedsProtectiveCeiling(r *http.Request) bool {
	switch r.Method + " " + r.URL.Path {
	case "POST /api/v1/carddav/account/test",
		"PUT /api/v1/carddav/account",
		"POST /api/v1/carddav/sync":
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/carddav/publications/") {
		return r.Method == http.MethodPost || r.Method == http.MethodDelete
	}
	return r.Method == http.MethodPost &&
		strings.HasPrefix(r.URL.Path, "/api/v1/carddav/conflicts/") &&
		strings.HasSuffix(r.URL.Path, "/resolve")
}

// cliRequestNeedsProtectiveCeiling identifies marked CLI routes whose
// production work still includes a filesystem, planner, or synchronous cache
// phase that cannot be interrupted at every point. They get the generous
// daemon ceiling until end-to-end cancellation is proven for the whole route.
func cliRequestNeedsProtectiveCeiling(r *http.Request) bool {
	switch r.Method + " " + r.URL.Path {
	case "GET /api/v1/cli/cache-stats",
		"POST /api/v1/cli/build-cache",
		"POST /api/v1/cli/add-calendar/plan",
		"POST /api/v1/cli/delete-staged/plan",
		"POST /api/v1/cli/deletion-manifests",
		"POST /api/v1/cli/embeddings/plan",
		"GET /api/v1/cli/message",
		"GET /api/v1/cli/message/raw",
		"GET /api/v1/cli/attachment",
		"GET /api/v1/cli/search",
		"POST /api/v1/cli/deduplicate/plan",
		"POST /api/v1/cli/identities",
		"DELETE /api/v1/cli/identities",
		"POST /api/v1/cli/identities/import":
		return true
	default:
		return false
	}
}

// requestTimeoutForPath returns the context deadline to impose on a request
// and whether one applies at all. POST /api/v1/query gets its own generous
// ceiling; the streaming/long-running CLI operations and meeting imports stay
// unbounded (they report progress and are gated by the operation gate);
// everything else gets the standard per-request timeout.
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
		importJobsEndpointPath,
		"/api/v1/carddav/sync",
		"/api/v1/cli/deduplicate/plan",
		meetingImportEndpointPath,
		"/api/v1/cli/identities/discover",
		"/api/v1/cli/rebuild-fts",
		"/api/v1/cli/repair-encoding",
		"/api/v1/cli/repair-message",
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
				pathKey, r.URL.Path,
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
					pathKey, path,
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
					pathKey, r.URL.Path,
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
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = fmt.Sprintf("msgvault-%d", nextRequestID.Add(1))
		}
		w.Header().Set("X-Request-ID", id)
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
	if r.Method == http.MethodPost && r.URL.Path == sessionLoginPath {
		return false
	}
	return isLoopbackRequest(r) && s.apiRequestAuthorized(r)
}

func (s *Server) logUnauthorizedAPIRequest(r *http.Request) {
	s.logger.Warn("unauthorized API request",
		pathKey, r.URL.Path,
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

	w.Header().Set("Content-Type", applicationJSONMediaType)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"shutting_down"}`))
	go s.shutdownFunc()
}

func (s *Server) handleDaemonIdentity(w http.ResponseWriter, r *http.Request) {
	if s.shutdownToken == "" {
		writeError(w, http.StatusNotFound, "identity_unavailable", "Daemon identity proof is not available")
		return
	}
	proof, err := daemonauth.Proof(
		s.shutdownToken,
		r.Header.Get(DaemonIdentityChallengeHeader),
		os.Getpid(),
	)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_challenge", "Invalid daemon identity challenge")
		return
	}
	w.Header().Set(DaemonIdentityProofHeader, proof)
	w.WriteHeader(http.StatusNoContent)
}

// handleHealth returns a simple health check response. It is served
// unauthenticated, so the vector view carries no detail message — init and
// drift details can name configured account identifiers.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.refreshVectorStatus(r.Context())
	writeJSON(w, http.StatusOK, HealthResponse{
		Status:          "ok",
		Vector:          s.vectorHealthPublic(),
		Operation:       s.operationBusyHealth(),
		AnalyticsEngine: s.analyticsModeForContext(r.Context()),
	})
}

// handleAuthenticatedHealth returns health details that are safe behind the
// API-key boundary.
func (s *Server) handleAuthenticatedHealth(w http.ResponseWriter, r *http.Request) {
	s.refreshVectorStatus(r.Context())
	writeJSON(w, http.StatusOK, HealthResponse{
		Status:           "ok",
		Vector:           s.vectorHealth(),
		Operation:        s.operationHealth(),
		AnalyticsEngine:  s.analyticsModeForContext(r.Context()),
		APISchemaVersion: APISchemaVersion,
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
