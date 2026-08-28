package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/circleback"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/granola"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/syncerr"
	"go.kenn.io/msgvault/internal/synctechsms"
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

// buildCacheSubprocessForRun is the daemon's staleness-derived build entry
// point (a test seam). auto=true re-evaluates the rebuild decision under the
// cache build lock, so a build that waited on another process's build does
// not redo (or erase) its work.
var buildCacheSubprocessForRun = func(ctx context.Context, fullRebuild bool) error {
	return buildCacheSubprocess(ctx, fullRebuild, true)
}

var buildStartupCacheSubprocessForRun = func(
	ctx context.Context,
	intent startupCacheBuildIntent,
) error {
	mode := buildCacheModeAuto
	if intent == startupCacheBuildIntentFull {
		mode = buildCacheModeFull
	}
	return buildCacheSubprocessMode(ctx, mode)
}

var executeBuildCacheSubprocessMode = buildCacheSubprocessMode

var runDerivedCacheSubprocess = func(ctx context.Context, analyticsDir string) error {
	err := executeBuildCacheSubprocessMode(ctx, buildCacheModeDerived)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrDerivedRefreshRequiresFullBuild) ||
		strings.Contains(err.Error(), ErrDerivedRefreshRequiresFullBuild.Error()) {
		// Escalate only to repair an existing cache. A derived refresh must
		// never create a cache that configuration (engine="sql",
		// auto_build_cache=false, PostgreSQL) deliberately leaves absent;
		// the caller reports the cache stale instead.
		readiness, inspectErr := query.InspectCacheReadiness(analyticsDir)
		if inspectErr != nil || readiness == query.CacheAbsent {
			return err
		}
		return executeBuildCacheSubprocessMode(ctx, buildCacheModeFull)
	}
	return err
}

var (
	importDiscordSourceForScheduledRun  = importDiscordSource
	rebuildCacheAfterScheduledSourceRun = rebuildCacheAfterScheduledSync
	startServeAPIServer                 = func(server *api.Server, listener net.Listener) error {
		return server.StartOnListener(listener)
	}
)

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
	rootCmd.AddCommand(daemonCmd)
	addServeLifecycleCommands(serveCmd)
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
	heartbeatCtx, stopHeartbeat := context.WithCancel(cmd.Context())
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		runtimeRecordHeartbeat(heartbeatCtx, ownership, daemonRuntimeHeartbeatInterval)
	}()
	defer func() {
		stopHeartbeat()
		<-heartbeatDone
		if err := ownership.Close(); err != nil {
			logger.Warn("release daemon ownership failed", "error", err)
		}
	}()
	setStartupPhase := func(phase string) {
		if err := ownership.SetStartupPhase(phase); err != nil {
			logger.Warn("update daemon startup phase failed", "error", err)
		}
	}

	// Open database
	dbPath := cfg.DatabaseDSN()
	setStartupPhase("opening archive database")
	logger.Info("daemon startup step", "step", "open_archive_database", "database", daemonStartupDatabaseLabel(dbPath))
	s, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	var analyticsInit *daemonAnalyticsInitHandle
	var vectorInit *vectorInitHandle
	resourceCleanupSafe := true
	defer func() {
		if !resourceCleanupSafe {
			logger.Warn("archive database cleanup skipped", "error", "HTTP shutdown did not complete")
			return
		}
		if err := closeDaemonStoreAfterInitializers(s, analyticsInit, vectorInit); err != nil {
			logger.Warn("archive database cleanup skipped", "error", err)
		}
	}()
	logger.Info("daemon startup step complete", "step", "open_archive_database")

	setStartupPhase("migrating archive schema")
	logger.Info("daemon startup step", "step", "init_archive_schema")
	// Under the signal-cancelled root context, not a background one: on an
	// existing archive this step runs a one-time full-table backfill that can
	// take hours, and it runs before the port is bound. With a background
	// context SIGINT and SIGTERM reach the process and nothing happens — the
	// backfill and its open transaction carry on, and the operator's only
	// remaining move is SIGKILL on a writing process. Cancelling here stops it
	// at the next batch boundary, keeps every batch already committed, and
	// leaves the migration unmarked so the next start resumes.
	if err := s.InitSchemaContext(cmd.Context()); err != nil {
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
	attachmentMaint, err := newAttachmentMaintenance(
		s, cfg.AttachmentsDir(), logger, !cfg.Data.LooseAttachments,
	)
	if err != nil {
		return fmt.Errorf("open attachment maintenance: %w", err)
	}
	defer func() {
		if resourceCleanupSafe {
			_ = attachmentMaint.close()
		}
	}()
	blobStore := attachmentMaint.blob

	// Vector misconfiguration still fails startup fast; the expensive
	// backend open/migrate/backfill runs in the background after the API
	// server is listening (startVectorInit below), so the TUI and other
	// clients are not blocked by vector maintenance.
	if err := precheckVectorFeatures(dbPath); err != nil {
		return fmt.Errorf("vector features: %w", err)
	}
	if !cfg.Vector.AnyLaneEnabled() {
		logger.Info("daemon startup step", "step", "skip_vector_backend", "enabled", false)
	}

	setStartupPhase("building analytics cache")
	logger.Info("daemon startup step", "step", "init_analytics_engine")
	startupCacheIntent, err := startupCacheBuildIntentFromEnv()
	if err != nil {
		return err
	}
	engine, analyticsMode, cacheOutcome, analyticsAsync, engineErr := prepareDaemonAnalyticsEngine(
		ctx, cfg, s, startupCacheIntent,
	)
	if startupCacheIntent != startupCacheBuildIntentNone && !analyticsAsync {
		if outcomeErr := ownership.SetStartupCacheBuildOutcome(cacheOutcome); outcomeErr != nil {
			if engine != nil {
				_ = engine.Close()
			}
			return outcomeErr
		}
	}
	if engineErr != nil {
		if engine != nil {
			_ = engine.Close()
		}
		return engineErr
	}
	initialAnalyticsEngine := engine
	analyticsServerStarted := false
	defer func() {
		if resourceCleanupSafe && !analyticsServerStarted && initialAnalyticsEngine != nil {
			_ = initialAnalyticsEngine.Close()
		}
	}()
	if !analyticsAsync {
		logger.Info("daemon startup step complete", "step", "init_analytics_engine")
	}

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
	cardDAVController, err := api.NewCardDAVController(cfg, s)
	if err != nil {
		return fmt.Errorf("configure CardDAV: %w", err)
	}
	cardDAVController.SetScheduleReconciler(func(cardDAVConfig config.CardDAVConfig, service api.CardDAVOperations) error {
		return reconcileCardDAVSchedulerJob(sched, cardDAVConfig, service, logger)
	})
	if err := reconcileCardDAVSchedulerJob(sched, cfg.CardDAV, cardDAVController.Current(), logger); err != nil {
		return err
	}

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
		// The job name must match the store identity (OwnerPhone) that
		// synctechsms.Importer uses for GetOrCreateSource, so
		// api.SchedulerJobNameForSource can find this job from a source
		// row. See internal/api/scheduler_jobs.go.
		jobName, ok := api.SchedulerJobNameForSource(synctechsms.SourceType, source.OwnerPhone)
		if !ok {
			logger.Error("no scheduler job mapping for synctech-sms source", "source", source.Name)
			continue
		}
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
		// The job name is keyed on the normalized account email so it
		// matches every per-calendar store source's identity
		// ("<accountEmail>/<calendarID>", see internal/calsync/calsync.go
		// sourceIdentifier) via api.SchedulerJobNameForSource, even when
		// the config [[gcal]] entry's Name differs from its Email.
		jobName := api.GCalJobNameForAccountEmail(normalizeCalendarAccountEmail(source.Email))
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
	if err := configureDocumentReconcileJob(
		ctx, sched, s, cfg.Attachments.Documents.Enabled,
	); err != nil {
		return fmt.Errorf("configure document reconciliation: %w", err)
	}
	if err := registerActivityProjectionJob(
		sched, s, cfg.Activity, logger); err != nil {
		return fmt.Errorf("schedule activity projection: %w", err)
	}
	if cfg.People.Sweep.Enabled {
		if err := cfg.People.Sweep.Validate(); err != nil {
			return fmt.Errorf("validate people sweep: %w", err)
		}
	}
	if err := addPeopleSweepJob(
		sched, cfg.People.Sweep, newPeopleSweepScheduledRun(cfg.People.Sweep, s),
	); err != nil {
		return fmt.Errorf("schedule people sweep: %w", err)
	}
	if err := registerPersonEnrichmentJob(
		ctx, sched, s, cfg.People.Enrichment); err != nil {
		return fmt.Errorf("schedule person enrichment: %w", err)
	}

	if cfg.Beeper.Enabled && cfg.Beeper.Schedule == "" {
		logger.Warn("beeper is enabled but has no schedule — the daemon will not sync it; its freshness will eventually go stale",
			"hint", `set a cron schedule (e.g. "*/30 * * * *") on the [beeper] entry`)
	}
	if cfg.Beeper.Enabled && cfg.Beeper.Schedule != "" {
		if err := registerScheduledBeeperJob(sched, cfg.Beeper.Schedule, attachmentMaint, func(ctx context.Context) error {
			return runConfiguredBeeperSync(ctx, s)
		}); err != nil {
			logger.Error("failed to schedule beeper sync", "error", err)
		} else {
			logger.Info("scheduled beeper sync", "schedule", cfg.Beeper.Schedule)
		}
	}

	if cfg.Slack.Enabled && cfg.Slack.Schedule == "" {
		logger.Warn("slack is enabled but has no schedule — the daemon will not sync it; its freshness will eventually go stale",
			"hint", `set a cron schedule (e.g. "*/30 * * * *") on the [slack] entry`)
	}
	if cfg.Slack.Enabled && cfg.Slack.Schedule != "" {
		if err := sched.AddJob(scheduler.Job{
			Name:     api.SlackJobName,
			Schedule: cfg.Slack.Schedule,
			Run: func(ctx context.Context) error {
				return runScheduledSource(ctx, attachmentMaint, true, func(ctx context.Context) error {
					return runConfiguredSlackSync(ctx, s)
				})
			},
		}); err != nil {
			logger.Error("failed to schedule slack sync", "error", err)
		} else {
			logger.Info("scheduled slack sync", "schedule", cfg.Slack.Schedule)
		}
	}

	// Meeting sources (Granola/Circleback) mirror the gcal treatment: warn
	// when enabled but unscheduled, then register the scheduled ones.
	for _, src := range cfg.Granola {
		if src.Enabled && src.Schedule == "" {
			logger.Warn("granola source is enabled but has no schedule — the daemon will not sync it; its freshness will eventually go stale",
				"source", src.Identifier,
				"hint", `set a cron schedule (e.g. "0 */6 * * *") on the [[granola]] entry`)
		}
	}
	for _, src := range cfg.ScheduledGranolaSources() {
		source := src
		// Already matches the store identity (config Identifier ==
		// GetOrCreateSource identifier); routed through the shared helper
		// so registration and api.sourceStatus can never drift.
		jobName, ok := api.SchedulerJobNameForSource(granola.SourceType, source.Identifier)
		if !ok {
			logger.Error("no scheduler job mapping for granola source", "source", source.Identifier)
			continue
		}
		if err := sched.AddJob(scheduler.Job{
			Name:     jobName,
			Schedule: source.Schedule,
			Run: func(ctx context.Context) error {
				return runConfiguredGranolaSync(ctx, s, source)
			},
		}); err != nil {
			logger.Error("failed to schedule granola source", "source", source.Identifier, "error", err)
		} else {
			logger.Info("scheduled granola source", "source", source.Identifier, "schedule", source.Schedule)
		}
	}
	for _, src := range cfg.Circleback {
		if src.Enabled && src.Schedule == "" {
			logger.Warn("circleback source is enabled but has no schedule — the daemon will not sync it; its freshness will eventually go stale",
				"source", src.Identifier,
				"hint", `set a cron schedule (e.g. "30 */6 * * *") on the [[circleback]] entry`)
		}
	}
	for _, src := range cfg.ScheduledCirclebackSources() {
		source := src
		// Already matches the store identity (config Identifier ==
		// GetOrCreateSource identifier); routed through the shared helper
		// so registration and api.sourceStatus can never drift.
		jobName, ok := api.SchedulerJobNameForSource(circleback.SourceType, source.Identifier)
		if !ok {
			logger.Error("no scheduler job mapping for circleback source", "source", source.Identifier)
			continue
		}
		if err := sched.AddJob(scheduler.Job{
			Name:     jobName,
			Schedule: source.Schedule,
			Run: func(ctx context.Context) error {
				return runConfiguredCirclebackSync(ctx, s, source)
			},
		}); err != nil {
			logger.Error("failed to schedule circleback source", "source", source.Identifier, "error", err)
		} else {
			logger.Info("scheduled circleback source", "source", source.Identifier, "schedule", source.Schedule)
		}
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start the scheduler
	sched.Start()

	// Create adapters for the API interfaces
	meetingImporter := meetingimport.NewImporter(s, meetingimport.Hooks{
		AfterSourceSetup: func() error {
			return runPostSourceCreateMigrations(s)
		},
		RefreshCache: func(_ context.Context, label string) error {
			// The import is already durable. Keep the refresh independent of the
			// client, but let daemon shutdown stop it before the store closes.
			return daemonCacheRefreshError(ctx, rebuildCacheAfterScheduledSync(ctx, label))
		},
	}).WithLogger(logger)
	storeAdapter := &storeAPIAdapter{
		store:                  s,
		attachmentMaintenance:  attachmentMaint,
		meetingImporter:        meetingImporter,
		analyticsDir:           cfg.AnalyticsDir(),
		personEnrichmentConfig: cfg.People.Enrichment,
		lookupEnv:              os.LookupEnv,
	}
	schedAdapter := &schedulerAdapter{scheduler: sched}

	// Create and start API server
	var apiServer *api.Server
	apiOpts := api.ServerOptions{
		Config:         cfg,
		Store:          storeAdapter,
		SavedViewStore: s,
		Engine:         engine,
		SQLQueryRunner: func(ctx context.Context, sql string) (*query.QueryResult, error) {
			if apiServer == nil {
				return nil, errors.New("daemon API server unavailable")
			}
			return runDaemonSQLQuery(ctx, cfg, s, apiServer.QueryEngineForRequest(ctx), sql)
		},
		ShutdownToken:                 ownership.shutdownToken,
		ShutdownFunc:                  cancel,
		Scheduler:                     schedAdapter,
		CardDAV:                       cardDAVController,
		Logger:                        logger,
		DaemonVersion:                 Version,
		AnalyticsMode:                 analyticsMode,
		AnalyticsInitializationActive: analyticsAsync,
		IdleTracker:                   idleTracker,
		OperationGate:                 operationGate,
		BlobStore:                     blobStore,
	}
	applyServerRuntimeConfig(&apiOpts, cfg)
	if cfg.Vector.AnyLaneEnabled() {
		apiOpts.VectorStatus = api.VectorStatusInitializing
	}
	apiServer = api.NewServerWithOptions(apiOpts)

	// Start API server in goroutine
	apiAddr := apiListener.Addr().String()
	setStartupPhase("")
	logger.Info("daemon startup step", "step", "start_api_server", "bind", apiAddr)
	serverErr := make(chan error, 1)
	go func() {
		if err := startServeAPIServer(apiServer, apiListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()
	var serverStartupErr error
	startErr := apiServer.WaitStarted(ctx)
	if startErr != nil {
		if ctx.Err() == nil {
			serverStartupErr = startErr
		}
		// A cancelled context may win the readiness wait before the listener
		// goroutine gets scheduled. Wait for its startup barrier to settle before
		// shutdown or deferred cleanup can release resources used by startup.
		if barrierErr := apiServer.WaitStarted(context.Background()); barrierErr != nil && serverStartupErr == nil {
			serverStartupErr = barrierErr
		} else if barrierErr == nil {
			listenerReserved = false
			resourceCleanupSafe = false
		}
		cancel()
	} else {
		listenerReserved = false
		analyticsServerStarted = true
		resourceCleanupSafe = false
		if analyticsAsync {
			analyticsInit = startDaemonAnalyticsInitializer(
				ctx, cfg, s, startupCacheIntent, apiServer, ownership,
				combineWorkTrackers(
					idleTracker,
					labelWorkTracker(operationGate, "analytics cache initialization"),
				),
			)
		} else {
			analyticsInit = completedDaemonAnalyticsInitHandle()
		}
		// Analytics must acquire the serial mutation gate before vector
		// initialization can compete for it. This keeps an explicit cache
		// request from waiting behind a long vector migration or backfill.
		_ = analyticsInit.WaitStarted(ctx)
		if idleTracker != nil {
			idleTracker.Touch()
			go idleTracker.Run(ctx)
			logger.Info("background daemon idle shutdown enabled", "timeout", cfg.Server.DaemonIdleTimeout)
		}

		vectorInit = startVectorInit(
			ctx, s, dbPath,
			combineWorkTrackers(idleTracker, labelWorkTracker(operationGate, "background embedding work")),
			apiServer, sched, blobStore,
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
		select {
		case sig := <-sigChan:
			logger.Info("received shutdown signal", "signal", sig)
			fmt.Printf("\nReceived %s, shutting down...\n", sig)
		case err := <-serverErr:
			logger.Error("API server error", "error", err)
			fmt.Printf("\nAPI server error: %v\n", err)
			serverStartupErr = err
		case err := <-analyticsInit.Fatal():
			if ctx.Err() == nil {
				logger.Error("analytics initialization error", "error", err)
				fmt.Printf("\nAnalytics initialization error: %v\n", err)
				serverStartupErr = err
				cancel()
			}
		case <-ctx.Done():
			logger.Info("context cancelled")
		}
	}

	// Stop background work first: vector init honors ctx, so cancelling
	// lets the operation-gate drain inside shutdownServeRuntime complete.
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), serveOperationDrainTimeout)
	defer shutdownCancel()
	shutdownErr := shutdownServeRuntime(shutdownCtx, cmd.OutOrStdout(), apiServer, sched, operationGate)
	if shutdownErr == nil {
		resourceCleanupSafe = true
	}
	// Wait for the background vector init regardless of the shutdown
	// outcome: the deferred s.Close() must not run under a still-running
	// init goroutine, and vectors.db needs closing whenever init finished.
	// Bound the wait by the time REMAINING on shutdownCtx rather than a
	// fresh full drain window — shutdownServeRuntime already consumed part
	// of it, and `daemon stop` budgets only one drain window before it kills
	// the daemon (serveStopGraceTimeout).
	if vectorInit != nil {
		if vectorInit.WaitContext(shutdownCtx) {
			if resourceCleanupSafe {
				vectorInit.CloseFeatures()
			}
		} else {
			logger.Warn("vector init did not stop within the shutdown drain timeout; skipping vectors.db close")
		}
	}
	if analyticsInit != nil {
		analyticsReady := analyticsInit.WaitContext(shutdownCtx)
		if analyticsReady && resourceCleanupSafe {
			if err := closeDaemonAnalyticsEngines(apiServer, initialAnalyticsEngine, analyticsInit); err != nil {
				shutdownErr = errors.Join(shutdownErr, err)
			}
		} else if !analyticsReady {
			logger.Warn("analytics initialization did not stop within the shutdown drain timeout; skipping analytics engine close")
		}
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

func reconcileCardDAVSchedulerJob(sched *scheduler.Scheduler, cardDAVConfig config.CardDAVConfig, service api.CardDAVOperations, logger *slog.Logger) error {
	if !cardDAVConfig.Enabled || cardDAVConfig.Schedule == "" {
		sched.RemoveJob(api.CardDAVJobName)
		if cardDAVConfig.Enabled && cardDAVConfig.Schedule == "" {
			logger.Warn("carddav is enabled but has no schedule — the daemon will not sync it",
				"hint", `set a cron schedule (e.g. "0 */6 * * *") in [carddav]`)
		}
		return nil
	}
	if service == nil {
		sched.RemoveJob(api.CardDAVJobName)
		logger.Warn("carddav credentials are unavailable or do not match saved discovery; skipping scheduled sync",
			"hint", "save the CardDAV account with its password to repair the connection")
		return nil
	}
	if err := sched.AddJob(scheduler.Job{
		Name: api.CardDAVJobName, Schedule: cardDAVConfig.Schedule,
		Run: func(ctx context.Context) error {
			_, err := service.Sync(ctx, carddav.SyncOptions{})
			return err
		},
	}); err != nil {
		return fmt.Errorf("schedule CardDAV sync: %w", err)
	}
	return nil
}

func daemonCacheRefreshError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func applyServerRuntimeConfig(options *api.ServerOptions, cfg *config.Config) {
	options.VectorCfg = cfg.Vector
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
	if engine == nil {
		return nil, api.ErrSQLQueryEngineUnavailable
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
	if !store.IsPostgresURL(dbPath) && !staleness.NeedsBuild {
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
	intent startupCacheBuildIntent,
) (query.Engine, string, startupCacheBuildOutcome, error) {
	if c == nil || s == nil {
		return nil, "", startupCacheBuildOutcomeNone,
			errors.New("daemon analytics engine unavailable")
	}
	if s.IsPostgreSQL() {
		outcome := startupCacheBuildOutcomeNone
		if intent != startupCacheBuildIntentNone {
			outcome = startupCacheBuildOutcomeUnconsumed
		}
		return query.NewEngine(s.DB(), true), api.AnalyticsModePostgres, outcome, nil
	}

	engineMode := c.Analytics.Engine
	if engineMode == "" {
		engineMode = config.AnalyticsEngineAuto
	}
	if engineMode == config.AnalyticsEngineSQL {
		logger.Info("using live SQL analytics engine",
			"engine", engineMode)
		outcome := startupCacheBuildOutcomeNone
		if intent != startupCacheBuildIntentNone {
			outcome = startupCacheBuildOutcomeUnconsumed
		}
		return query.NewEngine(s.DB(), false), api.AnalyticsModeSQL, outcome, nil
	}

	dbPath := c.DatabaseDSN()
	analyticsDir := c.AnalyticsDir()
	staleness := cacheNeedsBuild(dbPath, analyticsDir)
	outcome := startupCacheBuildOutcomeNone
	shouldBuild := intent != startupCacheBuildIntentNone ||
		(staleness.NeedsBuild && c.Analytics.AutoBuildCache)
	if shouldBuild {
		// Build the cache before serving rather than starting on live-SQL
		// fallback: incremental rebuilds take seconds, and startup progress
		// already streams to the CLI that auto-started the daemon. A failed
		// build is fatal for engine="duckdb" (documented to never fall back)
		// and falls back to live SQL for engine="auto".
		fullBuild := staleness.FullRebuild
		reason := staleness.Reason
		if intent == startupCacheBuildIntentFull {
			fullBuild = true
			reason = "explicit full rebuild requested"
		}
		logger.Info("daemon startup step",
			"step", "build_analytics_cache",
			"reason", reason,
			"full_rebuild", fullBuild)

		var buildErr error
		if intent != startupCacheBuildIntentNone {
			buildErr = buildStartupCacheSubprocessForRun(ctx, intent)
		} else {
			buildErr = buildCacheSubprocessForRun(ctx, staleness.FullRebuild)
		}
		if buildErr != nil {
			if intent != startupCacheBuildIntentNone {
				outcome = startupCacheBuildOutcomeFailed
			}
			logger.Warn("daemon startup step failed",
				"step", "build_analytics_cache",
				"reason", reason,
				"full_rebuild", fullBuild,
				"error", buildErr)
			if engineMode == config.AnalyticsEngineDuckDB {
				if intent != startupCacheBuildIntentNone {
					outcome = startupCacheBuildOutcomeFatal
				}
				return nil, "", outcome, fmt.Errorf("build analytics cache: %w", buildErr)
			}
			if intent != startupCacheBuildIntentNone {
				return query.NewEngine(s.DB(), false), api.AnalyticsModeSQLFallback, outcome, nil
			}
		} else {
			logger.Info("daemon startup step complete",
				"step", "build_analytics_cache",
				"reason", reason,
				"full_rebuild", fullBuild)
		}
		staleness = cacheNeedsBuild(dbPath, analyticsDir)
	}

	if !staleness.NeedsBuild {
		duckEngine, err := openDaemonDuckDBEngineForRun(c, s)
		if err != nil {
			if intent != startupCacheBuildIntentNone {
				outcome = startupCacheBuildOutcomeFailed
			}
			if engineMode == config.AnalyticsEngineDuckDB {
				if intent != startupCacheBuildIntentNone {
					outcome = startupCacheBuildOutcomeFatal
				}
				return nil, "", outcome, err
			}
			logger.Warn("DuckDB engine failed, falling back to live SQL",
				"error", err)
			return query.NewEngine(s.DB(), false), api.AnalyticsModeSQLFallback, outcome, nil
		}
		if intent != startupCacheBuildIntentNone {
			outcome = startupCacheBuildOutcomeFulfilled
		}
		if !c.Analytics.AutoBuildCache {
			logger.Warn(
				`automatic analytics cache refresh disabled; DuckDB analytics can become stale; engine = "sql" selects live aggregate data`,
				"auto_build_cache", false,
				"engine", engineMode,
			)
		}
		return duckEngine, api.AnalyticsModeDuckDB, outcome, nil
	}

	if intent != startupCacheBuildIntentNone {
		outcome = startupCacheBuildOutcomeFailed
	}
	if engineMode == config.AnalyticsEngineDuckDB {
		if intent != startupCacheBuildIntentNone {
			outcome = startupCacheBuildOutcomeFatal
		}
		reason := staleness.Reason
		if reason == "" {
			reason = "analytics cache is missing or incomplete"
		}
		return nil, "", outcome,
			fmt.Errorf("analytics engine=duckdb requires a usable cache: %s", reason)
	}
	if staleness.Reason != "" {
		logger.Info("analytics cache not usable, using live SQL engine",
			"reason", staleness.Reason,
			"auto_build_cache", c.Analytics.AutoBuildCache)
	} else {
		logger.Info("analytics cache not built - using live SQL engine (run 'msgvault build-cache' for faster aggregates)",
			"auto_build_cache", c.Analytics.AutoBuildCache)
	}
	return query.NewEngine(s.DB(), false), api.AnalyticsModeSQLFallback, outcome, nil
}

var openDaemonDuckDBEngineForRun = openDaemonDuckDBEngine

func openDaemonDuckDBEngine(c *config.Config, s *store.Store) (*query.DuckDBEngine, error) {
	if c == nil || s == nil {
		return nil, errors.New("daemon DuckDB engine unavailable")
	}
	spillParent, err := query.PrepareDaemonSpillDir(c.HomeDir)
	if err != nil {
		return nil, err
	}
	// Each engine spills into its own subdirectory: the daemon opens both a
	// long-lived engine and short-lived per-query engines (runDaemonSQLQuery),
	// and OwnTempDirectory deletes the directory on Close — sharing one
	// directory would let a temporary engine remove the live engine's spill
	// files. The pid-owned parent is reaped by PrepareDaemonSpillDir once
	// this process exits.
	tempDirectory, err := os.MkdirTemp(spillParent, "engine-")
	if err != nil {
		return nil, fmt.Errorf("create engine spill directory: %w", err)
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
		query.DuckDBOptions{
			DisableSQLiteScanner: true,
			TempDirectory:        tempDirectory,
			OwnTempDirectory:     true,
		},
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
	meetingImporter       *meetingimport.Importer
	// analyticsDir is the daemon's Parquet analytics cache directory, used
	// to read the revision committed by the derived-refresh child.
	analyticsDir           string
	personEnrichmentConfig personenrichment.Config
	lookupEnv              personenrichment.CredentialLookup
}

var _ api.MessageStore = (*storeAPIAdapter)(nil)
var _ api.CtxMessageStore = (*storeAPIAdapter)(nil)
var _ api.MessageIdentityStore = (*storeAPIAdapter)(nil)
var _ api.PersonFactStore = (*storeAPIAdapter)(nil)
var _ api.MeetingImporter = (*storeAPIAdapter)(nil)
var _ api.SourceStatusStore = (*storeAPIAdapter)(nil)
var _ api.CLIStore = (*storeAPIAdapter)(nil)
var _ api.ContextCLIStore = (*storeAPIAdapter)(nil)
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
var _ api.ContextCLIDedupDeleteStore = (*storeAPIAdapter)(nil)
var _ api.IdentityLinkStore = (*storeAPIAdapter)(nil)
var _ api.IdentityMatchStore = (*storeAPIAdapter)(nil)
var _ api.PersonProfileStore = (*storeAPIAdapter)(nil)
var _ api.PersonCompletionStore = (*storeAPIAdapter)(nil)
var _ api.PersonTrackingStore = (*storeAPIAdapter)(nil)
var _ api.PersonProfileValueStore = (*storeAPIAdapter)(nil)
var _ api.CommunicationServiceStore = (*storeAPIAdapter)(nil)
var _ api.AttributeDefinitionStore = (*storeAPIAdapter)(nil)
var _ api.PersonAttributeStore = (*storeAPIAdapter)(nil)
var _ api.PersonRelationshipStore = (*storeAPIAdapter)(nil)
var _ api.OrganizationStore = (*storeAPIAdapter)(nil)
var _ api.EmploymentStore = (*storeAPIAdapter)(nil)
var _ api.IdentityCacheRefresher = (*storeAPIAdapter)(nil)
var _ api.ClusterLookupStore = (*storeAPIAdapter)(nil)
var _ api.ConversationWindowStore = (*storeAPIAdapter)(nil)
var _ api.ChangedMessageLister = (*storeAPIAdapter)(nil)
var _ api.ArchiveIdentifier = (*storeAPIAdapter)(nil)
var _ api.DocumentSearchStore = (*storeAPIAdapter)(nil)
var _ api.DocumentStatusStore = (*storeAPIAdapter)(nil)
var _ api.DocumentVectorStatusStore = (*storeAPIAdapter)(nil)
var _ api.ActivityStore = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) ContactStateContext(
	ctx context.Context, personID int64, now time.Time,
) (store.ContactState, error) {
	return a.store.ContactStateContext(ctx, personID, now)
}

func (a *storeAPIAdapter) PersonDaysContext(
	ctx context.Context, request store.PersonDaysRequest,
) (*store.PersonDaysPage, error) {
	return a.store.PersonDaysContext(ctx, request)
}

func (a *storeAPIAdapter) PersonDayContext(
	ctx context.Context, request store.PersonDayRequest,
) (*store.PersonDayPage, error) {
	return a.store.PersonDayContext(ctx, request)
}

func (a *storeAPIAdapter) DayContext(
	ctx context.Context, request store.DayRequest,
) (*store.DayPage, error) {
	return a.store.DayContext(ctx, request)
}

func (a *storeAPIAdapter) ListDailyNoteEntriesContext(
	ctx context.Context, localDate string, limit, offset int,
) ([]store.DailyNoteEntry, error) {
	return a.store.ListDailyNoteEntriesContext(ctx, localDate, limit, offset)
}

func (a *storeAPIAdapter) CreateDailyNoteEntryContext(
	ctx context.Context, input store.DailyNoteEntryInput,
) (*store.DailyNoteEntry, error) {
	return a.store.CreateDailyNoteEntryContext(ctx, input)
}

func (a *storeAPIAdapter) DeleteDailyNoteEntryContext(ctx context.Context, id int64) error {
	return a.store.DeleteDailyNoteEntryContext(ctx, id)
}

func (a *storeAPIAdapter) ConversationExistsContext(ctx context.Context, conversationID int64) (bool, error) {
	return a.store.ConversationExistsContext(ctx, conversationID)
}

func (a *storeAPIAdapter) GetConversationWindowContext(
	ctx context.Context,
	conversationID, anchorID int64,
	before, after int,
	start, end *time.Time,
) (*store.ConversationWindow, error) {
	return a.store.GetConversationWindowContext(ctx, conversationID, anchorID, before, after, start, end)
}

// ListChangedMessages exposes the content-change feed to the API server. The
// daemon passes this adapter -- not *store.Store -- as ServerOptions.Store, so
// without this method the route's optional-interface check fails and the
// endpoint reports itself unavailable on every production request.
func (a *storeAPIAdapter) ListChangedMessages(
	ctx context.Context, since store.ChangedMessagesCursor, limit int,
) (store.ChangedMessagePage, error) {
	return a.store.ListChangedMessages(ctx, since, limit)
}

// ArchiveUIDContext exposes the archive's durable identity to the API server, which
// binds every change-feed cursor to it so a cursor cannot be resumed against a
// different archive. Same reason as ListChangedMessages above: the daemon
// passes this adapter, not *store.Store, so without this method the route
// reports itself unavailable on every production request.
func (a *storeAPIAdapter) ArchiveUIDContext(ctx context.Context) (string, error) {
	return a.store.ArchiveUIDContext(ctx)
}

func (a *storeAPIAdapter) SearchDocuments(
	ctx context.Context,
	request store.DocumentSearchRequest,
) (store.DocumentSearchResponse, error) {
	if err := reconcileDocumentOccurrencesForSearch(ctx, a.store); err != nil {
		return store.DocumentSearchResponse{}, err
	}
	return a.store.SearchDocuments(ctx, request)
}

func (a *storeAPIAdapter) ReconcileDocumentOccurrences(ctx context.Context) error {
	return reconcileDocumentOccurrencesForSearch(ctx, a.store)
}

func (a *storeAPIAdapter) GetDocumentIndexStatusForScope(
	ctx context.Context,
	profileID string,
	extractionInputKey string,
	allowedMediaTypes []string,
	allowedMessageTypes []string,
) (store.DocumentIndexStatus, error) {
	return a.store.GetDocumentIndexStatusForScope(
		ctx, profileID, extractionInputKey, allowedMediaTypes, allowedMessageTypes,
	)
}

func (a *storeAPIAdapter) GetActiveDocumentExtractionRebuild(
	ctx context.Context,
	profileID string,
	extractionInputKey string,
) (store.DocumentExtractionRebuild, error) {
	return a.store.GetActiveDocumentExtractionRebuild(ctx, profileID, extractionInputKey)
}

func (a *storeAPIAdapter) CountIncompleteDocumentExtractionRebuild(
	ctx context.Context,
	rebuild store.DocumentExtractionRebuild,
	allowedMediaTypes []string,
	allowedMessageTypes []string,
) (int64, error) {
	return a.store.CountIncompleteDocumentExtractionRebuild(
		ctx, rebuild, allowedMediaTypes, allowedMessageTypes,
	)
}

func (a *storeAPIAdapter) GetDocumentVectorTargetProfileID(ctx context.Context) (string, error) {
	return a.store.GetDocumentVectorTargetProfileID(ctx)
}

func (a *storeAPIAdapter) GetDocumentVectorOperationsStatus(
	ctx context.Context,
	configured store.DocumentVectorGenerationSpec,
	documentEgressFingerprint, queryEgressFingerprint string,
	generationID int64,
	afterToken string,
	limit int,
) (store.DocumentVectorOperationsStatus, error) {
	return a.store.GetDocumentVectorOperationsStatus(
		ctx, configured, documentEgressFingerprint, queryEgressFingerprint,
		generationID, afterToken, limit,
	)
}

func (a *storeAPIAdapter) GetStats() (*api.StoreStats, error) {
	return a.store.GetStats()
}

func (a *storeAPIAdapter) ImportMeeting(
	ctx context.Context,
	req meetingimport.Request,
) (meetingimport.Result, error) {
	if a == nil || a.meetingImporter == nil {
		return meetingimport.Result{}, meetingimport.ErrUnavailable
	}
	return a.meetingImporter.Import(ctx, req)
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

func (a *storeAPIAdapter) GetFileMetadata(ctx context.Context, id int64) (*store.FileMetadata, error) {
	return a.store.GetFileMetadata(ctx, id)
}

func (a *storeAPIAdapter) GetFileMetadataBatch(ctx context.Context, ids []int64) (map[int64]store.FileMetadata, error) {
	return a.store.GetFileMetadataBatch(ctx, ids)
}

func (a *storeAPIAdapter) GetArchivedMessageRaw(ctx context.Context, id int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return a.store.GetMessageRaw(id)
}

func (a *storeAPIAdapter) GetStatsForScope(sourceIDs []int64) (*store.Stats, error) {
	return a.store.GetStatsForScope(sourceIDs)
}

func (a *storeAPIAdapter) GetStatsForScopeContext(
	ctx context.Context,
	sourceIDs []int64,
) (*store.Stats, error) {
	return a.store.GetStatsForScopeContext(ctx, sourceIDs)
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

func (a *storeAPIAdapter) NeedsFTSBackfillQuickContext(ctx context.Context) bool {
	return a.store.NeedsFTSBackfillQuickContext(ctx)
}

func (a *storeAPIAdapter) BackfillFTS(progress func(done, total int64)) (int64, error) {
	return a.store.BackfillFTS(progress)
}

func (a *storeAPIAdapter) RebuildFTS(progress func(done, total int64)) (int64, error) {
	return a.store.RebuildFTS(progress)
}

func (a *storeAPIAdapter) RebuildFTSContext(
	ctx context.Context,
	progress func(done, total int64),
) (int64, error) {
	return a.store.RebuildFTSContext(ctx, progress)
}

func (a *storeAPIAdapter) RunStartupMigrationsContext(
	ctx context.Context,
	legacyIdentityAddresses []string,
) (store.StartupMigrationResult, error) {
	return a.store.RunStartupMigrationsContext(ctx, legacyIdentityAddresses)
}

func (a *storeAPIAdapter) BuildCLICache(
	ctx context.Context,
	fullRebuild bool,
	emit func(api.CLICacheBuildEvent) error,
) error {
	return buildCacheSubprocessStream(ctx, fullRebuild, false, emit)
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

// emitFolderArgs appends a --folder/--skip-folder flag for each
// element in values, e.g. "--folder Inbox --folder Archive".

func emitFolderArgs(args []string, flag string, values []string) []string {
	for _, v := range values {
		args = append(args, flag, v)
	}
	return args
}

func cliSyncSubprocessArgs(req api.CLISyncRequest) []string {
	if req.Full {
		args := []string{"sync-full"}
		if req.SourceIDSet {
			args = append(args, "--source-id", strconv.FormatInt(req.SourceID, 10))
		}
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
		args = emitFolderArgs(args, "--folder", req.Folders)
		args = emitFolderArgs(args, "--skip-folder", req.SkipFolders)
		if req.Email != "" {
			args = append(args, req.Email)
		}
		return args
	}
	args := []string{syncIncrementalCmd.Name()}
	if req.SourceIDSet {
		args = append(args, "--source-id", strconv.FormatInt(req.SourceID, 10))
	}
	args = emitFolderArgs(args, "--folder", req.Folders)
	args = emitFolderArgs(args, "--skip-folder", req.SkipFolders)
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
		args := req.Args
		if req.GrantDecided {
			args = append(append([]string(nil), args...), "--"+addAccountGrantDecidedFlag+"=true")
		}
		return run(ctx, args, req.Env, req.Cwd, emitSubprocess)
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
		if !attachmentRemovalCommand(req.Args) {
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

func attachmentRemovalCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == removeAccountCommandName {
		return true
	}
	if args[0] != purgeExcludedMediaCommandName {
		return false
	}
	confirmed := false
	for _, arg := range args[1:] {
		switch arg {
		case "--dry-run", "--dry-run=true":
			return false
		case purgeExcludedMediaYesFlag, "-y", purgeExcludedMediaYesFlag + "=true", "--" + purgeExcludedMediaConfirmedFlag,
			"--" + purgeExcludedMediaConfirmedFlag + "=true":
			confirmed = true
		}
	}
	return confirmed
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

func (a *storeAPIAdapter) CountAllDedupedContext(ctx context.Context) (int64, int64, error) {
	return a.store.CountAllDedupedContext(ctx)
}

func (a *storeAPIAdapter) CountDedupedBatches(batchIDs []string) ([]store.DedupedBatchCount, int64, error) {
	return a.store.CountDedupedBatches(batchIDs)
}

func (a *storeAPIAdapter) CountDedupedBatchesContext(
	ctx context.Context,
	batchIDs []string,
) ([]store.DedupedBatchCount, int64, error) {
	return a.store.CountDedupedBatchesContext(ctx, batchIDs)
}

func (a *storeAPIAdapter) DeleteAllDeduped() (int64, int64, error) {
	return a.store.DeleteAllDeduped()
}

func (a *storeAPIAdapter) DeleteAllDedupedContext(ctx context.Context) (int64, int64, error) {
	return a.store.DeleteAllDedupedContext(ctx)
}

func (a *storeAPIAdapter) DeleteDedupedBatch(batchID string) (int64, error) {
	return a.store.DeleteDedupedBatch(batchID)
}

func (a *storeAPIAdapter) DeleteDedupedBatchContext(
	ctx context.Context,
	batchID string,
) (int64, error) {
	return a.store.DeleteDedupedBatchContext(ctx, batchID)
}

func (a *storeAPIAdapter) DeleteDedupedBatches(batchIDs []string) (int64, error) {
	return a.store.DeleteDedupedBatches(batchIDs)
}

func (a *storeAPIAdapter) DeleteDedupedBatchesContext(
	ctx context.Context,
	batchIDs []string,
) (int64, error) {
	return a.store.DeleteDedupedBatchesContext(ctx, batchIDs)
}

func (a *storeAPIAdapter) BackupDatabase(dst string) error {
	return a.store.BackupDatabase(dst)
}

func (a *storeAPIAdapter) BackupDatabaseContext(ctx context.Context, dst string) error {
	return a.store.BackupDatabaseContext(ctx, dst)
}

func (a *storeAPIAdapter) CountMessagesForSource(sourceID int64) (int64, error) {
	return a.store.CountMessagesForSource(sourceID)
}

func (a *storeAPIAdapter) CountMessagesForSourceContext(
	ctx context.Context,
	sourceID int64,
) (int64, error) {
	return a.store.CountMessagesForSourceContext(ctx, sourceID)
}

func (a *storeAPIAdapter) CountSourceDeletedMessages(sourceIDs ...int64) (int64, error) {
	return a.store.CountSourceDeletedMessages(sourceIDs...)
}

func (a *storeAPIAdapter) CountSourceDeletedMessagesContext(
	ctx context.Context,
	sourceIDs ...int64,
) (int64, error) {
	return a.store.CountSourceDeletedMessagesContext(ctx, sourceIDs...)
}

func (a *storeAPIAdapter) ListSources(sourceType string) ([]*store.Source, error) {
	return a.store.ListSources(sourceType)
}

func (a *storeAPIAdapter) ListSourcesContext(
	ctx context.Context,
	sourceType string,
) ([]*store.Source, error) {
	return a.store.ListSourcesContext(ctx, sourceType)
}

func (a *storeAPIAdapter) GetSourcesByIdentifierOrDisplayName(query string) ([]*store.Source, error) {
	return a.store.GetSourcesByIdentifierOrDisplayName(query)
}

func (a *storeAPIAdapter) GetSourcesByIdentifierOrDisplayNameContext(
	ctx context.Context,
	query string,
) ([]*store.Source, error) {
	return a.store.GetSourcesByIdentifierOrDisplayNameContext(ctx, query)
}

func (a *storeAPIAdapter) GetSourcesByTypeAndAccount(
	sourceType, accountEmail string,
) ([]*store.Source, error) {
	return a.store.GetSourcesByTypeAndAccount(sourceType, accountEmail)
}

func (a *storeAPIAdapter) GetSourcesByTypeAndAccountContext(
	ctx context.Context,
	sourceType, accountEmail string,
) ([]*store.Source, error) {
	return a.store.GetSourcesByTypeAndAccountContext(ctx, sourceType, accountEmail)
}

func (a *storeAPIAdapter) GetCollectionByName(name string) (*store.CollectionWithSources, error) {
	return a.store.GetCollectionByName(name)
}

func (a *storeAPIAdapter) GetCollectionByNameContext(
	ctx context.Context,
	name string,
) (*store.CollectionWithSources, error) {
	return a.store.GetCollectionByNameContext(ctx, name)
}

func (a *storeAPIAdapter) ListCollections() ([]*store.CollectionWithSources, error) {
	return a.store.ListCollections()
}

func (a *storeAPIAdapter) ListCollectionsContext(
	ctx context.Context,
) ([]*store.CollectionWithSources, error) {
	return a.store.ListCollectionsContext(ctx)
}

func (a *storeAPIAdapter) CreateCollection(
	name, description string,
	sourceIDs []int64,
) (*store.Collection, error) {
	return a.store.CreateCollection(name, description, sourceIDs)
}

func (a *storeAPIAdapter) CreateCollectionContext(
	ctx context.Context,
	name, description string,
	sourceIDs []int64,
) (*store.Collection, error) {
	return a.store.CreateCollectionContext(ctx, name, description, sourceIDs)
}

func (a *storeAPIAdapter) AddSourcesToCollection(name string, sourceIDs []int64) error {
	return a.store.AddSourcesToCollection(name, sourceIDs)
}

func (a *storeAPIAdapter) AddSourcesToCollectionContext(
	ctx context.Context,
	name string,
	sourceIDs []int64,
) error {
	return a.store.AddSourcesToCollectionContext(ctx, name, sourceIDs)
}

func (a *storeAPIAdapter) RemoveSourcesFromCollection(name string, sourceIDs []int64) error {
	return a.store.RemoveSourcesFromCollection(name, sourceIDs)
}

func (a *storeAPIAdapter) RemoveSourcesFromCollectionContext(
	ctx context.Context,
	name string,
	sourceIDs []int64,
) error {
	return a.store.RemoveSourcesFromCollectionContext(ctx, name, sourceIDs)
}

func (a *storeAPIAdapter) DeleteCollection(name string) error {
	return a.store.DeleteCollection(name)
}

func (a *storeAPIAdapter) DeleteCollectionContext(ctx context.Context, name string) error {
	return a.store.DeleteCollectionContext(ctx, name)
}

func (a *storeAPIAdapter) UpdateSourceDisplayName(sourceID int64, displayName string) error {
	return a.store.UpdateSourceDisplayName(sourceID, displayName)
}

func (a *storeAPIAdapter) UpdateSourceDisplayNameContext(
	ctx context.Context,
	sourceID int64,
	displayName string,
) error {
	return a.store.UpdateSourceDisplayNameContext(ctx, sourceID, displayName)
}

func (a *storeAPIAdapter) GetSourceByID(id int64) (*store.Source, error) {
	return a.store.GetSourceByID(id)
}

func (a *storeAPIAdapter) GetSourceByIDContext(ctx context.Context, id int64) (*store.Source, error) {
	return a.store.GetSourceByIDContext(ctx, id)
}

func (a *storeAPIAdapter) ListAccountIdentities(sourceID int64) ([]store.AccountIdentity, error) {
	return a.store.ListAccountIdentities(sourceID)
}

func (a *storeAPIAdapter) ListAccountIdentitiesContext(
	ctx context.Context,
	sourceID int64,
) ([]store.AccountIdentity, error) {
	return a.store.ListAccountIdentitiesContext(ctx, sourceID)
}

func (a *storeAPIAdapter) ResolveAccountIdentityContext(
	ctx context.Context,
	sourceID int64,
	identifier string,
) (store.ResolvedAccountIdentity, error) {
	return a.store.ResolveAccountIdentityContext(ctx, sourceID, identifier)
}

func (a *storeAPIAdapter) MatchMessageIdentitiesContext(
	ctx context.Context,
	messageIDs []int64,
) (map[int64]store.MessageIdentityMatch, error) {
	return a.store.MatchMessageIdentitiesContext(ctx, messageIDs)
}

func (a *storeAPIAdapter) AddAccountIdentity(sourceID int64, address, signal string) error {
	return a.store.AddAccountIdentity(sourceID, address, signal)
}

func (a *storeAPIAdapter) AddAccountIdentityContext(
	ctx context.Context,
	sourceID int64,
	address, signal string,
) error {
	return a.store.AddAccountIdentityContext(ctx, sourceID, address, signal)
}

func (a *storeAPIAdapter) RemoveAccountIdentity(sourceID int64, address string) (int64, error) {
	return a.store.RemoveAccountIdentity(sourceID, address)
}

func (a *storeAPIAdapter) RemoveAccountIdentityContext(
	ctx context.Context,
	sourceID int64,
	address string,
) (int64, error) {
	return a.store.RemoveAccountIdentityContext(ctx, sourceID, address)
}

func (a *storeAPIAdapter) CountIdentityDiscoveryMessagesContext(
	ctx context.Context,
	sourceID int64,
) (int64, error) {
	return a.store.CountIdentityDiscoveryMessagesContext(ctx, sourceID)
}

func (a *storeAPIAdapter) ScanIdentityDiscoveryPageContext(
	ctx context.Context,
	sourceID, afterID int64,
	limit int,
) (store.IdentityDiscoveryPage, error) {
	return a.store.ScanIdentityDiscoveryPageContext(ctx, sourceID, afterID, limit)
}

func (a *storeAPIAdapter) ScanIdentityObservationsForSourceMessageIDsContext(
	ctx context.Context,
	sourceID int64,
	sourceMessageIDs []string,
) ([]store.IdentityObservation, error) {
	return a.store.ScanIdentityObservationsForSourceMessageIDsContext(ctx, sourceID, sourceMessageIDs)
}

func (a *storeAPIAdapter) AddAccountIdentitiesBatchContext(
	ctx context.Context,
	sourceID int64,
	candidates []store.IdentityConfirmation,
) ([]store.IdentityConfirmationOutcome, error) {
	return a.store.AddAccountIdentitiesBatchContext(ctx, sourceID, candidates)
}

func (a *storeAPIAdapter) MergeConfirmedAccountIdentitySignalsContext(
	ctx context.Context,
	sourceID int64,
	candidates []store.IdentityConfirmation,
) ([]store.IdentityConfirmationOutcome, error) {
	return a.store.MergeConfirmedAccountIdentitySignalsContext(ctx, sourceID, candidates)
}

func (a *storeAPIAdapter) LinkParticipants(participantA, participantB int64) (int64, error) {
	return a.store.LinkParticipants(participantA, participantB)
}

func (a *storeAPIAdapter) UnlinkParticipants(participantA, participantB int64) (int64, error) {
	return a.store.UnlinkParticipants(participantA, participantB)
}

func (a *storeAPIAdapter) IdentityRevision() (int64, error) {
	return a.store.IdentityRevision()
}

func (a *storeAPIAdapter) ListIdentityMatchCandidatesContext(
	ctx context.Context, states []store.IdentityMatchState, limit, offset int,
) ([]store.IdentityMatchCandidate, error) {
	return a.store.ListIdentityMatchCandidatesContext(ctx, states, limit, offset)
}

func (a *storeAPIAdapter) GetIdentityMatchCandidateContext(
	ctx context.Context, candidateID int64,
) (*store.IdentityMatchCandidate, error) {
	return a.store.GetIdentityMatchCandidateContext(ctx, candidateID)
}

func (a *storeAPIAdapter) AcceptIdentityMatchCandidateContext(
	ctx context.Context, candidateID int64, decidedBy string, notes *string,
) (*store.IdentityMatchCandidate, int64, error) {
	return a.store.AcceptIdentityMatchCandidateContext(ctx, candidateID, decidedBy, notes)
}

func (a *storeAPIAdapter) DecideIdentityMatchCandidateContext(
	ctx context.Context, candidateID int64, state store.IdentityMatchState,
	decidedBy string, notes *string,
) (*store.IdentityMatchCandidate, error) {
	return a.store.DecideIdentityMatchCandidateContext(ctx, candidateID, state, decidedBy, notes)
}

func (a *storeAPIAdapter) CreatePersonFromParticipantContext(
	ctx context.Context, participantID int64,
) (*store.Person, bool, error) {
	return a.store.CreatePersonFromParticipantContext(ctx, participantID)
}

func (a *storeAPIAdapter) GetPersonContext(ctx context.Context, id int64) (*store.Person, error) {
	return a.store.GetPersonContext(ctx, id)
}

func (a *storeAPIAdapter) GetPersonTrackingContext(
	ctx context.Context, id int64,
) (*store.PersonTracking, error) {
	return a.store.GetPersonTrackingContext(ctx, id)
}

func (a *storeAPIAdapter) SetPersonTrackingContext(
	ctx context.Context, id int64, tracked bool,
) (*store.PersonTracking, error) {
	return a.store.SetPersonTrackingContext(ctx, id, tracked)
}

func (a *storeAPIAdapter) BuildPersonFactCatalogContext(
	ctx context.Context, includeSensitive bool,
) (personfacts.Catalog, error) {
	return a.store.BuildPersonFactCatalogContext(ctx, includeSensitive)
}

func (a *storeAPIAdapter) ListPersonFactEvidenceContext(
	ctx context.Context, personID int64, filter personfacts.EvidenceFilter,
) ([]personfacts.Evidence, error) {
	return a.store.ListPersonFactEvidenceContext(ctx, personID, filter)
}

func (a *storeAPIAdapter) ListPersonFactEvidenceStatusEventsContext(
	ctx context.Context, personID int64, filter personfacts.EvidenceStatusFilter,
) ([]personfacts.EvidenceStatusEvent, error) {
	return a.store.ListPersonFactEvidenceStatusEventsContext(ctx, personID, filter)
}

func (a *storeAPIAdapter) ListPersonFactClaimsContext(
	ctx context.Context, personID int64, filter personfacts.ClaimFilter,
) ([]personfacts.Claim, error) {
	return a.store.ListPersonFactClaimsContext(ctx, personID, filter)
}

func (a *storeAPIAdapter) ListPersonFactDecisionsContext(
	ctx context.Context, personID int64, filter personfacts.DecisionFilter,
) ([]personfacts.Decision, error) {
	return a.store.ListPersonFactDecisionsContext(ctx, personID, filter)
}

func (a *storeAPIAdapter) ListPersonFactPinsContext(
	ctx context.Context, personID int64,
) ([]personfacts.PinState, error) {
	return a.store.ListPersonFactPinsContext(ctx, personID)
}

func (a *storeAPIAdapter) SetPersonFactPinContext(
	ctx context.Context, personID int64, target personfacts.TargetRef,
	pinned bool, actor string,
) (*personfacts.PinWrite, error) {
	return a.store.SetPersonFactPinContext(ctx, personID, target, pinned, actor)
}

func (a *storeAPIAdapter) ListPersonsContext(ctx context.Context) ([]store.Person, error) {
	return a.store.ListPersonsContext(ctx)
}

func (a *storeAPIAdapter) UpdatePersonDisplayNameContext(
	ctx context.Context, id, expectedRevision int64, displayName *string,
) (*store.Person, error) {
	return a.store.UpdatePersonDisplayNameContext(ctx, id, expectedRevision, displayName)
}

func (a *storeAPIAdapter) DeletePersonContext(ctx context.Context, id, expectedRevision int64) error {
	if !a.personEnrichmentConfig.Enabled {
		var hasHistory bool
		if err := a.store.DB().QueryRowContext(ctx, a.store.Rebind(`SELECT EXISTS (
			SELECT 1 FROM person_enrichment_attempts WHERE person_id = ?)`), id).Scan(&hasHistory); err != nil {
			return fmt.Errorf("check person enrichment history for deletion: %w", err)
		}
		if !hasHistory {
			return a.store.DeletePersonContext(ctx, id, expectedRevision)
		}
	}
	lookup := a.lookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	key, ok := lookup(a.personEnrichmentConfig.SuppressionKeyEnv)
	if !ok || key == "" {
		return fmt.Errorf("person enrichment suppression key environment %q is not set",
			a.personEnrichmentConfig.SuppressionKeyEnv)
	}
	keyBytes := []byte(key)
	hasher, err := personenrichment.NewSuppressionHasher(keyBytes)
	clear(keyBytes)
	if err != nil {
		return fmt.Errorf("load person enrichment suppression key for deletion: %w", err)
	}
	configuredKeyID, err := hasher.KeyID()
	if err != nil {
		return fmt.Errorf("load person enrichment suppression key ID for deletion: %w", err)
	}
	keyIDs, err := a.store.ListPersonEnrichmentSuppressionKeyIDsContext(ctx)
	if err != nil {
		return fmt.Errorf("load person enrichment suppression key state for deletion: %w", err)
	}
	for _, keyID := range keyIDs {
		if keyID != configuredKeyID {
			return personenrichment.ErrSuppressionKeyMismatch
		}
	}
	current, _, err := a.personEnrichmentDeletionDigests(ctx, id, hasher)
	if err != nil {
		return err
	}
	return a.store.DeletePersonWithEnrichmentSuppressionsContext(ctx,
		store.DeletePersonEnrichmentInput{
			PersonID: id, ExpectedRevision: expectedRevision, Actor: "api",
			Reason: store.PersonEnrichmentSuppressionDeletion, ConfiguredKeyID: configuredKeyID,
			CurrentIdentifiers: current,
		})
}

func (a *storeAPIAdapter) personEnrichmentDeletionDigests(
	ctx context.Context,
	personID int64,
	hasher *personenrichment.SuppressionHasher,
) ([]store.PersonEnrichmentSuppressionInput, int64, error) {
	person, err := a.store.GetPersonContext(ctx, personID)
	if err != nil {
		return nil, 0, err
	}
	input, err := a.store.LoadRequestInput(ctx, personenrichment.WorkLease{PersonID: person.ID})
	if err != nil {
		return nil, 0, fmt.Errorf("load current person enrichment identifiers for deletion: %w", err)
	}
	defer func() {
		clearPersonEnrichmentCandidates(input.Names)
		clearPersonEnrichmentCandidates(input.CurrentCompanies)
		clearPersonEnrichmentCandidates(input.Emails)
		clearPersonEnrichmentCandidates(input.Phones)
		clearPersonEnrichmentCandidates(input.PublicProfileURLs)
	}()
	rows, err := a.store.DB().QueryContext(ctx, `
		SELECT provider_namespace FROM person_enrichment_profiles
		GROUP BY provider_namespace ORDER BY provider_namespace`)
	if err != nil {
		return nil, 0, fmt.Errorf("list persisted person enrichment namespaces for deletion: %w", err)
	}
	defer func() { _ = rows.Close() }()
	namespaces := make([]string, 0)
	for rows.Next() {
		var namespace string
		if err := rows.Scan(&namespace); err != nil {
			_ = rows.Close()
			return nil, 0, fmt.Errorf("read persisted person enrichment namespace for deletion: %w", err)
		}
		namespaces = append(namespaces, namespace)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("close persisted person enrichment namespaces for deletion: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate persisted person enrichment namespaces for deletion: %w", err)
	}

	digests := make([]store.PersonEnrichmentSuppressionInput, 0)
	seen := make(map[string]struct{})
	appendDigest := func(namespace string, class personenrichment.SuppressionIdentifierClass, values ...string) error {
		normalized, normalizeErr := personenrichment.NormalizeSuppressionIdentifier(class, values)
		for i := range values {
			values[i] = ""
		}
		if normalizeErr != nil {
			return normalizeErr
		}
		digest := hasher.Digest(namespace, normalized.Class, normalized.NormalizationVersion, normalized.Value)
		normalized.Value = ""
		key := digest.ProviderNamespace + "\x00" + string(digest.IdentifierClass) + "\x00" +
			digest.NormalizationVersion + "\x00" + digest.KeyID + "\x00" + string(digest.Digest)
		if _, exists := seen[key]; exists {
			return nil
		}
		seen[key] = struct{}{}
		digests = append(digests, store.PersonEnrichmentSuppressionInput{
			ProviderNamespace: digest.ProviderNamespace, IdentifierClass: digest.IdentifierClass,
			NormalizationVersion: digest.NormalizationVersion, KeyID: digest.KeyID,
			Digest: digest.Digest,
		})
		return nil
	}
	for _, namespace := range namespaces {
		for i := range input.Emails {
			if err := appendDigest(namespace, personenrichment.SuppressionEmail, input.Emails[i].Value); err != nil {
				return nil, 0, fmt.Errorf("normalize current email for deletion: %w", err)
			}
		}
		for i := range input.Phones {
			if err := appendDigest(namespace, personenrichment.SuppressionPhone, input.Phones[i].Value); err != nil {
				return nil, 0, fmt.Errorf("normalize current phone for deletion: %w", err)
			}
		}
		for i := range input.PublicProfileURLs {
			if err := appendDigest(namespace, personenrichment.SuppressionPublicProfileURL,
				input.PublicProfileURLs[i].Value); err != nil {
				return nil, 0, fmt.Errorf("normalize current public profile URL for deletion: %w", err)
			}
		}
		for i := range input.Names {
			for j := range input.CurrentCompanies {
				if err := appendDigest(namespace, personenrichment.SuppressionNameCompany,
					input.Names[i].Value, input.CurrentCompanies[j].Value); err != nil {
					return nil, 0, fmt.Errorf("normalize current name-company identity for deletion: %w", err)
				}
			}
		}
		providerIDs, loadErr := a.store.LoadProviderPersonIDs(ctx, personID, namespace)
		if loadErr != nil {
			return nil, 0, loadErr
		}
		for i := range providerIDs {
			if err := appendDigest(namespace, personenrichment.SuppressionProviderPersonID, providerIDs[i]); err != nil {
				clearPersonEnrichmentStrings(providerIDs)
				return nil, 0, fmt.Errorf("normalize stored provider identity for deletion: %w", err)
			}
		}
		clearPersonEnrichmentStrings(providerIDs)
	}
	return digests, person.Revision, nil
}

func clearPersonEnrichmentCandidates(values []personenrichment.IdentityCandidate) {
	for i := range values {
		values[i].Value = ""
	}
}

func clearPersonEnrichmentStrings(values []string) {
	for i := range values {
		values[i] = ""
	}
}

func (a *storeAPIAdapter) PersonForParticipantsContext(
	ctx context.Context, participantIDs []int64,
) (*store.Person, error) {
	return a.store.PersonForParticipantsContext(ctx, participantIDs)
}

func (a *storeAPIAdapter) MergePersonsContext(
	ctx context.Context, request store.PersonMergeRequest,
) (*store.PersonMergeResult, error) {
	return a.store.MergePersonsContext(ctx, request)
}

func (a *storeAPIAdapter) SplitPersonMergeContext(
	ctx context.Context, request store.PersonSplitRequest,
) (*store.PersonSplitResult, error) {
	return a.store.SplitPersonMergeContext(ctx, request)
}

func (a *storeAPIAdapter) ListPersonMergesPageContext(
	ctx context.Context, personID int64, limit, offset int,
) ([]store.PersonMergeSummary, error) {
	return a.store.ListPersonMergesPageContext(ctx, personID, limit, offset)
}

func (a *storeAPIAdapter) GetPersonMergeContext(
	ctx context.Context, mergeID int64,
) (*store.PersonMergeDetail, error) {
	return a.store.GetPersonMergeContext(ctx, mergeID)
}

func (a *storeAPIAdapter) GetPersonMergeSnapshotContext(
	ctx context.Context, mergeID int64,
) (*store.PersonMergeSnapshotResponse, error) {
	return a.store.GetPersonMergeSnapshotContext(ctx, mergeID)
}

func (a *storeAPIAdapter) DecidePersonMergeCandidateContext(
	ctx context.Context, request store.PersonMergeCandidateDecisionRequest,
) (*store.PersonMergeCandidateDecisionResult, error) {
	return a.store.DecidePersonMergeCandidateContext(ctx, request)
}

func (a *storeAPIAdapter) CompletePersonProfilesContext(
	ctx context.Context, request store.PersonCompletionQuery,
) ([]store.PersonCompletion, error) {
	return a.store.CompletePersonProfilesContext(ctx, request)
}

func (a *storeAPIAdapter) GetPersonProfileContext(
	ctx context.Context, personID int64,
) (*store.PersonProfile, error) {
	return a.store.GetPersonProfileContext(ctx, personID)
}

func (a *storeAPIAdapter) ApplyPersonProfilePatchContext(
	ctx context.Context,
	personID, expectedRevision int64,
	patch store.PersonProfilePatch,
) (*store.PersonProfile, error) {
	return a.store.ApplyPersonProfilePatchContext(ctx, personID, expectedRevision, patch)
}

func (a *storeAPIAdapter) GetPersonProfileHistoryContext(
	ctx context.Context, personID int64,
) (*store.PersonProfileHistory, error) {
	return a.store.GetPersonProfileHistoryContext(ctx, personID)
}

func (a *storeAPIAdapter) ReadPersonMediaDataContext(
	ctx context.Context, personID, mediaID int64,
) ([]byte, string, error) {
	return a.store.ReadPersonMediaDataContext(ctx, personID, mediaID)
}

func (a *storeAPIAdapter) ListCommunicationServicesContext(
	ctx context.Context, includeInactive bool,
) ([]store.CommunicationService, error) {
	return a.store.ListCommunicationServicesContext(ctx, includeInactive)
}

func (a *storeAPIAdapter) EnsureCommunicationServiceContext(
	ctx context.Context, input store.CommunicationServiceInput,
) (*store.CommunicationService, bool, error) {
	return a.store.EnsureCommunicationServiceContext(ctx, input)
}

func (a *storeAPIAdapter) ListAttributeDefinitionsContext(
	ctx context.Context, filter store.AttributeDefinitionFilter,
) ([]store.AttributeDefinition, error) {
	return a.store.ListAttributeDefinitionsContext(ctx, filter)
}

func (a *storeAPIAdapter) GetAttributeDefinitionContext(
	ctx context.Context, id int64,
) (*store.AttributeDefinition, error) {
	return a.store.GetAttributeDefinitionContext(ctx, id)
}

func (a *storeAPIAdapter) GetAttributeDefinitionBySlugContext(
	ctx context.Context, objectType store.AttributeObjectType, slug string,
) (*store.AttributeDefinition, error) {
	return a.store.GetAttributeDefinitionBySlugContext(ctx, objectType, slug)
}

func (a *storeAPIAdapter) CreateAttributeDefinitionContext(
	ctx context.Context, input store.AttributeDefinitionInput,
) (*store.AttributeDefinition, error) {
	return a.store.CreateAttributeDefinitionContext(ctx, input)
}

func (a *storeAPIAdapter) UpdateAttributeDefinitionContext(
	ctx context.Context, id, expectedRevision int64, update store.AttributeDefinitionUpdate,
) (*store.AttributeDefinition, error) {
	return a.store.UpdateAttributeDefinitionContext(ctx, id, expectedRevision, update)
}

func (a *storeAPIAdapter) DeleteAttributeDefinitionContext(
	ctx context.Context, id, expectedRevision int64,
) error {
	return a.store.DeleteAttributeDefinitionContext(ctx, id, expectedRevision)
}

func (a *storeAPIAdapter) ListPersonAttributeValuesContext(
	ctx context.Context, personID int64, query store.PersonAttributeQuery,
) ([]store.PersonAttributeValue, error) {
	return a.store.ListPersonAttributeValuesContext(ctx, personID, query)
}

func (a *storeAPIAdapter) SetPersonAttributeValueContext(
	ctx context.Context, input store.PersonAttributeValueInput,
) (*store.PersonAttributeWrite, error) {
	return a.store.SetPersonAttributeValueContext(ctx, input)
}

func (a *storeAPIAdapter) AppendPersonNoteContext(
	ctx context.Context, input store.PersonNoteAppendInput,
) (*store.PersonAttributeWrite, error) {
	return a.store.AppendPersonNoteContext(ctx, input)
}

func (a *storeAPIAdapter) SupersedePersonAttributeValueContext(
	ctx context.Context, input store.PersonAttributeSupersedeInput,
) (*store.PersonAttributeWrite, error) {
	return a.store.SupersedePersonAttributeValueContext(ctx, input)
}

func (a *storeAPIAdapter) ListRelationshipTypesContext(
	ctx context.Context,
) ([]store.RelationshipType, error) {
	return a.store.ListRelationshipTypesContext(ctx)
}

func (a *storeAPIAdapter) GetRelationshipTypeContext(
	ctx context.Context, id int64,
) (*store.RelationshipType, error) {
	return a.store.GetRelationshipTypeContext(ctx, id)
}

func (a *storeAPIAdapter) CreateRelationshipTypeContext(
	ctx context.Context, input store.RelationshipTypeInput,
) (*store.RelationshipType, error) {
	return a.store.CreateRelationshipTypeContext(ctx, input)
}

func (a *storeAPIAdapter) UpdateRelationshipTypeContext(
	ctx context.Context, id, expectedRevision int64, update store.RelationshipTypeUpdate,
) (*store.RelationshipType, error) {
	return a.store.UpdateRelationshipTypeContext(ctx, id, expectedRevision, update)
}

func (a *storeAPIAdapter) DeleteRelationshipTypeContext(
	ctx context.Context, id, expectedRevision int64,
) error {
	return a.store.DeleteRelationshipTypeContext(ctx, id, expectedRevision)
}

func (a *storeAPIAdapter) AddPersonRelationshipContext(
	ctx context.Context, input store.PersonRelationshipInput,
) (*store.PersonRelationship, error) {
	return a.store.AddPersonRelationshipContext(ctx, input)
}

func (a *storeAPIAdapter) GetPersonRelationshipContext(
	ctx context.Context, id int64,
) (*store.PersonRelationship, error) {
	return a.store.GetPersonRelationshipContext(ctx, id)
}

func (a *storeAPIAdapter) PatchPersonRelationshipContext(
	ctx context.Context, id, expectedRevision int64, patch store.PersonRelationshipPatch, actor string,
) (*store.PersonRelationship, error) {
	return a.store.PatchPersonRelationshipContext(ctx, id, expectedRevision, patch, actor)
}

func (a *storeAPIAdapter) DeletePersonRelationshipContext(
	ctx context.Context, id, expectedRevision int64,
) error {
	return a.store.DeletePersonRelationshipContext(ctx, id, expectedRevision)
}

func (a *storeAPIAdapter) ListPersonRelationshipsContext(
	ctx context.Context, personID int64, opts store.PersonRelationshipListOptions,
) ([]store.PersonRelationshipView, error) {
	return a.store.ListPersonRelationshipsContext(ctx, personID, opts)
}

func (a *storeAPIAdapter) ListRelationshipReviewsContext(
	ctx context.Context, opts store.RelationshipReviewListOptions,
) ([]store.RelationshipReview, error) {
	return a.store.ListRelationshipReviewsContext(ctx, opts)
}

func (a *storeAPIAdapter) CreateOrganizationContext(
	ctx context.Context, input store.OrganizationInput,
) (*store.Organization, error) {
	return a.store.CreateOrganizationContext(ctx, input)
}

func (a *storeAPIAdapter) GetOrganizationContext(
	ctx context.Context, id int64,
) (*store.Organization, error) {
	return a.store.GetOrganizationContext(ctx, id)
}

func (a *storeAPIAdapter) ListOrganizationsContext(
	ctx context.Context, filter store.OrganizationFilter,
) ([]store.Organization, error) {
	return a.store.ListOrganizationsContext(ctx, filter)
}

func (a *storeAPIAdapter) CountOrganizationsContext(
	ctx context.Context, filter store.OrganizationFilter,
) (int64, error) {
	return a.store.CountOrganizationsContext(ctx, filter)
}

func (a *storeAPIAdapter) ReplaceOrganizationContext(
	ctx context.Context, id, expectedRevision int64, input store.OrganizationInput, retired bool,
) (*store.Organization, error) {
	return a.store.ReplaceOrganizationContext(ctx, id, expectedRevision, input, retired)
}

func (a *storeAPIAdapter) DeleteOrganizationContext(
	ctx context.Context, id, expectedRevision int64,
) error {
	return a.store.DeleteOrganizationContext(ctx, id, expectedRevision)
}

func (a *storeAPIAdapter) MergeOrganizationsContext(
	ctx context.Context, survivorID, survivorRevision, losingID, losingRevision int64,
) (*store.Organization, error) {
	return a.store.MergeOrganizationsContext(
		ctx, survivorID, survivorRevision, losingID, losingRevision,
	)
}

func (a *storeAPIAdapter) GetOrganizationProfileContext(
	ctx context.Context, id int64, includeSuperseded bool,
) (*store.OrganizationProfile, error) {
	return a.store.GetOrganizationProfileContext(ctx, id, includeSuperseded)
}

func (a *storeAPIAdapter) ReplaceOrganizationProfileContext(
	ctx context.Context, id, expectedRevision int64, input store.OrganizationProfileInput,
) (*store.OrganizationProfile, error) {
	return a.store.ReplaceOrganizationProfileContext(ctx, id, expectedRevision, input)
}

func (a *storeAPIAdapter) ListOrganizationAttributeValuesContext(
	ctx context.Context, organizationID int64, query store.OrganizationAttributeQuery,
) ([]store.OrganizationAttributeValue, error) {
	return a.store.ListOrganizationAttributeValuesContext(ctx, organizationID, query)
}

func (a *storeAPIAdapter) SetOrganizationAttributeValueContext(
	ctx context.Context, input store.OrganizationAttributeValueInput,
) (*store.OrganizationAttributeWrite, error) {
	return a.store.SetOrganizationAttributeValueContext(ctx, input)
}

func (a *storeAPIAdapter) SupersedeOrganizationAttributeValueContext(
	ctx context.Context, input store.OrganizationAttributeSupersedeInput,
) (*store.OrganizationAttributeWrite, error) {
	return a.store.SupersedeOrganizationAttributeValueContext(ctx, input)
}

func (a *storeAPIAdapter) ReadOrganizationMediaDataContext(
	ctx context.Context, organizationID, mediaID int64,
) ([]byte, string, error) {
	return a.store.ReadOrganizationMediaDataContext(ctx, organizationID, mediaID)
}

func (a *storeAPIAdapter) AddEmploymentContext(
	ctx context.Context, input store.EmploymentInput,
) (*store.Employment, error) {
	return a.store.AddEmploymentContext(ctx, input)
}

func (a *storeAPIAdapter) GetEmploymentContext(
	ctx context.Context, id int64,
) (*store.Employment, error) {
	return a.store.GetEmploymentContext(ctx, id)
}

func (a *storeAPIAdapter) UpdateEmploymentContext(
	ctx context.Context, id, expectedRevision int64, input store.EmploymentInput,
) (*store.Employment, error) {
	return a.store.UpdateEmploymentContext(ctx, id, expectedRevision, input)
}

func (a *storeAPIAdapter) EndEmploymentContext(
	ctx context.Context, id, expectedRevision int64, endDate store.PartialDate,
) (*store.Employment, error) {
	return a.store.EndEmploymentContext(ctx, id, expectedRevision, endDate)
}

func (a *storeAPIAdapter) SetPrimaryEmploymentContext(
	ctx context.Context, id, expectedRevision int64,
) (*store.Employment, error) {
	return a.store.SetPrimaryEmploymentContext(ctx, id, expectedRevision)
}

func (a *storeAPIAdapter) DeleteEmploymentContext(
	ctx context.Context, id, expectedRevision int64,
) error {
	return a.store.DeleteEmploymentContext(ctx, id, expectedRevision)
}

func (a *storeAPIAdapter) ListEmploymentsContext(
	ctx context.Context, filter store.EmploymentFilter,
) ([]store.Employment, error) {
	return a.store.ListEmploymentsContext(ctx, filter)
}

func (a *storeAPIAdapter) PrimaryCurrentEmploymentContext(
	ctx context.Context, personID int64,
) (store.EmploymentProjection, bool, error) {
	return a.store.PrimaryCurrentEmploymentContext(ctx, personID)
}

func (a *storeAPIAdapter) ClusterMembers(id int64) ([]int64, error) {
	return a.store.ClusterMembers(id)
}

func (a *storeAPIAdapter) ClusterEdges(id int64) ([]store.LinkEdge, error) {
	return a.store.ClusterEdges(id)
}

// RefreshIdentityDatasets rebuilds identity-derived Parquet in a short-lived,
// resource-bounded child. The child owns the cache lock and its DuckDB
// allocator exits with the process; the long-lived daemon does neither.
func (a *storeAPIAdapter) RefreshIdentityDatasets(ctx context.Context) (int64, error) {
	if err := runDerivedCacheSubprocess(ctx, a.analyticsDir); err != nil {
		return 0, err
	}
	state, err := query.ReadCacheSyncState(a.analyticsDir)
	if err != nil {
		return 0, fmt.Errorf("read refreshed cache revision: %w", err)
	}
	return state.IdentityRevision, nil
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

const personEnrichmentJob = "person-enrichment"

type personEnrichmentScheduleWorker interface {
	RunOnce(ctx context.Context, runID int64) (bool, error)
}

// personEnrichmentSchedule owns only wake-up ordering. Runs, attempts, retry
// times, provider jobs, and spend remain database-owned.
type personEnrichmentSchedule struct {
	Store        *store.Store
	Worker       personEnrichmentScheduleWorker
	CatchUpLimit int
}

func (r *personEnrichmentSchedule) Wake(ctx context.Context, occurrence time.Time) error {
	if r == nil || r.Store == nil || r.Worker == nil || occurrence.IsZero() ||
		r.CatchUpLimit < 1 || r.CatchUpLimit > 200 {
		return errors.New("person enrichment schedule is invalid")
	}
	runs := make([]personenrichment.DurableRun, 0)
	var afterID int64
	var afterRequestedAt time.Time
	for {
		page, err := r.Store.ListRunningRuns(ctx, personenrichment.RunningRunFilter{
			AfterRequestedAt: afterRequestedAt, AfterID: afterID, Limit: 200,
		})
		if err != nil {
			return fmt.Errorf("list running person enrichment runs: %w", err)
		}
		runs = append(runs, page...)
		if len(page) < 200 {
			break
		}
		afterID = page[len(page)-1].ID
		afterRequestedAt = page[len(page)-1].RequestedAt
	}
	for _, run := range runs {
		if err := r.drainRun(ctx, run.ID); err != nil {
			return err
		}
	}

	requestedAt := occurrence.UTC().Truncate(time.Minute)
	run, _, err := r.Store.StartRun(ctx, personenrichment.RunStart{
		Kind: "scheduled", RequestedBy: canonicalPersonEnrichmentOccurrence(requestedAt),
		RequestedAt: requestedAt,
	})
	if err != nil {
		return fmt.Errorf("start scheduled person enrichment run: %w", err)
	}
	if run.State != "running" {
		return nil
	}
	for {
		count, err := r.Store.EnqueueDuePersonEnrichmentContext(
			ctx, requestedAt, r.CatchUpLimit)
		if err != nil {
			return fmt.Errorf("catch up person enrichment work: %w", err)
		}
		if count < r.CatchUpLimit {
			break
		}
	}
	return r.drainRun(ctx, run.ID)
}

func (r *personEnrichmentSchedule) drainRun(ctx context.Context, runID int64) error {
	for {
		processed, err := r.Worker.RunOnce(ctx, runID)
		if err != nil {
			return fmt.Errorf("run person enrichment work for run %d: %w", runID, err)
		}
		if !processed {
			break
		}
	}
	err := r.Store.CompleteRun(ctx, runID, personenrichment.RunCompletion{
		State: "", CompletedAt: time.Now().UTC(),
	})
	if errors.Is(err, store.ErrRunNotTerminal) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("complete person enrichment run %d: %w", runID, err)
	}
	return nil
}

func canonicalPersonEnrichmentOccurrence(occurrence time.Time) string {
	return occurrence.UTC().Truncate(time.Minute).Format(time.RFC3339)
}

func registerPersonEnrichmentJob(
	ctx context.Context,
	sched *scheduler.Scheduler,
	st *store.Store,
	enrichmentConfig personenrichment.Config,
) error {
	if !enrichmentConfig.Enabled {
		if st == nil {
			return nil
		}
		if err := st.CancelPersonEnrichmentWorkOutsideProfilesContext(ctx, nil); err != nil {
			return fmt.Errorf("cancel disabled person enrichment work: %w", err)
		}
		return nil
	}
	if sched == nil || st == nil {
		return errors.New("person enrichment schedule requires scheduler and store")
	}
	if err := enrichmentConfig.Validate(); err != nil {
		return err
	}
	suppressionKey, ok := os.LookupEnv(enrichmentConfig.SuppressionKeyEnv)
	if !ok || suppressionKey == "" {
		return fmt.Errorf("person enrichment suppression key environment %q is not set",
			enrichmentConfig.SuppressionKeyEnv)
	}
	hasher, err := personenrichment.NewSuppressionHasher([]byte(suppressionKey))
	if err != nil {
		return fmt.Errorf("load person enrichment suppression key: %w", err)
	}
	catalog, err := st.BuildPersonFactCatalogContext(ctx, true)
	if err != nil {
		return fmt.Errorf("build person enrichment target catalog: %w", err)
	}
	factories := make(map[string]personenrichment.ProviderFactory)
	providerConfigs := make(map[string]personenrichment.ProviderConfig)
	activeFingerprints := make([]string, 0, len(enrichmentConfig.Providers))
	for _, configured := range enrichmentConfig.Providers {
		provider := configured
		if !provider.Enabled {
			continue
		}
		profile, err := provider.Profile(catalog)
		if err != nil {
			return fmt.Errorf("build person enrichment profile %q: %w", provider.Name, err)
		}
		if _, err := st.EnsurePersonEnrichmentProfile(ctx, profile); err != nil {
			return fmt.Errorf("ensure person enrichment profile %q: %w", provider.Name, err)
		}
		activeFingerprints = append(activeFingerprints, profile.Fingerprint)
		providerConfigs[provider.Name] = provider
		switch provider.Kind {
		case personenrichment.ProviderExa:
			factories[provider.Name] = func(config personenrichment.ProviderConfig, credential string) (personenrichment.Provider, error) {
				return personenrichment.NewExaProvider(config, credential, http.DefaultClient)
			}
		case personenrichment.ProviderSixtyfour:
			factories[provider.Name] = func(config personenrichment.ProviderConfig, credential string) (personenrichment.Provider, error) {
				return personenrichment.NewSixtyfourProvider(config, credential, http.DefaultClient)
			}
		default:
			return fmt.Errorf("unsupported person enrichment provider kind %q", provider.Kind)
		}
	}
	if len(factories) == 0 {
		return errors.New("person enrichment has no structurally valid enabled provider")
	}
	if err := st.CancelPersonEnrichmentWorkOutsideProfilesContext(ctx, activeFingerprints); err != nil {
		return fmt.Errorf("cancel unavailable person enrichment work: %w", err)
	}
	gate, err := personenrichment.NewEgressGate(st, st, hasher, os.LookupEnv)
	if err != nil {
		return fmt.Errorf("configure person enrichment egress: %w", err)
	}
	worker, err := personenrichment.NewWorker(st, st, *gate, factories, personenrichment.WorkerOptions{
		Owner: "daemon-person-enrichment", LeaseDuration: enrichmentConfig.LeaseDuration,
		RenewEvery: enrichmentConfig.LeaseDuration / 4, Clock: time.Now,
		Jitter:          func(delay time.Duration) time.Duration { return delay },
		ProviderConfigs: providerConfigs,
	})
	if err != nil {
		return fmt.Errorf("configure person enrichment worker: %w", err)
	}
	runner := &personEnrichmentSchedule{
		Store: st, Worker: worker, CatchUpLimit: min(enrichmentConfig.BatchSize, 200),
	}
	return sched.AddJob(scheduler.Job{
		Name: personEnrichmentJob, Schedule: enrichmentConfig.Schedule,
		Run: func(ctx context.Context) error {
			return runner.Wake(ctx, time.Now())
		},
	})
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

func (a *schedulerAdapter) JobStatus() []api.JobStatus {
	return a.scheduler.JobStatus()
}

func (a *schedulerAdapter) IsJobScheduled(name string) bool {
	return a.scheduler.IsJobScheduled(name)
}

func (a *schedulerAdapter) TriggerJob(name string) error {
	return a.scheduler.TriggerJob(name)
}

func (a *schedulerAdapter) StartJob(name string) error {
	return a.scheduler.StartJob(name)
}

// runScheduledSync performs a sync for a scheduled account. It resolves
// ALL syncable source rows for the identifier (gmail, imap, teams, discord) and
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
		summary, syncErr := runScheduledGmailSync(ctx, identifier, nil, s, getOAuthMgr)
		if syncErr == nil {
			logger.Info("sync completed",
				"identifier", identifier,
				"source_type", sourceTypeGmail,
				"messages_added", summary.MessagesAdded,
				"duration", time.Since(startTime),
			)
		}
		return errors.Join(syncErr, rebuildCacheAfterScheduledSourceRun(context.WithoutCancel(ctx), identifier))
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
		case sourceTypeDiscord:
			var discordSummary *discord.ImportSummary
			discordSummary, err = importDiscordSourceForScheduledRun(
				ctx, s, src, defaultDiscordCommandDeps(), false, time.Time{}, nil,
			)
			logScheduledDiscordIssues(identifier, discordSummary)
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
	if err := rebuildCacheAfterScheduledSourceRun(context.WithoutCancel(ctx), identifier); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func logScheduledDiscordIssues(identifier string, summary *discord.ImportSummary) {
	if summary == nil {
		return
	}
	for _, issue := range summary.CatalogIssues {
		logger.Warn("discord catalog issue",
			"identifier", identifier,
			"scope", issue.Scope,
			"kind", issue.Kind,
			"guild_id", issue.GuildID,
			"parent_id", issue.ParentID,
			"status_code", issue.StatusCode,
			"discord_code", issue.DiscordCode,
		)
	}
	for _, issue := range summary.ContainerIssues {
		logger.Warn("discord container issue",
			"identifier", identifier,
			"container_id", issue.ContainerID,
			"kind", issue.Kind,
			"status_code", issue.StatusCode,
			"discord_code", issue.DiscordCode,
		)
	}
}

// findScheduledSyncSources resolves ALL syncable source rows for a
// scheduler identifier. Returns at most one row per syncable type
// (gmail, imap, teams, discord), in that stable order. Non-syncable types
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
	seen := make(map[string]*store.Source, 4)
	for _, src := range rows {
		switch src.SourceType {
		case sourceTypeGmail, sourceTypeIMAP, sourceTypeTeams:
			if _, dup := seen[src.SourceType]; !dup {
				seen[src.SourceType] = src
			}
		case sourceTypeDiscord:
			// Guild display names are not stable or unique. Scheduled Discord
			// jobs must use the exact guild snowflake as their key.
			if src.Identifier == identifier {
				seen[src.SourceType] = src
			}
		}
	}

	// Return in stable order: gmail, imap, teams, discord.
	var result []*store.Source
	for _, t := range []string{sourceTypeGmail, sourceTypeIMAP, sourceTypeTeams, sourceTypeDiscord} {
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

	syncer := newMessageSyncer(client, s, opts).WithLogger(logger)

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

	summary, err := syncer.IncrementalWithHistoryRecovery(ctx, source, func(resumed bool) {
		if resumed {
			logger.Warn("resuming interrupted Gmail history recovery", keyEmail, email)
		} else {
			logger.Warn("gmail history expired; reconciling complete mailbox", keyEmail, email)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("gmail sync failed: %w", err)
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
	imapOpts := imapFolderStateOptions(s, src, false)
	apiClient, err := buildAPIClient(ctx, src, nil, nil, imapOpts...)
	if err != nil {
		return nil, fmt.Errorf("build IMAP client: %w", err)
	}
	defer func() { _ = apiClient.Close() }()

	opts := sync.DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	opts.AttachmentsDir = cfg.AttachmentsDir()
	opts.NoResume = true

	syncer := newMessageSyncer(apiClient, s, opts).WithLogger(logger)

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
	if err := saveIMAPFolderStates(ctx, s, src, apiClient, summary, 0); err != nil {
		return nil, fmt.Errorf("save IMAP incremental state: %w", err)
	}
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

	mgr := microsoft.NewGraphManager(cfg.Microsoft.ClientID, cfg.Microsoft.EffectiveTenantID(), cfg.Microsoft.EffectiveRedirectURI(), cfg.TokensDir(), logger)
	tokenFn, err := mgr.TokenSource(ctx, email)
	if err != nil {
		return err
	}
	qps := float64(cfg.Sync.RateLimitQPS)
	if qps <= 0 {
		qps = 5
	}
	client := teams.NewClient("https://graph.microsoft.com/v1.0", teams.TokenFunc(tokenFn), qps)
	opts := scheduledTeamsImportOptions(email)
	_, err = teams.NewImporter(s, client).Import(ctx, opts)
	return err
}

func scheduledTeamsImportOptions(email string) teams.ImportOptions {
	return teams.ImportOptions{
		Email:           email,
		AttachmentsDir:  cfg.AttachmentsDir(),
		MediaPolicy:     cfg.Teams.MediaPolicy(email),
		IncludeChannels: true,
	}
}
