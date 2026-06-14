package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/syncerr"
	"golang.org/x/oauth2"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run msgvault as a daemon with scheduled sync",
	Long: `Run msgvault as a long-running daemon that syncs email accounts on schedule.

The daemon runs in the foreground and performs:
  - HTTP API server on configured port (default: 8080)
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

func init() {
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, args []string) error {
	// Validate security posture before doing any work
	if err := cfg.Server.ValidateSecure(); err != nil {
		return err
	}
	if cfg.Server.APIKey != "" && len(cfg.Server.APIKey) < 16 {
		logger.Warn("api_key is very short — use a randomly generated key of at least 32 characters")
	}

	// Validate config
	if !cfg.OAuth.HasAnyConfig() {
		return errOAuthNotConfigured()
	}

	// Check for scheduled accounts (warn but don't fail - allows token upload first)
	scheduled := cfg.ScheduledAccounts()
	if len(scheduled) == 0 {
		logger.Warn("no scheduled accounts configured - server will start but no syncs will run",
			"hint", "Add accounts to config.toml or upload tokens via API first")
	}

	// Open database
	dbPath := cfg.DatabaseDSN()
	s, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.InitSchema(); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
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

	// Build optional vector-search components. Returns (nil, nil) when
	// cfg.Vector.Enabled is false, or an error when enabled but the
	// binary was built without -tags sqlite_vec.
	vf, err := setupVectorFeatures(ctx, s.DB(), dbPath)
	if err != nil {
		return fmt.Errorf("vector features: %w", err)
	}
	defer func() {
		if vf != nil && vf.Close != nil {
			if closeErr := vf.Close(); closeErr != nil {
				logger.Warn("closing vectors.db failed", "error", closeErr)
			}
		}
	}()

	// Create query engine for TUI aggregate support.
	// Prefer DuckDB over Parquet when the cache is complete and fresh;
	// otherwise fall back to SQLite so remote endpoints still work.
	// PostgreSQL bypasses the cache entirely — it is a SQLite-only ETL.
	analyticsDir := cfg.AnalyticsDir()
	var engine query.Engine
	if s.IsPostgreSQL() {
		engine = query.NewEngine(s.DB(), true)
	} else {
		staleness := cacheNeedsBuild(dbPath, analyticsDir)
		if !staleness.NeedsBuild && query.HasCompleteParquetData(analyticsDir) {
			// DisableSQLiteScanner keeps DuckDB's bundled SQLite library
			// from ATTACHing the live database for the daemon's entire
			// lifetime, which would corrupt the daemon's own go-sqlite3
			// connections' WAL/lock state (issue #379). Detail queries
			// route through the shared go-sqlite3 connection instead;
			// aggregates still read Parquet.
			duckEngine, engineErr := query.NewDuckDBEngine(
				analyticsDir, dbPath, s.DB(),
				query.DuckDBOptions{DisableSQLiteScanner: true},
			)
			if engineErr != nil {
				logger.Warn("DuckDB engine failed, falling back to SQLite",
					"error", engineErr)
				engine = query.NewEngine(s.DB(), false)
			} else {
				engine = duckEngine
			}
		} else {
			if staleness.Reason != "" {
				logger.Info("parquet cache not usable, using SQLite engine",
					"reason", staleness.Reason)
			} else {
				logger.Info("parquet cache not built - using SQLite engine (run 'msgvault build-cache' for faster aggregates)")
			}
			engine = query.NewEngine(s.DB(), false)
		}
	}
	defer func() { _ = engine.Close() }()

	getOAuthMgr := oauthManagerCache()

	// Create sync function for the scheduler. vf is captured and used
	// inside runScheduledSync to wire the embed enqueuer into each
	// per-run Syncer; it is nil when vector search is disabled.
	syncFunc := func(ctx context.Context, email string) error {
		return runScheduledSync(ctx, email, s, getOAuthMgr, vf)
	}

	// Create and configure scheduler
	sched := scheduler.New(syncFunc).WithLogger(logger)

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
				return runConfiguredSynctechSMSSource(ctx, source)
			},
		}); err != nil {
			logger.Error("failed to schedule synctech-sms source", "source", source.Name, "error", err)
		} else {
			logger.Info("scheduled synctech-sms source", "source", source.Name, "schedule", source.Schedule)
		}
	}

	// Register the embed job (cron-driven plus optional post-sync hook).
	// Only when vector search is enabled and wired.
	if vf != nil {
		embedJob := &scheduler.EmbedJob{
			Worker:      vf.Worker,
			Backend:     vf.Backend,
			VectorsDB:   vf.VectorsDB,
			Fingerprint: vf.Cfg.GenerationFingerprint(),
			Log:         logger,
		}
		schedule := cfg.Vector.Embed.Schedule.Cron
		if err := sched.SetEmbedJob(
			embedJob, schedule, cfg.Vector.Embed.Schedule.RunAfterSync,
		); err != nil {
			return fmt.Errorf("register embed job: %w", err)
		}
		logger.Info("embed scheduled",
			"cron", schedule,
			"run_after_sync", cfg.Vector.Embed.Schedule.RunAfterSync,
		)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start the scheduler
	sched.Start()

	// Create adapters for the API interfaces
	storeAdapter := &storeAPIAdapter{store: s}
	schedAdapter := &schedulerAdapter{scheduler: sched}

	// Create and start API server
	apiOpts := api.ServerOptions{
		Config:    cfg,
		Store:     storeAdapter,
		Engine:    engine,
		Scheduler: schedAdapter,
		Logger:    logger,
	}
	if vf != nil {
		apiOpts.HybridEngine = vf.HybridEngine
		apiOpts.Backend = vf.Backend
		apiOpts.VectorCfg = vf.Cfg
	}
	apiServer := api.NewServerWithOptions(apiOpts)

	// Start API server in goroutine
	serverErr := make(chan error, 1)
	go func() {
		if err := apiServer.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	bindAddr := cfg.Server.BindAddr
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	fmt.Printf("msgvault daemon started\n")
	fmt.Printf("  API server: http://%s\n", net.JoinHostPort(bindAddr, strconv.Itoa(cfg.Server.APIPort)))
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
	case <-ctx.Done():
		logger.Info("context cancelled")
	}

	// Graceful shutdown
	fmt.Println("Shutting down API server...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := apiServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("API server shutdown error", "error", err)
	}

	fmt.Println("Waiting for running syncs to complete...")
	schedCtx := sched.Stop()

	// Wait for scheduler to stop (with timeout)
	select {
	case <-schedCtx.Done():
		fmt.Println("Shutdown complete.")
	case <-time.After(30 * time.Second):
		fmt.Println("Shutdown timed out after 30 seconds.")
	}

	return nil
}

// storeAPIAdapter adapts store.Store to api.MessageStore.
// Since api.APIMessage, api.StoreStats, etc. are type aliases for store types,
// the adapter methods are simple pass-throughs with no conversion needed.
type storeAPIAdapter struct {
	store *store.Store
}

func (a *storeAPIAdapter) GetStats() (*api.StoreStats, error) {
	return a.store.GetStats()
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

// runScheduledSync performs a sync for a scheduled account. The
// dispatch is by source_type: Gmail accounts run an incremental sync
// using the Gmail History API; IMAP accounts run a full sync (already
// deduplicated by message-id at the store layer, since IMAP has no
// equivalent history API). When vf is non-nil (vector search enabled),
// the Syncer is configured to enqueue newly-ingested message IDs into
// the embedding pipeline so subsequent embed runs pick them up.
//
// The identifier passed in is whatever the scheduler holds — for
// Gmail this is the email address, for IMAP it's the full
// `imaps://user@host:port` URL recorded by `add-imap`.
func runScheduledSync(ctx context.Context, identifier string, s *store.Store, getOAuthMgr func(string) (*oauth.Manager, error), vf *vectorFeatures) error {
	logger.Info("starting scheduled sync", "identifier", identifier)
	startTime := time.Now()

	src, srcErr := findScheduledSyncSource(s, identifier)
	if srcErr != nil {
		return fmt.Errorf("look up source for %s: %w", identifier, srcErr)
	}

	// Source type drives dispatch. A nil source falls back to Gmail to
	// preserve the token-first workflow (tokens uploaded via API before
	// the source row exists).
	sourceType := sourceTypeGmail
	if src != nil {
		sourceType = src.SourceType
		if sourceType == "" {
			sourceType = sourceTypeGmail
		}
	}

	var (
		summary *gmail.SyncSummary
		err     error
	)
	switch sourceType {
	case sourceTypeGmail:
		summary, err = runScheduledGmailSync(ctx, identifier, src, s, getOAuthMgr, vf)
	case sourceTypeIMAP:
		summary, err = runScheduledIMAPSync(ctx, src, s, vf)
	default:
		return fmt.Errorf("source %q has type %q which is not supported by the daemon scheduler (only gmail and imap)", identifier, sourceType)
	}
	if err != nil {
		return err
	}

	logger.Info("sync completed",
		"identifier", identifier,
		"source_type", sourceType,
		"messages_added", summary.MessagesAdded,
		"duration", time.Since(startTime),
	)

	// Rebuild cache if stale (covers new messages and deletions). The
	// Parquet cache is SQLite-only; skip on PostgreSQL DSNs.
	dbPath := cfg.DatabaseDSN()
	if store.IsPostgresURL(dbPath) {
		return nil
	}
	analyticsDir := cfg.AnalyticsDir()
	if staleness := cacheNeedsBuild(dbPath, analyticsDir); staleness.NeedsBuild {
		logger.Info("rebuilding cache after sync",
			"identifier", identifier, "reason", staleness.Reason,
			"full_rebuild", staleness.FullRebuild)
		// Build in a subprocess: buildCache uses DuckDB's bundled SQLite
		// library, which corrupts the daemon's own long-lived go-sqlite3
		// connections' WAL/lock state when run in-process (issue #379).
		if err := buildCacheSubprocess(ctx, staleness.FullRebuild); err != nil {
			logger.Error("cache build failed", "error", err)
			// Don't fail the sync for cache build errors
		} else {
			logger.Info("cache build completed")
		}
	}

	return nil
}

// findScheduledSyncSource resolves the source row for a scheduler
// identifier. Returns (nil, nil) when no syncable source matches —
// callers fall back to the Gmail token-first workflow.
//
// Matches against both sources.identifier and sources.display_name so
// an IMAP account listed in config.toml as a plain email
// (`email = "user@example.com"`) resolves to the row whose identifier
// is the `imaps://...` URL but whose display_name is that email.
//
// When multiple rows match (rare; e.g. an mbox import plus a Gmail
// account with the same name), the first syncable row (gmail/imap)
// wins, with gmail taking precedence over imap.
func findScheduledSyncSource(s *store.Store, identifier string) (*store.Source, error) {
	sources, err := s.GetSourcesByIdentifierOrDisplayName(identifier)
	if err != nil {
		return nil, err
	}
	var imapSrc *store.Source
	for _, src := range sources {
		switch src.SourceType {
		case sourceTypeGmail:
			return src, nil
		case sourceTypeIMAP:
			if imapSrc == nil {
				imapSrc = src
			}
		}
	}
	return imapSrc, nil
}

// runScheduledGmailSync runs an incremental Gmail sync for the daemon.
// Token-source lookup uses oauthMgr.TokenSource directly (not
// getTokenSourceWithReauth) because serve runs as a daemon and cannot
// open a browser for OAuth — the error path tells the user how to
// re-authorize from a terminal.
func runScheduledGmailSync(ctx context.Context, email string, src *store.Source, s *store.Store, getOAuthMgr func(string) (*oauth.Manager, error), vf *vectorFeatures) (*gmail.SyncSummary, error) {
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
	if vf != nil {
		syncer.SetEmbedEnqueuer(vf.Enqueuer)
	}

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
		return nil, fmt.Errorf("incremental sync failed: %w", err)
	}
	return summary, nil
}

// runScheduledIMAPSync runs a full IMAP sync for the daemon. IMAP has
// no incremental/history API, so we always do a full pass and rely on
// the store to dedupe by message-id. NoResume is forced on because
// IMAP page tokens are numeric offsets that don't survive across
// processes (see syncfull.go).
func runScheduledIMAPSync(ctx context.Context, src *store.Source, s *store.Store, vf *vectorFeatures) (*gmail.SyncSummary, error) {
	apiClient, err := buildAPIClient(ctx, src, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("build IMAP client: %w", err)
	}
	defer func() { _ = apiClient.Close() }()

	opts := sync.DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	opts.AttachmentsDir = cfg.AttachmentsDir()
	opts.NoResume = true

	syncer := sync.New(apiClient, s, opts).WithLogger(logger)
	if vf != nil {
		syncer.SetEmbedEnqueuer(vf.Enqueuer)
	}

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
	return summary, nil
}
