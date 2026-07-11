package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/syncerr"
	"go.kenn.io/msgvault/internal/teams"
	"golang.org/x/oauth2"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run msgvault as a daemon with scheduled sync",
	Long: `Run msgvault as a long-running daemon that syncs email accounts on schedule.

The daemon runs in the foreground and performs:
  - HTTP API server (auto-selects an open port unless [server] api_port is set)
  - Scheduled incremental syncs based on account config
  - Automatic cache rebuilds after each sync

Configure schedules in config.toml:
  [[accounts]]
  email = "you@gmail.com"
  schedule = "0 2 * * *"   # 2am daily (cron format)
  enabled = true

Cron format: minute hour day-of-month month day-of-week
  Examples:
    0 2 * * *     = 2:00 AM daily
    */15 * * * *  = Every 15 minutes
    0 0 * * 0     = Midnight on Sundays
    0 8,18 * * *  = 8 AM and 6 PM daily

Use Ctrl+C to stop the daemon gracefully.`,
	RunE: runServe,
}

const daemonIdleTimeoutEnv = "MSGVAULT_DAEMON_IDLE_TIMEOUT"

var buildCacheSubprocessForRun = buildCacheSubprocess

type serveRuntimeAPIServer interface {
	Shutdown(ctx context.Context) error
}

type serveRuntimeScheduler interface {
	Stop() context.Context
}

type serveRuntimeOperationGate interface {
	StartDrain()
	Wait(ctx context.Context) error
}

func init() {
	rootCmd.AddCommand(serveCmd)
	serveCmd.AddCommand(serveStartCmd)
	serveCmd.AddCommand(serveStatusCmd)
	serveCmd.AddCommand(serveStopCmd)
	serveCmd.AddCommand(serveRestartCmd)
}

func runServe(cmd *cobra.Command, args []string) error {
	// Validate security posture before doing any work
	if err := cfg.Server.ValidateSecure(); err != nil {
		return err
	}
	if cfg.Server.APIKey != "" && len(cfg.Server.APIKey) < 16 {
		logger.Warn("api_key is very short — use a randomly generated key of at least 32 characters")
	}

	// Missing provider credentials should not prevent the daemon from serving
	// read-only HTTP requests against an import-only archive.
	if !hasServeOAuthConfig(cfg) {
		logger.Warn("OAuth/Microsoft credentials not configured - daemon will serve API requests, but scheduled provider syncs require credentials")
	}

	// Check for scheduled accounts (warn but don't fail - allows token upload first)
	scheduled := cfg.ScheduledAccounts()
	if len(scheduled) == 0 {
		logger.Warn("no scheduled accounts configured - server will start but no syncs will run",
			"hint", "Add accounts to config.toml or upload tokens via API first")
	}

	bindAddr := cfg.Server.BindAddr
	if bindAddr == "" {
		bindAddr = defaultDaemonBindAddr
	}
	apiListener, err := listenServeAPI(bindAddr, cfg.Server.APIPort)
	if err != nil {
		return err
	}
	listenerReserved := true
	defer func() {
		if listenerReserved {
			_ = apiListener.Close()
		}
	}()

	// Record the ACTUAL bound port, not cfg.Server.APIPort: when api_port is
	// unset (0) the listener binds an ephemeral port, and this is the port
	// clients discover through the daemon runtime record.
	boundPort, err := listenerPort(apiListener)
	if err != nil {
		return err
	}

	ownership, err := claimServeOwnership(cmd.Context(), cfg, bindAddr, boundPort, Version)
	if err != nil {
		return fmt.Errorf("claim daemon ownership: %w", err)
	}
	defer func() {
		if err := ownership.Close(); err != nil {
			logger.Warn("release daemon ownership failed", "error", err)
		}
	}()

	// Open database
	dbPath := cfg.DatabaseDSN()
	logger.Info("daemon startup step", "step", "open_archive_database", "database", daemonStartupDatabaseLabel(dbPath))
	s, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = s.Close() }()
	logger.Info("daemon startup step complete", "step", "open_archive_database")

	logger.Info("daemon startup step", "step", "init_archive_schema")
	if err := s.InitSchema(); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	logger.Info("daemon startup step complete", "step", "init_archive_schema")
	// Legacy [identity] migration is deferred to the first scheduled sync's
	// runPostSourceCreateMigrations call, which fires AFTER that sync's
	// confirmDefaultIdentity. Calling the migration here would race
	// confirmDefaultIdentity for upgraded DBs with sources + a legacy
	// [identity] block — same ordering hole the ingest commands already
	// close by routing the legacy migration exclusively post-source-create.

	// Set up cancellable context early so vector-backend initialization
	// (which may open files and run migrations) respects Ctrl+C.
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	idleTracker := newDaemonIdleTracker(cfg, cancel)
	operationGate := api.NewSerialOperationGate()
	// Closed on shutdown so cached pack readers don't hold attachment pack
	// files open past the daemon's lifetime (blocks deletion on Windows).
	attachmentMaint, err := newAttachmentMaintenance(s, cfg.AttachmentsDir(), logger)
	if err != nil {
		return fmt.Errorf("open attachment maintenance: %w", err)
	}
	defer func() { _ = attachmentMaint.close() }()
	blobStore := attachmentMaint.blob

	// Vector misconfiguration still fails startup fast; the expensive
	// backend open/migrate/backfill runs in the background after the API
	// server is listening (startVectorInit below), so the TUI and other
	// clients are not blocked by vector maintenance.
	if err := precheckVectorFeatures(dbPath); err != nil {
		return fmt.Errorf("vector features: %w", err)
	}
	if !cfg.Vector.Enabled {
		logger.Info("daemon startup step", "step", "skip_vector_backend", "enabled", false)
	}

	logger.Info("daemon startup step", "step", "init_analytics_engine")
	engine, analyticsMode, err := openDaemonAnalyticsEngine(cmd.Context(), cfg, s)
	if err != nil {
		return err
	}
	defer func() { _ = engine.Close() }()
	logger.Info("daemon startup step complete", "step", "init_analytics_engine")

	getOAuthMgr := oauthManagerCache()

	// Create sync function for the scheduler. Under scan-and-fill the
	// Syncer no longer needs an enqueuer — newly-ingested messages get
	// embed_gen = NULL by column default and the embed worker (registered
	// by the background startVectorInit) discovers them on its next run, so
	// the sync path no longer threads the vector features.
	syncFunc := func(ctx context.Context, email string) error {
		return runScheduledSource(ctx, attachmentMaint, true, func(ctx context.Context) error {
			return runScheduledSync(ctx, email, s, getOAuthMgr)
		})
	}

	// Create and configure scheduler
	sched := scheduler.New(syncFunc).WithLogger(logger).
		WithWorkTracker(combineWorkTrackers(idleTracker, labelWorkTracker(operationGate, "a scheduled sync")))

	// Add all scheduled accounts
	count, errs := sched.AddAccountsFromConfig(cfg)
	if len(errs) > 0 {
		for _, err := range errs {
			logger.Error("failed to schedule account", "error", err)
		}
	}
	if count == 0 {
		logger.Warn("no accounts scheduled - upload tokens via API and add accounts to config.toml")
	}

	for _, src := range cfg.ScheduledSynctechSMSSources() {
		source := src
		jobName := "synctech-sms:" + source.Name
		if err := sched.AddJob(scheduler.Job{
			Name:     jobName,
			Schedule: source.Schedule,
			Run: func(ctx context.Context) error {
				return runScheduledSource(ctx, attachmentMaint, true, func(ctx context.Context) error {
					return runConfiguredSynctechSMSSourceWithStore(ctx, s, source)
				})
			},
		}); err != nil {
			logger.Error("failed to schedule synctech-sms source", "source", source.Name, "error", err)
		} else {
			logger.Info("scheduled synctech-sms source", "source", source.Name, "schedule", source.Schedule)
		}
	}

	// Warn about enabled calendar sources with no schedule: they are never
	// daemon-synced, so once a manual sync seeds the source row its freshness
	// drifts stale and the freshness monitor eventually alarms RED.
	for _, src := range cfg.GCal {
		if src.Enabled && src.Schedule == "" {
			logger.Warn("gcal source is enabled but has no schedule — the daemon will not sync it; its freshness will eventually go stale",
				"source", src.Name, "email", src.Email,
				"hint", `set a cron schedule (e.g. "0 */6 * * *") on the [[gcal]] entry`)
		}
	}

	for _, src := range cfg.ScheduledGCalSources() {
		source := src
		jobName := "gcal:" + source.Name
		if err := sched.AddJob(scheduler.Job{
			Name:     jobName,
			Schedule: source.Schedule,
			Run: func(ctx context.Context) error {
				return runScheduledSource(ctx, attachmentMaint, false, func(ctx context.Context) error {
					return runConfiguredGCalSync(ctx, s, source)
				})
			},
		}); err != nil {
			logger.Error("failed to schedule gcal source", "source", source.Name, "error", err)
		} else {
			logger.Info("scheduled gcal source", "source", source.Name, "schedule", source.Schedule)
		}
	}
	if err := registerAttachmentMaintenanceJob(sched, attachmentMaint); err != nil {
		return fmt.Errorf("schedule attachment maintenance: %w", err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start the scheduler
	sched.Start()

	// Create adapters for the API interfaces
	storeAdapter := &storeAPIAdapter{store: s, attachmentMaintenance: attachmentMaint}
	schedAdapter := &schedulerAdapter{scheduler: sched}

	// Create and start API server
	apiOpts := api.ServerOptions{
		Config: cfg,
		Store:  storeAdapter,
		Engine: engine,
		SQLQueryRunner: func(ctx context.Context, sql string) (*query.QueryResult, error) {
			return runDaemonSQLQuery(ctx, cfg, s, engine, sql)
		},
		ShutdownToken: ownership.shutdownToken,
		ShutdownFunc:  cancel,
		Scheduler:     schedAdapter,
		Logger:        logger,
		DaemonVersion: Version,
		AnalyticsMode: analyticsMode,
		IdleTracker:   idleTracker,
		OperationGate: operationGate,
		BlobStore:     blobStore,
	}
	if cfg.Vector.Enabled {
		apiOpts.VectorStatus = api.VectorStatusInitializing
	}
	apiServer := api.NewServerWithOptions(apiOpts)

	// Start API server in goroutine
	apiAddr := apiListener.Addr().String()
	logger.Info("daemon startup step", "step", "start_api_server", "bind", apiAddr)
	serverErr := make(chan error, 1)
	listenerReserved = false
	go func() {
		if err := apiServer.StartOnListener(apiListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()
	if idleTracker != nil {
		idleTracker.Touch()
		go idleTracker.Run(ctx)
		logger.Info("background daemon idle shutdown enabled", "timeout", cfg.Server.DaemonIdleTimeout)
	}

	vectorInit := startVectorInit(
		ctx, s, dbPath,
		combineWorkTrackers(idleTracker, labelWorkTracker(operationGate, "background embedding work")),
		apiServer, sched,
	)

	fmt.Printf("msgvault daemon started\n")
	fmt.Printf("  API server: http://%s\n", apiAddr)
	fmt.Printf("  Scheduled accounts: %d\n", count)
	fmt.Printf("  Data directory: %s\n", cfg.Data.DataDir)
	fmt.Println()
	fmt.Println("Press Ctrl+C to stop.")
	fmt.Println()

	// Print schedule info
	for _, status := range sched.Status() {
		fmt.Printf("  %s: next sync at %s\n", status.Email, status.NextRun.Local().Format("2006-01-02 15:04:05"))
	}
	fmt.Println()

	// Wait for shutdown signal or server error
	var serverStartupErr error
	select {
	case sig := <-sigChan:
		logger.Info("received shutdown signal", "signal", sig)
		fmt.Printf("\nReceived %s, shutting down...\n", sig)
	case err := <-serverErr:
		logger.Error("API server error", "error", err)
		fmt.Printf("\nAPI server error: %v\n", err)
		serverStartupErr = err
	case <-ctx.Done():
		logger.Info("context cancelled")
	}

	// Stop background work first: vector init honors ctx, so cancelling
	// lets the operation-gate drain inside shutdownServeRuntime complete.
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), serveOperationDrainTimeout)
	defer shutdownCancel()
	shutdownErr := shutdownServeRuntime(shutdownCtx, cmd.OutOrStdout(), apiServer, sched, operationGate)
	// Wait for the background vector init regardless of the shutdown
	// outcome: the deferred s.Close() must not run under a still-running
	// init goroutine, and vectors.db needs closing whenever init finished.
	// Bound the wait by the time REMAINING on shutdownCtx rather than a
	// fresh full drain window — shutdownServeRuntime already consumed part
	// of it, and `serve stop` budgets only one drain window before it kills
	// the daemon (serveStopGraceTimeout).
	if vectorInit.WaitContext(shutdownCtx) {
		vectorInit.CloseFeatures()
	} else {
		logger.Warn("vector init did not stop within the shutdown drain timeout; skipping vectors.db close")
	}
	if shutdownErr != nil {
		logger.Error("daemon shutdown error", "error", shutdownErr)
		return shutdownErr
	}
	if serverStartupErr != nil {
		return fmt.Errorf("API server: %w", serverStartupErr)
	}

	return nil
}

func listenServeAPI(bindAddr string, port int) (net.Listener, error) {
	if bindAddr == "" {
		bindAddr = defaultDaemonBindAddr
	}
	addr := net.JoinHostPort(bindAddr, strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if port != 0 {
			return nil, fmt.Errorf(
				"API server address unavailable at %s: %w "+
					"(set a different [server] api_port, or unset it to auto-select an open port)",
				addr, err)
		}
		return nil, fmt.Errorf("API server address unavailable at %s: %w", addr, err)
	}
	return ln, nil
}

// listenerPort extracts the TCP port a listener bound to. With an ephemeral
// (api_port = 0) bind this is the OS-assigned port that clients discover
// through the daemon runtime record.
func listenerPort(ln net.Listener) (int, error) {
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
		return tcpAddr.Port, nil
	}
	_, portText, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return 0, fmt.Errorf("parse API listener address %q: %w", ln.Addr().String(), err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0, fmt.Errorf("parse API listener port %q: %w", portText, err)
	}
	return port, nil
}

func daemonStartupDatabaseLabel(dsn string) string {
	if store.IsPostgresURL(dsn) {
		return "postgres://<redacted>"
	}
	return dsn
}

func shutdownServeRuntime(
	ctx context.Context,
	out io.Writer,
	apiServer serveRuntimeAPIServer,
	sched serveRuntimeScheduler,
	gate serveRuntimeOperationGate,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		out = io.Discard
	}
	if gate != nil {
		gate.StartDrain()
	}
	_, _ = fmt.Fprintln(out, "Shutting down API server...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), serveAPIShutdownTimeout)
	defer shutdownCancel()
	var shutdownErr error
	if apiServer != nil {
		shutdownErr = apiServer.Shutdown(shutdownCtx)
	}

	_, _ = fmt.Fprintln(out, "Waiting for running syncs to complete...")
	var schedCtx context.Context
	if sched != nil {
		schedCtx = sched.Stop()
	}
	if schedCtx != nil {
		select {
		case <-schedCtx.Done():
		case <-time.After(serveSchedulerStopTimeout):
			_, _ = fmt.Fprintln(out, "Shutdown timed out after 30 seconds.")
		}
	}

	if gate != nil {
		_, _ = fmt.Fprintln(out, "Waiting for active archive operations to complete...")
		if err := gate.Wait(ctx); err != nil {
			return fmt.Errorf("wait for active archive operations: %w", err)
		}
	}
	_, _ = fmt.Fprintln(out, "Shutdown complete.")
	if shutdownErr != nil {
		return fmt.Errorf("API server shutdown: %w", shutdownErr)
	}
	return nil
}

func runDaemonSQLQuery(
	ctx context.Context,
	c *config.Config,
	s *store.Store,
	engine query.Engine,
	sqlStr string,
) (*query.QueryResult, error) {
	if c == nil || s == nil {
		return nil, errors.New("daemon query unavailable")
	}
	if s.IsPostgreSQL() {
		if querier, ok := engine.(query.SQLQuerier); ok {
			return querier.QuerySQL(ctx, sqlStr)
		}
		return nil, errors.New("SQL query requires DuckDB engine")
	}

	dbPath := c.DatabaseDSN()
	analyticsDir := c.AnalyticsDir()
	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	if !staleness.NeedsBuild {
		if querier, ok := engine.(query.SQLQuerier); ok {
			return querier.QuerySQL(ctx, sqlStr)
		}
	}

	if staleness.NeedsBuild {
		if err := buildCacheSubprocessForRun(ctx, staleness.FullRebuild); err != nil {
			return nil, fmt.Errorf("build cache: %w", err)
		}
		logger.Info("rebuilt analytics cache for SQL query",
			"reason", staleness.Reason,
			"full_rebuild", staleness.FullRebuild)
	}

	duckEngine, err := openDaemonDuckDBEngine(c, s)
	if err != nil {
		return nil, fmt.Errorf("open DuckDB query engine: %w", err)
	}
	defer func() { _ = duckEngine.Close() }()

	return duckEngine.QuerySQL(ctx, sqlStr)
}

// openDaemonAnalyticsEngine picks the daemon's analytics engine once at
// startup and also returns the api.AnalyticsMode constant describing that
// choice, which /health reports so clients (the TUI launch notice) can tell
// cache-backed aggregates from live-SQL fallback.
func openDaemonAnalyticsEngine(
	ctx context.Context,
	c *config.Config,
	s *store.Store,
) (query.Engine, string, error) {
	if c == nil || s == nil {
		return nil, "", errors.New("daemon analytics engine unavailable")
	}
	if s.IsPostgreSQL() {
		return query.NewEngine(s.DB(), true), api.AnalyticsModePostgres, nil
	}

	engineMode := c.Analytics.Engine
	if engineMode == "" {
		engineMode = config.AnalyticsEngineAuto
	}
	if engineMode == config.AnalyticsEngineSQL {
		logger.Info("using live SQL analytics engine",
			"engine", engineMode)
		return query.NewEngine(s.DB(), false), api.AnalyticsModeSQL, nil
	}

	dbPath := c.DatabaseDSN()
	analyticsDir := c.AnalyticsDir()
	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	if staleness.NeedsBuild && c.Analytics.AutoBuildCache && engineMode == config.AnalyticsEngineDuckDB {
		if err := buildCacheSubprocessForRun(ctx, staleness.FullRebuild); err != nil {
			return nil, "", fmt.Errorf("build analytics cache: %w", err)
		}
		logger.Info("rebuilt analytics cache",
			"reason", staleness.Reason,
			"full_rebuild", staleness.FullRebuild)
		staleness = cacheNeedsBuild(dbPath, analyticsDir)
	}

	if !staleness.NeedsBuild && query.HasCompleteParquetData(analyticsDir) {
		duckEngine, err := openDaemonDuckDBEngine(c, s)
		if err != nil {
			if engineMode == config.AnalyticsEngineDuckDB {
				return nil, "", err
			}
			logger.Warn("DuckDB engine failed, falling back to live SQL",
				"error", err)
			return query.NewEngine(s.DB(), false), api.AnalyticsModeSQLFallback, nil
		}
		return duckEngine, api.AnalyticsModeDuckDB, nil
	}

	if engineMode == config.AnalyticsEngineDuckDB {
		reason := staleness.Reason
		if reason == "" {
			reason = "analytics cache is missing or incomplete"
		}
		return nil, "", fmt.Errorf("analytics engine=duckdb requires a usable cache: %s", reason)
	}
	if staleness.Reason != "" {
		logger.Info("analytics cache not usable, using live SQL engine",
			"reason", staleness.Reason,
			"auto_build_cache", c.Analytics.AutoBuildCache)
	} else {
		logger.Info("analytics cache not built - using live SQL engine (run 'msgvault build-cache' for faster aggregates)",
			"auto_build_cache", c.Analytics.AutoBuildCache)
	}
	return query.NewEngine(s.DB(), false), api.AnalyticsModeSQLFallback, nil
}

func openDaemonDuckDBEngine(c *config.Config, s *store.Store) (*query.DuckDBEngine, error) {
	if c == nil || s == nil {
		return nil, errors.New("daemon DuckDB engine unavailable")
	}
	// DisableSQLiteScanner keeps DuckDB's bundled SQLite library from
	// ATTACHing the live database for the daemon's lifetime, which can
	// interfere with the daemon's own go-sqlite3 WAL/lock state. Detail
	// queries route through the shared go-sqlite3 connection instead;
	// aggregates still read Parquet.
	return query.NewDuckDBEngine(
		c.AnalyticsDir(),
		c.DatabaseDSN(),
		s.DB(),
		query.DuckDBOptions{DisableSQLiteScanner: true},
	)
}

func hasServeOAuthConfig(c *config.Config) bool {
	if c == nil {
		return false
	}
	return c.OAuth.HasAnyConfig() || c.Microsoft.ClientID != ""
}

func newDaemonIdleTracker(c *config.Config, stop context.CancelFunc) *api.IdleTracker {
	if c == nil || os.Getenv(serveBackgroundChildEnv) != "1" {
		return nil
	}
	timeout := c.Server.DaemonIdleTimeout
	if raw := os.Getenv(daemonIdleTimeoutEnv); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			logger.Warn("invalid daemon idle timeout override ignored",
				"env", daemonIdleTimeoutEnv,
				"value", raw,
				"error", err)
		} else {
			timeout = parsed
		}
	}
	if timeout <= 0 {
		return nil
	}
	return api.NewIdleTracker(timeout, func() {
		logger.Info("background daemon idle timeout elapsed; shutting down", "timeout", timeout)
		stop()
	})
}

// storeAPIAdapter adapts store.Store to the API store interfaces.
// Since api.APIMessage, api.StoreStats, etc. are type aliases for store types,
// the adapter methods are simple pass-throughs with no conversion needed.
type storeAPIAdapter struct {
	store                 *store.Store
	attachmentMaintenance *attachmentMaintenance
}

var _ api.MessageStore = (*storeAPIAdapter)(nil)
var _ api.CtxMessageStore = (*storeAPIAdapter)(nil)
var _ api.SourceStatusStore = (*storeAPIAdapter)(nil)
var _ api.CLIStore = (*storeAPIAdapter)(nil)
var _ api.CLIStartupMigrationStore = (*storeAPIAdapter)(nil)
var _ api.CLICacheBuilder = (*storeAPIAdapter)(nil)
var _ api.CLISyncRunner = (*storeAPIAdapter)(nil)
var _ api.CLIVerifyRunner = (*storeAPIAdapter)(nil)
var _ api.CLIRepairEncodingRunner = (*storeAPIAdapter)(nil)
var _ api.CLIRunner = (*storeAPIAdapter)(nil)
var _ api.CLIAddCalendarPlanner = (*storeAPIAdapter)(nil)
var _ api.CLIDeleteStagedPlanner = (*storeAPIAdapter)(nil)
var _ api.CLIDeletionManifestSaver = (*storeAPIAdapter)(nil)
var _ api.DeletionManifestLister = (*storeAPIAdapter)(nil)
var _ api.DeletionManifestCanceller = (*storeAPIAdapter)(nil)
var _ api.CLIDeduplicatePlanner = (*storeAPIAdapter)(nil)
var _ api.CLIEmbeddingsPlanner = (*storeAPIAdapter)(nil)
var _ api.CLIDedupDeleteStore = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) GetStats() (*api.StoreStats, error) {
	return a.store.GetStats()
}

func (a *storeAPIAdapter) GetStatsContext(ctx context.Context) (*api.StoreStats, error) {
	return a.store.GetStatsContext(ctx)
}

func (a *storeAPIAdapter) ListMessagesContext(
	ctx context.Context, offset, limit int,
) ([]api.APIMessage, int64, error) {
	return a.store.ListMessagesContext(ctx, offset, limit)
}

func (a *storeAPIAdapter) GetMessageContext(ctx context.Context, id int64) (*api.APIMessage, error) {
	return a.store.GetMessageContext(ctx, id)
}

func (a *storeAPIAdapter) GetMessagesSummariesByIDsContext(
	ctx context.Context, ids []int64,
) ([]api.APIMessage, error) {
	return a.store.GetMessagesSummariesByIDsContext(ctx, ids)
}

func (a *storeAPIAdapter) GetStatsForScope(sourceIDs []int64) (*store.Stats, error) {
	return a.store.GetStatsForScope(sourceIDs)
}

func (a *storeAPIAdapter) ListMessages(offset, limit int) ([]api.APIMessage, int64, error) {
	return a.store.ListMessages(offset, limit)
}

func (a *storeAPIAdapter) GetMessage(id int64) (*api.APIMessage, error) {
	return a.store.GetMessage(id)
}

func (a *storeAPIAdapter) GetMessagesSummariesByIDs(ids []int64) ([]api.APIMessage, error) {
	return a.store.GetMessagesSummariesByIDs(ids)
}

func (a *storeAPIAdapter) SearchMessages(query string, offset, limit int) ([]api.APIMessage, int64, error) {
	return a.store.SearchMessages(query, offset, limit)
}

func (a *storeAPIAdapter) SearchMessagesQuery(q *search.Query, offset, limit int) ([]api.APIMessage, int64, error) {
	return a.store.SearchMessagesQuery(q, offset, limit)
}

func (a *storeAPIAdapter) SearchMessagesContext(ctx context.Context, query string, offset, limit int) ([]api.APIMessage, int64, error) {
	return a.store.SearchMessagesContext(ctx, query, offset, limit)
}

func (a *storeAPIAdapter) SearchMessagesQueryContext(ctx context.Context, q *search.Query, offset, limit int) ([]api.APIMessage, int64, error) {
	return a.store.SearchMessagesQueryContext(ctx, q, offset, limit)
}

func (a *storeAPIAdapter) NeedsFTSBackfill() bool {
	return a.store.NeedsFTSBackfill()
}

func (a *storeAPIAdapter) NeedsFTSBackfillQuick() bool {
	return a.store.NeedsFTSBackfillQuick()
}

func (a *storeAPIAdapter) BackfillFTS(progress func(done, total int64)) (int64, error) {
	return a.store.BackfillFTS(progress)
}

func (a *storeAPIAdapter) RebuildFTS(progress func(done, total int64)) (int64, error) {
	return a.store.RebuildFTS(progress)
}

func (a *storeAPIAdapter) RunStartupMigrations(
	legacyIdentityAddresses []string,
) (store.StartupMigrationResult, error) {
	return a.store.RunStartupMigrations(legacyIdentityAddresses)
}

func (a *storeAPIAdapter) BuildCLICache(
	ctx context.Context,
	fullRebuild bool,
	emit func(api.CLICacheBuildEvent) error,
) error {
	return buildCacheSubprocessStream(ctx, fullRebuild, emit)
}

func (a *storeAPIAdapter) RunCLISync(
	ctx context.Context,
	req api.CLISyncRequest,
	emit func(api.CLISyncEvent) error,
) error {
	return a.runCLISyncWithRunner(ctx, req, emit, runDaemonCLISubprocessStream)
}

type cliSyncSubprocessRunner func(
	context.Context,
	[]string,
	func(stream, data string) error,
) error

func (a *storeAPIAdapter) runCLISyncWithRunner(
	ctx context.Context,
	req api.CLISyncRequest,
	emit func(api.CLISyncEvent) error,
	run cliSyncSubprocessRunner,
) error {
	emitSubprocess := func(stream, data string) error {
		if emit == nil {
			return nil
		}
		return emit(api.CLISyncEvent{Type: stream, Data: data})
	}
	emitWarning := func(message string) error {
		if emit == nil {
			return nil
		}
		return emit(api.CLISyncEvent{Type: "stderr", Data: message})
	}
	return runAfterSuccessfulAttachmentIngest(ctx, a.attachmentMaintenance, func(ctx context.Context) error {
		return run(ctx, cliSyncSubprocessArgs(req), emitSubprocess)
	}, emitWarning)
}

func cliSyncSubprocessArgs(req api.CLISyncRequest) []string {
	if req.Full {
		args := []string{"sync-full"}
		if req.Query != "" {
			args = append(args, "--query", req.Query)
		}
		if req.NoResume {
			args = append(args, "--noresume")
		}
		if req.Before != "" {
			args = append(args, "--before", req.Before)
		}
		if req.After != "" {
			args = append(args, "--after", req.After)
		}
		if req.Limit > 0 {
			args = append(args, "--limit", strconv.Itoa(req.Limit))
		}
		if req.Email != "" {
			args = append(args, req.Email)
		}
		return args
	}
	args := []string{"sync"}
	if req.Email != "" {
		args = append(args, req.Email)
	}
	return args
}

func (a *storeAPIAdapter) RunCLIVerify(
	ctx context.Context,
	req api.CLIVerifyRequest,
	emit func(api.CLIVerifyEvent) error,
) error {
	return runDaemonCLISubprocessStream(ctx, cliVerifySubprocessArgs(req), func(stream, data string) error {
		if emit == nil {
			return nil
		}
		return emit(api.CLIVerifyEvent{Type: stream, Data: data})
	})
}

func cliVerifySubprocessArgs(req api.CLIVerifyRequest) []string {
	args := []string{"verify"}
	if req.SampleSize != 100 {
		args = append(args, "--sample", strconv.Itoa(req.SampleSize))
	}
	if req.SkipDBCheck {
		args = append(args, "--skip-db-check")
	}
	if req.JSON {
		args = append(args, "--json")
	}
	args = append(args, req.Email)
	return args
}

func (a *storeAPIAdapter) RunCLIRepairEncoding(
	ctx context.Context,
	emit func(api.CLIRepairEncodingEvent) error,
) error {
	return runDaemonCLISubprocessStream(ctx, []string{"repair-encoding"}, func(stream, data string) error {
		if emit == nil {
			return nil
		}
		return emit(api.CLIRepairEncodingEvent{Type: stream, Data: data})
	})
}

func (a *storeAPIAdapter) RunCLICommand(
	ctx context.Context,
	req api.CLIRunRequest,
	emit func(api.CLIRunEvent) error,
) error {
	return a.runCLICommandWithRunner(ctx, req, emit, runDaemonCLISubprocessStreamWithEnv)
}

type cliCommandSubprocessRunner func(
	context.Context,
	[]string,
	map[string]string,
	string,
	func(stream, data string) error,
) error

func (a *storeAPIAdapter) runCLICommandWithRunner(
	ctx context.Context,
	req api.CLIRunRequest,
	emit func(api.CLIRunEvent) error,
	run cliCommandSubprocessRunner,
) error {
	emitSubprocess := func(stream, data string) error {
		if emit == nil {
			return nil
		}
		return emit(api.CLIRunEvent{Type: stream, Data: data})
	}
	runSubprocess := func(ctx context.Context) error {
		return run(ctx, req.Args, req.Env, req.Cwd, emitSubprocess)
	}
	if len(req.Args) > 0 && req.Args[0] == "repack-attachments" {
		if !repackAttachmentsParentArgsAllowed(req.Args[1:]) {
			return errors.New("repack-attachments accepts no command-specific arguments")
		}
		if a.attachmentMaintenance == nil {
			return errors.New("attachment maintenance is unavailable")
		}
		stats, err := a.attachmentMaintenance.repack(ctx, 0)
		if err != nil {
			return err
		}
		if emit != nil {
			var output bytes.Buffer
			writeRepackAttachmentsStats(&output, stats)
			if err := emit(api.CLIRunEvent{Type: cliStreamStdout, Data: output.String()}); err != nil {
				return err
			}
		}
		return nil
	}
	if !attachmentProducingCommand(req.Args) {
		if len(req.Args) == 0 || req.Args[0] != removeAccountCommandName {
			return runSubprocess(ctx)
		}
		emitWarning := func(message string) error {
			if emit == nil {
				return nil
			}
			return emit(api.CLIRunEvent{Type: cliStreamStderr, Data: message})
		}
		return runAfterSuccessfulAttachmentRemoval(
			ctx, a.attachmentMaintenance, runSubprocess, emitWarning,
		)
	}
	emitWarning := func(message string) error {
		if emit == nil {
			return nil
		}
		return emit(api.CLIRunEvent{Type: "stderr", Data: message})
	}
	return runAfterSuccessfulAttachmentIngest(
		ctx,
		a.attachmentMaintenance,
		runSubprocess,
		emitWarning,
	)
}

// repackAttachmentsParentArgsAllowed accepts only root logging flags that
// daemonCLIArgsFromCobra deliberately forwards after the command word. The
// parent performs repack itself rather than spawning a child, so those flags
// need no further interpretation here, but they must not be mistaken for
// command-specific arguments.
func repackAttachmentsParentArgsAllowed(args []string) bool {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			return false
		}
		name := strings.TrimPrefix(arg, "--")
		if before, _, ok := strings.Cut(name, "="); ok {
			name = before
		}
		if !loggingPassthroughFlags[name] {
			return false
		}
	}
	return true
}

func (a *storeAPIAdapter) PlanCLIAddCalendar(
	ctx context.Context,
	req api.CLIAddCalendarPlanRequest,
) (api.CLIAddCalendarPlanResponse, error) {
	return planCLIAddCalendar(ctx, a.store, req)
}

func (a *storeAPIAdapter) PlanCLIEmbeddings(
	ctx context.Context,
	req api.CLIEmbeddingsPlanRequest,
) (api.CLIEmbeddingsPlanResponse, error) {
	return planCLIEmbeddings(ctx, req)
}

func (a *storeAPIAdapter) PlanCLIDeleteStaged(
	ctx context.Context,
	req api.CLIDeleteStagedPlanRequest,
) (api.CLIDeleteStagedPlanResponse, error) {
	return planCLIDeleteStaged(ctx, a.store, req)
}

func (a *storeAPIAdapter) deletionManager() (*deletion.Manager, error) {
	mgr, err := deletion.NewManager(filepath.Join(cfg.Data.DataDir, "deletions"))
	if err != nil {
		return nil, fmt.Errorf("create deletion manager: %w", err)
	}
	return mgr, nil
}

func (a *storeAPIAdapter) SaveCLIDeletionManifest(_ context.Context, manifest *deletion.Manifest) error {
	mgr, err := a.deletionManager()
	if err != nil {
		return err
	}
	return mgr.SaveManifest(manifest)
}

func (a *storeAPIAdapter) ListDeletionManifests(_ context.Context, status deletion.Status) ([]*deletion.Manifest, error) {
	mgr, err := a.deletionManager()
	if err != nil {
		return nil, err
	}
	if status != "" {
		return mgr.ListByStatus(status)
	}
	var all []*deletion.Manifest
	for _, s := range deletion.PersistedStatuses() {
		manifests, err := mgr.ListByStatus(s)
		if err != nil {
			return nil, err
		}
		all = append(all, manifests...)
	}
	return all, nil
}

func (a *storeAPIAdapter) GetDeletionManifest(_ context.Context, id string) (*deletion.Manifest, deletion.Status, error) {
	mgr, err := a.deletionManager()
	if err != nil {
		return nil, "", err
	}
	return mgr.GetManifestWithStatus(id)
}

func (a *storeAPIAdapter) CancelDeletionManifest(_ context.Context, id string) error {
	mgr, err := a.deletionManager()
	if err != nil {
		return err
	}
	return mgr.CancelManifest(id)
}

func (a *storeAPIAdapter) PlanCLIDeduplicate(
	ctx context.Context,
	req api.CLIDeduplicatePlanRequest,
) (api.CLIDeduplicatePlanResponse, error) {
	return planCLIDeduplicate(ctx, a.store, req)
}

func (a *storeAPIAdapter) CountAllDeduped() (int64, int64, error) {
	return a.store.CountAllDeduped()
}

func (a *storeAPIAdapter) CountDedupedBatches(batchIDs []string) ([]store.DedupedBatchCount, int64, error) {
	return a.store.CountDedupedBatches(batchIDs)
}

func (a *storeAPIAdapter) DeleteAllDeduped() (int64, int64, error) {
	return a.store.DeleteAllDeduped()
}

func (a *storeAPIAdapter) DeleteDedupedBatch(batchID string) (int64, error) {
	return a.store.DeleteDedupedBatch(batchID)
}

func (a *storeAPIAdapter) BackupDatabase(dst string) error {
	return a.store.BackupDatabase(dst)
}

func (a *storeAPIAdapter) CountMessagesForSource(sourceID int64) (int64, error) {
	return a.store.CountMessagesForSource(sourceID)
}

func (a *storeAPIAdapter) CountSourceDeletedMessages(sourceIDs ...int64) (int64, error) {
	return a.store.CountSourceDeletedMessages(sourceIDs...)
}

func (a *storeAPIAdapter) ListSources(sourceType string) ([]*store.Source, error) {
	return a.store.ListSources(sourceType)
}

func (a *storeAPIAdapter) GetSourcesByIdentifierOrDisplayName(query string) ([]*store.Source, error) {
	return a.store.GetSourcesByIdentifierOrDisplayName(query)
}

func (a *storeAPIAdapter) GetSourcesByTypeAndAccount(
	sourceType, accountEmail string,
) ([]*store.Source, error) {
	return a.store.GetSourcesByTypeAndAccount(sourceType, accountEmail)
}

func (a *storeAPIAdapter) GetCollectionByName(name string) (*store.CollectionWithSources, error) {
	return a.store.GetCollectionByName(name)
}

func (a *storeAPIAdapter) ListCollections() ([]*store.CollectionWithSources, error) {
	return a.store.ListCollections()
}

func (a *storeAPIAdapter) CreateCollection(
	name, description string,
	sourceIDs []int64,
) (*store.Collection, error) {
	return a.store.CreateCollection(name, description, sourceIDs)
}

func (a *storeAPIAdapter) AddSourcesToCollection(name string, sourceIDs []int64) error {
	return a.store.AddSourcesToCollection(name, sourceIDs)
}

func (a *storeAPIAdapter) RemoveSourcesFromCollection(name string, sourceIDs []int64) error {
	return a.store.RemoveSourcesFromCollection(name, sourceIDs)
}

func (a *storeAPIAdapter) DeleteCollection(name string) error {
	return a.store.DeleteCollection(name)
}

func (a *storeAPIAdapter) UpdateSourceDisplayName(sourceID int64, displayName string) error {
	return a.store.UpdateSourceDisplayName(sourceID, displayName)
}

func (a *storeAPIAdapter) GetSourceByID(id int64) (*store.Source, error) {
	return a.store.GetSourceByID(id)
}

func (a *storeAPIAdapter) ListAccountIdentities(sourceID int64) ([]store.AccountIdentity, error) {
	return a.store.ListAccountIdentities(sourceID)
}

func (a *storeAPIAdapter) AddAccountIdentity(sourceID int64, address, signal string) error {
	return a.store.AddAccountIdentity(sourceID, address, signal)
}

func (a *storeAPIAdapter) RemoveAccountIdentity(sourceID int64, address string) (int64, error) {
	return a.store.RemoveAccountIdentity(sourceID, address)
}

func (a *storeAPIAdapter) GetActiveSync(sourceID int64) (*store.SyncRun, error) {
	return a.store.GetActiveSync(sourceID)
}

func (a *storeAPIAdapter) GetLatestSync(sourceID int64) (*store.SyncRun, error) {
	return a.store.GetLatestSync(sourceID)
}

func (a *storeAPIAdapter) GetLastSuccessfulSync(sourceID int64) (*store.SyncRun, error) {
	return a.store.GetLastSuccessfulSync(sourceID)
}

func (a *storeAPIAdapter) CountSyncRunItems(syncRunID int64, status string) (int64, error) {
	return a.store.CountSyncRunItems(syncRunID, status)
}

func (a *storeAPIAdapter) ListSyncRunItems(syncRunID int64, status string, limit int) ([]store.SyncRunItem, error) {
	return a.store.ListSyncRunItems(syncRunID, status, limit)
}

// schedulerAdapter adapts scheduler.Scheduler to api.SyncScheduler.
// Since api.AccountStatus is a type alias for scheduler.AccountStatus,
// the adapter methods are simple pass-throughs.
type schedulerAdapter struct {
	scheduler *scheduler.Scheduler
}

func (a *schedulerAdapter) IsScheduled(email string) bool {
	return a.scheduler.IsScheduled(email)
}

func (a *schedulerAdapter) TriggerSync(email string) error {
	return a.scheduler.TriggerSync(email)
}

func (a *schedulerAdapter) AddAccount(email, schedule string) error {
	return a.scheduler.AddAccount(email, schedule)
}

func (a *schedulerAdapter) IsRunning() bool {
	return a.scheduler.IsRunning()
}

func (a *schedulerAdapter) Status() []api.AccountStatus {
	return a.scheduler.Status()
}

// runScheduledSync performs a sync for a scheduled account. It resolves
// ALL syncable source rows for the identifier (gmail, imap, teams) and
// dispatches each in turn. When no source row matches, it falls back to
// the Gmail token-first workflow (tokens uploaded via API before the
// source row exists) so that legacy deployments keep working.
//
// Under scan-and-fill there is no enqueue step — newly-ingested messages
// get embed_gen = NULL by column default, so subsequent embed runs
// discover and pick them up by scanning; the sync path therefore needs
// no vector-feature wiring.
//
// Per-source errors are collected with errors.Join; the cache rebuild
// runs once after all sources regardless of per-source errors.
//
// The identifier passed in is whatever the scheduler holds — for Gmail
// this is the email address, for IMAP it's the full
// `imaps://user@host:port` URL recorded by `add-imap`, for Teams it is
// the UPN/email recorded by `add-o365`.
func runScheduledSync(ctx context.Context, identifier string, s *store.Store, getOAuthMgr func(string) (*oauth.Manager, error)) error {
	logger.Info("starting scheduled sync", "identifier", identifier)

	srcs, srcErr := findScheduledSyncSources(s, identifier)
	if srcErr != nil {
		return fmt.Errorf("look up sources for %s: %w", identifier, srcErr)
	}

	// No source row found: fall back to the Gmail token-first workflow
	// (preserves behaviour for tokens uploaded via API before the source
	// row exists).
	if len(srcs) == 0 {
		startTime := time.Now()
		summary, err := runScheduledGmailSync(ctx, identifier, nil, s, getOAuthMgr)
		if err != nil {
			return err
		}
		logger.Info("sync completed",
			"identifier", identifier,
			"source_type", sourceTypeGmail,
			"messages_added", summary.MessagesAdded,
			"duration", time.Since(startTime),
		)
		rebuildCacheAfterScheduledSync(ctx, identifier)
		return nil
	}

	var errs []error
	for _, src := range srcs {
		startTime := time.Now()
		sourceType := src.SourceType
		if sourceType == "" {
			sourceType = sourceTypeGmail
		}

		var (
			summary *gmail.SyncSummary
			err     error
		)
		switch sourceType {
		case sourceTypeGmail:
			summary, err = runScheduledGmailSync(ctx, identifier, src, s, getOAuthMgr)
		case sourceTypeIMAP:
			summary, err = runScheduledIMAPSync(ctx, src, s)
		case sourceTypeTeams:
			err = runScheduledTeamsSync(ctx, src, s)
		default:
			err = fmt.Errorf("source %q has type %q which is not supported by the daemon scheduler", identifier, sourceType)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s (%s): %w", identifier, sourceType, err))
			continue
		}

		if summary != nil {
			logger.Info("sync completed",
				"identifier", identifier,
				"source_type", sourceType,
				"messages_added", summary.MessagesAdded,
				"duration", time.Since(startTime),
			)
		} else {
			logger.Info("sync completed",
				"identifier", identifier,
				"source_type", sourceType,
				"duration", time.Since(startTime),
			)
		}
	}

	// Rebuild cache once after all sources, regardless of per-source errors.
	rebuildCacheAfterScheduledSync(ctx, identifier)

	return errors.Join(errs...)
}

// findScheduledSyncSources resolves ALL syncable source rows for a
// scheduler identifier. Returns at most one row per syncable type
// (gmail, imap, teams), in that stable order. Non-syncable types
// (mbox, apple-mail, etc.) are skipped.
//
// Returns an empty slice (not nil) when no syncable source matches —
// callers should fall back to the Gmail token-first workflow.
//
// Matches against both sources.identifier and sources.display_name.
func findScheduledSyncSources(s *store.Store, identifier string) ([]*store.Source, error) {
	rows, err := s.GetSourcesByIdentifierOrDisplayName(identifier)
	if err != nil {
		return nil, err
	}

	// Collect first occurrence of each syncable type.
	seen := make(map[string]*store.Source, 3)
	for _, src := range rows {
		switch src.SourceType {
		case sourceTypeGmail, sourceTypeIMAP, sourceTypeTeams:
			if _, dup := seen[src.SourceType]; !dup {
				seen[src.SourceType] = src
			}
		}
	}

	// Return in stable order: gmail, imap, teams.
	var result []*store.Source
	for _, t := range []string{sourceTypeGmail, sourceTypeIMAP, sourceTypeTeams} {
		if src, ok := seen[t]; ok {
			result = append(result, src)
		}
	}
	return result, nil
}

// runScheduledGmailSync runs an incremental Gmail sync for the daemon.
// Token-source lookup uses oauthMgr.TokenSource directly (not
// getTokenSourceWithReauth) because serve runs as a daemon and cannot
// open a browser for OAuth — the error path tells the user how to
// re-authorize from a terminal.
func runScheduledGmailSync(ctx context.Context, email string, src *store.Source, s *store.Store, getOAuthMgr func(string) (*oauth.Manager, error)) (*gmail.SyncSummary, error) {
	appName := ""
	if src != nil {
		appName = sourceOAuthApp(src)
	}

	var tokenSource oauth2.TokenSource
	var tsErr error

	if saKeyPath := cfg.OAuth.ServiceAccountKeyFor(appName); saKeyPath != "" {
		saMgr, saErr := oauth.NewServiceAccountManager(saKeyPath, oauth.Scopes)
		if saErr != nil {
			return nil, fmt.Errorf("service account for %s: %w", email, saErr)
		}
		tokenSource, tsErr = saMgr.TokenSource(ctx, email)
		if tsErr != nil {
			return nil, fmt.Errorf("service account token for %s: %w", email, tsErr)
		}
	} else {
		oauthMgr, oaErr := getOAuthMgr(appName)
		if oaErr != nil {
			return nil, fmt.Errorf("resolve OAuth credentials for %s: %w", email, oaErr)
		}
		tokenSource, tsErr = oauthMgr.TokenSource(ctx, email)
		if tsErr != nil {
			// Distinguish transient network failures (DNS lookup timeout,
			// dial timeout after laptop sleep/wake, Wi-Fi flap) from real
			// auth errors. Suggesting reauth on every network blip sends
			// the user down the wrong path.
			if syncerr.IsTransientNetwork(tsErr) {
				return nil, fmt.Errorf("get token source: %w (transient network error; will retry on next schedule)", tsErr)
			}
			if oauthMgr.HasToken(email) {
				return nil, fmt.Errorf("get token source: %w (token may be expired; run 'sync %s' or 'verify %s' from an interactive terminal to re-authorize)", tsErr, email, email)
			}
			return nil, fmt.Errorf("get token source: %w (run 'add-account %s' first)", tsErr, email)
		}
	}

	rateLimiter := gmail.NewRateLimiter(float64(cfg.Sync.RateLimitQPS))
	client := gmail.NewClient(tokenSource,
		gmail.WithLogger(logger),
		gmail.WithRateLimiter(rateLimiter),
	)
	defer func() { _ = client.Close() }()

	opts := sync.DefaultOptions()
	opts.AttachmentsDir = cfg.AttachmentsDir()

	syncer := sync.New(client, s, opts).WithLogger(logger)

	source, err := s.GetOrCreateSource(sourceTypeGmail, email)
	if err != nil {
		return nil, fmt.Errorf("get source: %w", err)
	}
	// Auto-default-identity must run BEFORE the legacy migration retry
	// — see comment in account_identity.go. serve is a daemon, so the
	// confirmation message has no terminal; discard it. Helper logs any
	// failure path through its own logger.Warn.
	confirmDefaultIdentity(io.Discard, s, source.ID, email, email, "account-identifier")
	if err := runPostSourceCreateMigrations(s); err != nil {
		return nil, fmt.Errorf("post-source-create migrations: %w", err)
	}

	summary, err := syncer.Incremental(ctx, source)
	if err != nil {
		// Once the history baseline expires (Gmail keeps only ~7 days),
		// every future incremental sync fails too; without a fallback the
		// account stays stuck until someone runs sync-full by hand. The
		// full sync is resumable and skips already-archived messages.
		if errors.Is(err, sync.ErrHistoryExpired) {
			logger.Warn("gmail history expired; falling back to full sync", keyEmail, email)
			summary, err = syncer.Full(ctx, source.Identifier)
			if err != nil {
				return nil, fmt.Errorf("full sync fallback failed: %w", err)
			}
			return summary, nil
		}
		return nil, fmt.Errorf("incremental sync failed: %w", err)
	}
	return summary, nil
}

// runScheduledIMAPSync runs a full IMAP sync for the daemon. IMAP has
// no incremental/history API, so we do a full pass, skipping mailboxes
// unchanged since the last completed sync (saved UIDVALIDITY/UIDNEXT)
// and relying on the store to dedupe by message-id. NoResume is forced
// on because IMAP page tokens are numeric offsets that don't survive
// across processes (see syncfull.go).
func runScheduledIMAPSync(ctx context.Context, src *store.Source, s *store.Store) (*gmail.SyncSummary, error) {
	apiClient, err := buildAPIClient(ctx, src, nil, nil, imapFolderStateOptions(s, src, false)...)
	if err != nil {
		return nil, fmt.Errorf("build IMAP client: %w", err)
	}
	defer func() { _ = apiClient.Close() }()

	opts := sync.DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	opts.AttachmentsDir = cfg.AttachmentsDir()
	opts.NoResume = true

	syncer := sync.New(apiClient, s, opts).WithLogger(logger)

	// runPostSourceCreateMigrations is keyed off Gmail-only legacy
	// state, so it's a no-op for fresh IMAP installs; we still call it
	// for parity with the Gmail path so the daemon converges legacy
	// DBs that happen to mix sources.
	//
	// Pass display_name (the IMAP username/email recorded by `add-imap`),
	// not Identifier (the `imaps://...` URL), so the auto-default-identity
	// matches what add-imap wrote and won't pollute account_identities
	// with a URL if the user has cleared identities. confirmDefaultIdentity
	// silently no-ops when the identifier arg is empty, so a legacy IMAP
	// row with NULL display_name skips the write rather than re-injecting
	// the URL.
	displayName := src.DisplayName.String
	confirmDefaultIdentity(io.Discard, s, src.ID, displayName, displayName, "account-identifier")
	if err := runPostSourceCreateMigrations(s); err != nil {
		return nil, fmt.Errorf("post-source-create migrations: %w", err)
	}

	summary, err := syncer.Full(ctx, src.Identifier)
	if err != nil {
		return nil, fmt.Errorf("IMAP sync failed: %w", err)
	}
	saveIMAPFolderStates(s, src, apiClient, summary, 0)
	return summary, nil
}

// runScheduledTeamsSync runs a Teams sync for the daemon.
func runScheduledTeamsSync(ctx context.Context, src *store.Source, s *store.Store) error {
	email := src.Identifier

	// Seed the default identity and converge legacy migrations before
	// syncing, for parity with the Gmail/IMAP daemon paths. add-teams
	// already does both, so this is a no-op in the normal flow, but it
	// ensures a Teams source created by another path still gets its
	// "me" identity. Auto-default-identity must run BEFORE the legacy
	// migration retry (see account_identity.go); serve is a daemon, so
	// the confirmation message has no terminal and is discarded.
	confirmDefaultIdentity(io.Discard, s, src.ID, email, email, "account-identifier")
	if err := runPostSourceCreateMigrations(s); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}

	mgr := microsoft.NewGraphManager(cfg.Microsoft.ClientID, cfg.Microsoft.EffectiveTenantID(), cfg.TokensDir(), logger)
	tokenFn, err := mgr.TokenSource(ctx, email)
	if err != nil {
		return err
	}
	qps := float64(cfg.Sync.RateLimitQPS)
	if qps <= 0 {
		qps = 5
	}
	client := teams.NewClient("https://graph.microsoft.com/v1.0", teams.TokenFunc(tokenFn), qps)
	opts := teams.ImportOptions{
		Email:           email,
		AttachmentsDir:  cfg.AttachmentsDir(),
		IncludeChannels: true,
	}
	_, err = teams.NewImporter(s, client).Import(ctx, opts)
	return err
}
